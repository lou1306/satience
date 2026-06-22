// Package solver implements a CDCL SAT solver with modern techniques.
//
// This package provides a sound and complete CDCL (Conflict-Driven Clause Learning)
// SAT solver implementing:
//   - 1-UIP conflict analysis with backjumping
//   - VSIDS variable selection with activity heap
//   - LBD-based clause database management
//   - Watched literals propagation
//   - Adaptive restarts (Luby sequence + Glucose-style)
//   - Phase saving heuristic
//   - Clause minimization via self-subsumption
//   - Preprocessing and inprocessing
//
// Basic usage:
//
//	cnf := parseCNF("formula.cnf")
//	solver := solver.NewCDCLSolver(cnf)
//	result := solver.Solve()
//	if result == solver.SAT {
//	    model := solver.GetAssignments()
//	}
package solver

import (
	"fmt"
	"runtime"
	"satience/internal/cnf"
	"sort"
	"time"
)

// SolveResult represents the result of SAT solving.
type SolveResult int

const (
	// SAT indicates the formula is satisfiable.
	SAT SolveResult = iota
	// UNSAT indicates the formula is unsatisfiable.
	UNSAT
	// UNKNOWN indicates the solver couldn't determine satisfiability (timeout, etc.).
	UNKNOWN
)

// Solver configuration constants.
//
// These defaults balance performance and memory usage for typical instances.
// Tuning may be beneficial for specific instance families.
const (
	DefaultMaxLearned       = 10000  // Increased for better performance  // Maximum learned clauses before deletion
	DefaultMinLearned       = 2000  // Target clauses after deletion (20% reduction)
	DefaultRestartBase      = 100   // Base for Luby restart sequence (MiniSat-style)
	VSIDSDecayFactor        = 0.95  // VSIDS activity decay factor
	ClauseActivityDecay     = 0.95  // Clause activity decay factor
	GlueLBDThreshold        = 2     // LBD ≤ 2 considered glue clauses (protected)
	CoreGlueLBDThreshold    = 2     // LBD ≤ 2 are core glue (never delete)
	MaxClauseAge            = 500   // Age threshold for forced deletion
	LargeClauseSize         = 15    // Size threshold for forced deletion
	IterationReportInterval = 10000 // Report progress every N iterations

	// Clause minimization thresholds.
	// Set to extremely high values to enable aggressive minimization on ALL clauses.
	MinimizationMaxSize       = 10000 // Minimize clauses up to 10K literals (effectively all)
	MinimizationMaxLBD        = 10000 // Minimize clauses up to LBD 10K (effectively all)
	MinimizationMaxReasonSize = 100   // Allow reason clauses up to 100 literals (more aggressive)

	// Debugging thresholds.
	DebugConflictLimit = 100 // Verbose debug output for first N conflicts
)

// CDCLSolver implements a CDCL SAT solver with modern techniques.
//
// The solver uses:
//   - Watched literals for O(1) propagation
//   - 1-UIP conflict analysis with clause minimization
//   - VSIDS variable selection with activity heap
//   - LBD-based clause database management
//   - Adaptive restarts
//   - Phase saving
//
// Fields are mostly private; use provided methods for interaction.

// literalFreeSlot tracks a free region in the learnedLiterals pool for reuse
type literalFreeSlot struct {
	offset int // Start offset in learnedLiterals
	size   int // Number of literals in this free slot
}

type CDCLSolver struct {
	cnf                 *cnf.CNF
	assignments         []Assignment
	trail               []int
	varLevel            []int  // Cache of variable levels (avoids random assignments[].Level access)
	trailHead           []int
	level               int
	vsids              *VSIDS
	conflicts          int
	implication        []*cnf.Clause // Clause pointer (nil for decisions)
	iterations         int
	propagations       int // Total propagations (assignments by unit propagation)
	maxIter            int
	// Memory pool for learned clauses - contiguous literal storage to eliminate per-clause allocations
	learnedLiterals      []cnf.Literal // All learned clause literals in one contiguous slice
	learnedOffsets       []int         // Start offset in learnedLiterals for each clause
	learnedSizes         []int         // Number of literals in each clause (0 = deleted/tombstone)
	clauseActivity       []float64
	clauseAge            []int
	clauseSize           []int // Track clause size for deletion (mirrors learnedSizes for speed)
	clauseLBD            []int // Track LBD at time of learning
	clauseUseCount       []int // Track how often clause used in conflict analysis
	clausePropCount      []int // Track how many propagations clause caused
	normalClauseCount    int   // Track number of non-glue clauses (LBD > 3)
	learnedActiveCount   int   // Number of active clauses (excludes tombstones)
	learnedCapacity      int   // Total capacity including tombstones
	literalFreeSlots     []literalFreeSlot // Free regions in learnedLiterals for reuse
	currentAge         int
	verbose            bool
	decisions          int
	backjumpLevel      int
	maxLearned         int
	minLearned         int // Minimum clauses to keep (aggressive deletion target)
	savedPhase         []bool
	restartBase        int
	restartCount       int
	lubyIndex          int
	lbdSum             int
	lbdCount           int
	lastConflictLBD    int
	conflictsAtLevel   []int   // Track conflicts per decision level
	lastRandomDecision int     // Last conflict where we made random decision
	randomDecisionRate      float64 // Probability of making a random decision (0.0 = never, 1.0 = always)
	unitLearnedClauses map[uint32]bool // Map of variables with unit learned clauses (bit 31 = polarity)
	randomDecisionPeriod int     // Period for forced random decisions (default 0=disabled, causes O(n) overhead)
	randomSeed            uint64  // Seed for deterministic random selection
	lastDecisionVar         uint32  // Last variable chosen for decision
	consecutiveFlips        int     // Count of consecutive decisions on same variable
	// Exploration diversity tracking (IMPROVEMENT #3)
	decidedVars           []uint32 // Variables decided during current search phase
	decidedVarSet         []bool   // Fast lookup for decided variables
	restartDecisionCount  int      // Decisions since last restart (for diversity reset)
	minimizationMaxSize     int     // Skip minimization for clauses > this size (0=all)
	minimizationMaxLBD      int     // Skip minimization for clauses with LBD > this (0=all)
	minimizationMaxReasonSize int   // Skip resolution with reason clauses > this size
	// Reusable buffers for conflict analysis (avoid per-conflict allocation)
	tmpLiteralInClause  []bool
	tmpLiteralIsNegated []bool
	tmpLevelCount       []int
	tmpLevelCountUsed   []bool   // Track which levels have non-zero tmpLevelCount
	tmpCandidates       []resolveCandidate
	tmpLevelSet         []int    // For LBD calculation (replaces map)
	tmpLevelSetUsed     []bool   // Track which levels are in tmpLevelSet
	tmpResolved         []bool   // Track resolved variables in 1-UIP to prevent cycles
	tmpResolvedVars     []uint32 // Track which variables were resolved (for fast reset)
	tmpClauseHash uint64 // Hash for duplicate detection
	tmpFlippedVars []bool // Track flipped variables at level 1 to prevent infinite loops
	tmpTouchedVars []uint32 // Track which variables were modified (for fast reset)
	tmpUnassignedVars []uint32 // Reusable buffer for random variable selection (avoids allocation)
	tmpLearnedLits []cnf.Literal // Reusable buffer for learned clause literals
	tmpSortedLits []cnf.Literal // Temporary buffer for canonical clause sorting
	tmpSubsumeSet     []bool   // Reusable bitmap for subsumption checking (avoids map allocation)
	tmpSubsumeVars    []uint32 // Track variables in subsumption set for fast cleanup

	// Clause database hash table for O(1) duplicate detection
	// Stores canonical hashes (sorted literals) to detect A∨B == B∨A
	learnedClauseHashes map[uint64]bool

	// Watched literals infrastructure
	watchLists        [][]cnf.Watch // watchLists[lit] = clauses watching lit
	watchListsBinary  [][]cnf.Watch // watchListsBinary[lit] = binary clauses watching lit (optimized path)
	watchInitialized  bool          // True if watches have been initialized
	learnedClauseBase int           // Base ID for learned clause watches (fixed at initialization)
	binaryClauseCount int           // Number of binary learned clauses (for statistics)

	// LBD-based learned clause ordering for propagation prioritization
	learnedClauseOrder  []int // Indices into learnedClauses/clauseLBD sorted by LBD

	// Variable elimination tracking for model reconstruction
	eliminatedVars      []uint32                // List of eliminated variable indices
	varElimDefinition   map[uint32][]cnf.Literal // Definition of eliminated var (resolvent that eliminated it)
	varElimPolarity     map[uint32]bool         // Polarity of eliminated var in its definition
	emptyClauseFound    bool   // Set when empty learned clause derived (UNSAT)
	lbdOrderDirty       bool  // True if order needs rebuilding
	lbdOrderLastRebuild int   // Conflict count when order was last rebuilt

	qhead int // Watched literals: next trail index to process

	// Configurable parameters (exposed for tuning)
	inprocessingInterval     int     // Run inprocessing every N conflicts (default 500)
	inprocessingMaxClauses   int     // Skip inprocessing if > N clauses (default 5000)
	inprocessingTimeLimitMs  int     // Time limit for inprocessing in ms (default 200)
	preprocessingMinClauses  int     // Skip preprocessing if < N clauses (default 50)
	preprocessingMaxVars     int     // Skip preprocessing if > N vars (default 50000)
	preprocessingMaxClauses  int     // Skip preprocessing if > N clauses (default 500000)
	varElimMaxVars           int     // Skip variable elimination if > N vars (default 20000)
	varElimMaxClauses        int     // Skip variable elimination if > N clauses (default 100000)
	varElimMaxResolventSize  int     // Max resolvent size for variable elimination (default 100)
	varElimMaxOccurrences    int     // Max occurrences of variable to eliminate (default 500)
	varElimMinDeficiency     float64 // Min deficiency ratio for elimination (default 1.0 = require clause reduction)
	varElimMaxIterations     int     // Max variables to eliminate per pass (default 100)
	varElimMaxTimeMs         int     // Time limit for VE in ms (default 500)
	clauseDeletionMinLBD     int     // Minimum LBD to consider for deletion (default 3)
	glueClauseLBDThreshold   int     // LBD ≤ this are glue clauses (default 2)
	coreGlueLBDThreshold     int     // LBD ≤ this are core glue (never delete, default 2)
	largeClauseSizeThreshold int     // Size threshold for large clause detection (default 15)
	maxClauseAgeThreshold    int     // Age threshold for forced deletion (default 500)
	clauseActivityDecay      float64 // Clause activity decay factor (default 0.95)
	tmpCandidateBufferSize   int     // Buffer size for resolve candidates (default 100)
	tmpLearnedLitBufferSize  int     // Buffer size for learned literals (default 64)
	learnedClauseHashInitial int     // Initial capacity for learned clause hash table (default 2500)
	// Restart policy parameters
	restartGlucoseRatio      float64 // Glucose-style restart when LBD > ratio × avg (default 1.5)
	restartGlucoseMinConflicts int   // Min conflicts before Glucose restarts kick in (default 50)
	restartKeepGlueLBD       int     // Keep clauses with LBD ≤ this during restart (default 3)
	// Clause deletion scoring parameters
	clauseDeletionLBDWeight      float64 // LBD score weight (default 200.0)
	clauseDeletionAgeWeight      float64 // Age score weight (default 5.0)
	clauseDeletionSizeWeight     float64 // Size score weight (default 10.0)
	clauseDeletionActivityWeight float64 // Activity protection weight (default 100.0)
	clauseDeletionUseCountHigh   int     // UseCount threshold for strong protection (default 5)
	clauseDeletionUseCountLow    int     // UseCount threshold for light protection (default 0)
	clauseDeletionPropCountHigh  int     // PropCount threshold for strong protection (default 15)
	clauseDeletionPropCountLow   int     // PropCount threshold for light protection (default 0)
	clauseDeletionHighLBD1       int     // LBD threshold for force deletion bonus 1 (default 8)
	clauseDeletionHighLBD2       int     // LBD threshold for force deletion bonus 2 (default 12)
	clauseDeletionHighLBDBonus1  float64 // Force deletion bonus for LBD > highLBD1 (default 2000.0)
	clauseDeletionHighLBDBonus2  float64 // Force deletion bonus for LBD > highLBD2 (default 3000.0)
	clauseDeletionKeepRatio      float64 // Ratio of clauses to keep during deletion (default 0.50)
}

// resolveCandidate is used in learnClause for tracking resolution candidates
type resolveCandidate struct {
	varIdx     uint32
	trailPos   int // Trail position (for preserving trail order after sorting)
	reasonSize int // Size of reason clause (for optional sorting heuristics)
}

// clauseInfo is used in deleteLearnedClauses for tracking clause deletion scores
type clauseInfo struct {
	idx       int
	lbd       int
	size      int
	age       int
	activity  float64
	useCount  int
	propCount int
	score     float64 // Higher = more likely to delete
}

// clauseInfoSlice implements sort.Interface for clauseInfo slice
type clauseInfoSlice []clauseInfo

func (s clauseInfoSlice) Len() int           { return len(s) }
func (s clauseInfoSlice) Less(i, j int) bool { return s[i].score > s[j].score } // Descending: higher score = delete first
func (s clauseInfoSlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// computeCanonicalHash computes a hash for a clause with literals sorted
// This ensures A∨B and B∨A produce the same hash (duplicate detection)
func computeCanonicalHash(literals []cnf.Literal, tmpSorted []cnf.Literal) uint64 {
	// Copy literals to temporary buffer for sorting
	tmpSorted = append(tmpSorted[:0], literals...)
	
	// Sort literals for canonical representation
	// Simple insertion sort (efficient for small clauses)
	for i := 1; i < len(tmpSorted); i++ {
		key := tmpSorted[i]
		j := i - 1
		for j >= 0 && tmpSorted[j] > key {
			tmpSorted[j+1] = tmpSorted[j]
			j--
		}
		tmpSorted[j+1] = key
	}
	
	// Compute hash from sorted literals
	hash := uint64(len(tmpSorted))
	for _, lit := range tmpSorted {
		hash = hash*31 + uint64(lit)
	}
	return hash
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	maxLearned := DefaultMaxLearned
	minLearned := DefaultMinLearned
	restartBase := DefaultRestartBase

	// Ensure literal pool is built for efficient propagation
	formula.RebuildLiteralPool()

	solver := &CDCLSolver{
		cnf:                 formula,
		assignments:         make([]Assignment, formula.NumVars),
		trail:               make([]int, 0, formula.NumVars),
		varLevel:            make([]int, formula.NumVars),
		trailHead:           make([]int, 1),
		qhead:               0,
		level:               0,
		vsids:               NewVSIDS(formula.NumVars),
		conflicts:           0,
		implication:         make([]*cnf.Clause, formula.NumVars),
		iterations:          0,
		maxIter:             0,
		learnedLiterals:     make([]cnf.Literal, 0, 10000), // Pre-allocate for ~2500 clauses × avg 4 literals
		learnedOffsets:      make([]int, 0, 2500),
		learnedSizes:        make([]int, 0, 2500),
		clauseActivity:      make([]float64, 0, 2500),
		clauseAge:           make([]int, 0, 2500),
		clauseSize:          make([]int, 0, 2500),
		clauseLBD:           make([]int, 0, 2500),
		clauseUseCount:      make([]int, 0, 2500),
		clausePropCount:     make([]int, 0, 2500),
		learnedActiveCount:  0,
		learnedCapacity:     0,
		literalFreeSlots:    make([]literalFreeSlot, 0, 64),
		currentAge:          0,
		verbose:             false,
		decisions:           0,
		backjumpLevel:       0,
		maxLearned:          maxLearned,
		minLearned:          minLearned,
		savedPhase:          make([]bool, formula.NumVars),
		restartBase:         restartBase,
		restartCount:        0,
		lubyIndex:           0,
		lbdSum:              0,
		lbdCount:            0,
		randomDecisionRate:  0.0,
		randomDecisionPeriod: 0,    // Default: DISABLED (causes 65% overhead with no measurable benefit)
		randomSeed:          0,
		learnedClauseOrder:  make([]int, 0),
		lbdOrderDirty:       true,
		lbdOrderLastRebuild: 0,
		lastConflictLBD:     0,
		conflictsAtLevel:    make([]int, formula.NumVars+1),
		decidedVars:         make([]uint32, 0, formula.NumVars),
		decidedVarSet:       make([]bool, formula.NumVars),
		restartDecisionCount: 0,
		lastRandomDecision:  -1000,
		// Pre-allocate reusable buffers
		tmpLiteralInClause:  make([]bool, formula.NumVars),
		tmpLiteralIsNegated: make([]bool, formula.NumVars),
		tmpLevelCount:       make([]int, formula.NumVars+1),
		tmpLevelCountUsed:   make([]bool, formula.NumVars+1),
		tmpCandidates:       make([]resolveCandidate, 0, 100),
		tmpLevelSet:         make([]int, 0, formula.NumVars),
		tmpLevelSetUsed:     make([]bool, formula.NumVars+1),
		tmpResolved:         make([]bool, formula.NumVars),
		tmpResolvedVars:     make([]uint32, 0, formula.NumVars),
		tmpFlippedVars:      make([]bool, formula.NumVars),
		tmpTouchedVars:      make([]uint32, 0, formula.NumVars),
		tmpUnassignedVars:   make([]uint32, 0, formula.NumVars),
		tmpLearnedLits:      make([]cnf.Literal, 0, 64),
		tmpSortedLits:       make([]cnf.Literal, 0, 64),
		tmpSubsumeSet:       make([]bool, formula.NumVars),
		tmpSubsumeVars:      make([]uint32, 0, 64),
		learnedClauseHashes: make(map[uint64]bool, 2500),
		learnedClauseBase:   int(formula.NumClauses),
		unitLearnedClauses:  make(map[uint32]bool),
		// Minimization thresholds
		minimizationMaxSize:       30,
		minimizationMaxLBD:        8,
		minimizationMaxReasonSize: 15,
		// Variable elimination tracking
		eliminatedVars:    make([]uint32, 0),
		varElimDefinition: make(map[uint32][]cnf.Literal),
		varElimPolarity:   make(map[uint32]bool),
		// Configurable parameters with defaults
		inprocessingInterval:     500,
		inprocessingMaxClauses:   5000,
		inprocessingTimeLimitMs:  200,
		preprocessingMinClauses:  10,  // Lowered to enable inprocessing after aggressive VE
		preprocessingMaxVars:     50000,
		preprocessingMaxClauses:  500000,
		varElimMaxVars:           20000,
		varElimMaxClauses:        100000,
		varElimMaxResolventSize:  50,     // Reduced from 100 (more conservative)
		varElimMaxOccurrences:    200,    // Reduced from 500 (only eliminate low-occurrence vars)
		varElimMinDeficiency:     0.0,    // Allow elimination even without clause reduction (MiniSat-style)
		varElimMaxIterations:     100,    // Increased from 50 for better PHP performance
		varElimMaxTimeMs:         500,    // Increased from 200ms for more thorough elimination
		clauseDeletionMinLBD:     3,
		glueClauseLBDThreshold:   2,
		coreGlueLBDThreshold:     2,
		largeClauseSizeThreshold: 15,
		maxClauseAgeThreshold:    500,
		clauseActivityDecay:      0.95,
		tmpCandidateBufferSize:   100,
		tmpLearnedLitBufferSize:  64,
		learnedClauseHashInitial: 2500,
		// Restart policy defaults (aggressive for better performance on random instances)
		restartGlucoseRatio:      3.0,     // Less aggressive (MiniSat-style)
		restartGlucoseMinConflicts: 100,   // Wait for more conflicts
		restartKeepGlueLBD:       3,
		// Clause deletion scoring defaults (LBD-primary, age/size secondary)
		clauseDeletionLBDWeight:      200.0,
		clauseDeletionAgeWeight:      5.0,
		clauseDeletionSizeWeight:     10.0,
		clauseDeletionActivityWeight: 100.0,
		clauseDeletionUseCountHigh:   5,
		clauseDeletionPropCountHigh:  15,
		clauseDeletionHighLBD1:       8,
		clauseDeletionHighLBD2:       12,
		clauseDeletionHighLBDBonus1:  2000.0,
		clauseDeletionHighLBDBonus2:  3000.0,
		clauseDeletionKeepRatio:      0.50,
	}

	// Enable LBD-based VSIDS for better variable selection
	// Variables in low-LBD clauses get higher priority
	solver.vsids.EnableLBD()

	return solver
}

// SetMaxIter sets the maximum iteration limit (0 = unlimited)
func (s *CDCLSolver) SetMaxIter(limit int) {
	s.maxIter = limit
}

// SetVerbose enables/disables verbose output
func (s *CDCLSolver) SetVerbose(v bool) {
	s.verbose = v
}

// SetRandomDecisionRate sets the probability of making a random decision
// rate should be in [0.0, 1.0] where 0.0 = never random, 1.0 = always random
// Recommended: 0.01-0.05 for most instances
func (s *CDCLSolver) SetRandomDecisionRate(rate float64) {
	if rate < 0.0 {
		rate = 0.0
	}
	if rate > 1.0 {
		rate = 1.0
	}
	s.randomDecisionRate = rate
}

// SetRandomDecisionPeriod sets the frequency of forced random decisions
// period is the number of conflicts between random decisions (0 = disabled)
// WARNING: Random decisions cause O(n) variable scan overhead
// Profiling shows 65% overhead with no measurable benefit on standard benchmarks
// Only enable for specific instance families that benefit from diversification
func (s *CDCLSolver) SetRandomDecisionPeriod(period int) {
	if period < 0 {
		period = 0
	}
	s.randomDecisionPeriod = period
}

// SetRandomSeed sets the seed for deterministic random selection
// Default seed is 0
func (s *CDCLSolver) SetRandomSeed(seed uint64) {
	s.randomSeed = seed
	s.vsids.SetRandomSeed(seed)
}

// SetInprocessingInterval sets how often to run inprocessing (default 500 conflicts)
func (s *CDCLSolver) SetInprocessingInterval(interval int) {
	if interval < 100 {
		interval = 100
	}
	s.inprocessingInterval = interval
}

// SetInprocessingMaxClauses sets the maximum clauses for inprocessing (default 5000)
func (s *CDCLSolver) SetInprocessingMaxClauses(max int) {
	if max < 100 {
		max = 100
	}
	s.inprocessingMaxClauses = max
}

// SetInprocessingTimeLimitMs sets the time limit for inprocessing in milliseconds (default 200)
func (s *CDCLSolver) SetInprocessingTimeLimitMs(ms int) {
	if ms < 50 {
		ms = 50
	}
	s.inprocessingTimeLimitMs = ms
}

// SetPreprocessingThresholds sets the preprocessing size thresholds
// minClauses: skip if < N clauses (default 50)
// maxVars: skip if > N vars (default 50000)
// maxClauses: skip if > N clauses (default 500000)
func (s *CDCLSolver) SetPreprocessingThresholds(minClauses, maxVars, maxClauses int) {
	if minClauses < 0 {
		minClauses = 0
	}
	if maxVars < 0 {
		maxVars = 0
	}
	if maxClauses < 0 {
		maxClauses = 0
	}
	s.preprocessingMinClauses = minClauses
	s.preprocessingMaxVars = maxVars
	s.preprocessingMaxClauses = maxClauses
}

// SetVariableEliminationThresholds sets the variable elimination thresholds
// maxVars: skip if > N vars (default 20000)
// maxClauses: skip if > N clauses (default 100000)
// maxResolventSize: max resolvent size (default 100)
// maxOccurrences: max occurrences of variable to eliminate (default 500)
// minDeficiency: min deficiency ratio (default 0.0 = disabled)
func (s *CDCLSolver) SetVariableEliminationThresholds(maxVars, maxClauses, maxResolventSize int, maxOccurrences int, minDeficiency float64) {
	if maxVars < 0 {
		maxVars = 0
	}
	if maxClauses < 0 {
		maxClauses = 0
	}
	if maxResolventSize < 0 {
		maxResolventSize = 0
	}
	if maxOccurrences < 0 {
		maxOccurrences = 0
	}
	if minDeficiency < 0.0 {
		minDeficiency = 0.0
	}
	s.varElimMaxVars = maxVars
	s.varElimMaxClauses = maxClauses
	s.varElimMaxResolventSize = maxResolventSize
	s.varElimMaxOccurrences = maxOccurrences
	s.varElimMinDeficiency = minDeficiency
}

// SetClauseDeletionLBDThresholds sets the LBD thresholds for clause deletion
// minLBD: minimum LBD to consider for deletion (default 3)
// glueLBD: LBD ≤ this are glue clauses (default 2)
// coreGlueLBD: LBD ≤ this are core glue, never delete (default 2)
func (s *CDCLSolver) SetClauseDeletionLBDThresholds(minLBD, glueLBD, coreGlueLBD int) {
	if minLBD < 0 {
		minLBD = 0
	}
	if glueLBD < 0 {
		glueLBD = 0
	}
	if coreGlueLBD < 0 {
		coreGlueLBD = 0
	}
	s.clauseDeletionMinLBD = minLBD
	s.glueClauseLBDThreshold = glueLBD
	s.coreGlueLBDThreshold = coreGlueLBD
}

// SetClauseDeletionThresholds sets the size and age thresholds for clause deletion
// largeSize: size threshold for large clause detection (default 15)
// maxAge: age threshold for forced deletion (default 500)
func (s *CDCLSolver) SetClauseDeletionThresholds(largeSize, maxAge int) {
	if largeSize < 0 {
		largeSize = 0
	}
	if maxAge < 0 {
		maxAge = 0
	}
	s.largeClauseSizeThreshold = largeSize
	s.maxClauseAgeThreshold = maxAge
}

// SetClauseActivityDecay sets the clause activity decay factor (default 0.95)
func (s *CDCLSolver) SetClauseActivityDecay(decay float64) {
	if decay < 0.5 || decay > 1.0 {
		decay = 0.95
	}
	s.clauseActivityDecay = decay
}

// SetTemporaryBufferSizes sets the buffer sizes for temporary allocations
// candidateBuf: buffer size for resolve candidates (default 100)
// learnedLitBuf: buffer size for learned literals (default 64)
// hashInitial: initial capacity for learned clause hash table (default 2500)
func (s *CDCLSolver) SetTemporaryBufferSizes(candidateBuf, learnedLitBuf, hashInitial int) {
	if candidateBuf < 1 {
		candidateBuf = 1
	}
	if learnedLitBuf < 1 {
		learnedLitBuf = 1
	}
	if hashInitial < 1 {
		hashInitial = 1
	}
	s.tmpCandidateBufferSize = candidateBuf
	s.tmpLearnedLitBufferSize = learnedLitBuf
	s.learnedClauseHashInitial = hashInitial
}

// SetDecayInterval sets the VSIDS decay interval (conflicts between activity decays)
// Higher values = fewer heap rebuilds but slower activity differentiation
// Lower values = more frequent decay but more heap rebuilds
// Default is 10, which provides good balance for most instances
func (s *CDCLSolver) SetDecayInterval(interval int) {
	s.vsids.SetDecayInterval(interval)
}

// EnableLRB enables LRB (Learning Rate Based) heuristic
func (s *CDCLSolver) EnableLRB() {
	s.vsids.EnableLRB()
}

// EnableCHB enables CHB (Conflict History Based) heuristic
// CHB tracks recent conflict frequency with aggressive decay instead of cumulative VSIDS activity
func (s *CDCLSolver) EnableCHB() {
	s.vsids.EnableCHB()
}

// SetCHBParameters configures CHB heuristic parameters
// decayFactor: decay factor for conflict frequency (default 0.75, range 0.5-0.95)
// decayInterval: decay every N conflicts (default 50)
func (s *CDCLSolver) SetCHBParameters(decayFactor float64, decayInterval int) {
	s.vsids.SetCHBDecayFactor(decayFactor)
	s.vsids.SetCHBDecayInterval(decayInterval)
}

// SetRestartParameters configures restart policy parameters
// base: Luby sequence base multiplier (default 20, range 1-1000)
// glucoseRatio: Glucose restart when LBD > ratio × avg (default 1.5, range 1.0-5.0)
// minConflicts: min conflicts before Glucose restarts activate (default 50)
// keepGlueLBD: keep clauses with LBD ≤ this during restart (default 3)
func (s *CDCLSolver) SetRestartParameters(base int, glucoseRatio float64, minConflicts, keepGlueLBD int) {
	if base < 1 {
		base = 1
	}
	if base > 1000 {
		base = 1000
	}
	s.restartBase = base
	
	if glucoseRatio < 1.0 {
		glucoseRatio = 1.0
	}
	if glucoseRatio > 5.0 {
		glucoseRatio = 5.0
	}
	s.restartGlucoseRatio = glucoseRatio
	
	if minConflicts < 0 {
		minConflicts = 0
	}
	s.restartGlucoseMinConflicts = minConflicts
	
	if keepGlueLBD < 2 {
		keepGlueLBD = 2
	}
	s.restartKeepGlueLBD = keepGlueLBD
}

// SetClauseDeletionParameters configures clause deletion scoring parameters
// lbdWeight: LBD score weight (default 200.0)
// ageWeight: Age score weight (default 5.0)
// sizeWeight: Size score weight (default 10.0)
// activityWeight: Activity protection weight (default 100.0)
// keepRatio: Ratio of clauses to keep during deletion (default 0.50)
func (s *CDCLSolver) SetClauseDeletionParameters(
	lbdWeight, ageWeight, sizeWeight, activityWeight, keepRatio float64,
) {
	if lbdWeight < 0 {
		lbdWeight = 0
	}
	if ageWeight < 0 {
		ageWeight = 0
	}
	if sizeWeight < 0 {
		sizeWeight = 0
	}
	if activityWeight < 0 {
		activityWeight = 0
	}
	if keepRatio < 0.1 || keepRatio > 0.9 {
		keepRatio = 0.5
	}
	
	s.clauseDeletionLBDWeight = lbdWeight
	s.clauseDeletionAgeWeight = ageWeight
	s.clauseDeletionSizeWeight = sizeWeight
	s.clauseDeletionActivityWeight = activityWeight
	s.clauseDeletionKeepRatio = keepRatio
}

// SetMinimizationThresholds configures clause minimization behavior
// size: skip minimization for clauses larger than this (0=all clauses)
// lbd: skip minimization for clauses with LBD larger than this (0=all clauses)
// reasonSize: skip resolution with reason clauses larger than this
func (s *CDCLSolver) SetMinimizationThresholds(size, lbd, reasonSize int) {
	s.minimizationMaxSize = size
	s.minimizationMaxLBD = lbd
	s.minimizationMaxReasonSize = reasonSize
}

// GetStats returns solving statistics
// SolverStats holds detailed solving statistics
type SolverStats struct {
	Conflicts      int
	Decisions      int
	Iterations     int
	LearnedClauses int
	MaxLevel       int
	AvgClauseSize  int
	AvgLBD         int
	MinClauseSize  int
	MaxClauseSize  int
}

func (s *CDCLSolver) GetStats() map[string]int {
	stats := s.getDetailedStats()
	return map[string]int{
		"conflicts":  stats.Conflicts,
		"decisions":  stats.Decisions,
		"iterations": stats.Iterations,
		"learned":    stats.LearnedClauses,
		"level":      stats.MaxLevel,
	}
}

// GetConflicts returns the number of conflicts
func (s *CDCLSolver) GetConflicts() int {
	return s.conflicts
}

// GetDecisions returns the number of decisions
func (s *CDCLSolver) GetDecisions() int {
	return s.decisions
}

// GetIterations returns the number of iterations
func (s *CDCLSolver) GetIterations() int {
	return s.iterations
}

// GetLearnedCount returns the number of learned clauses
func (s *CDCLSolver) GetLearnedCount() int {
	return s.learnedActiveCount
}

// GetMemoryPoolStats returns memory pool statistics
func (s *CDCLSolver) GetMemoryPoolStats() (activeClauses, poolLiterals, poolMemoryKB int) {
	activeClauses = s.learnedActiveCount
	poolLiterals = len(s.learnedLiterals)
	// Approximate memory: literals (4 bytes each) + offsets/sizes/metadata (8 bytes each × 7 arrays × capacity)
	cap := cap(s.learnedOffsets)
	poolMemoryKB = (len(s.learnedLiterals)*4 + cap*8*7) / 1024
	return activeClauses, poolLiterals, poolMemoryKB
}

// getLearnedClauseLiterals returns the literals for a learned clause (view into contiguous pool)
func (s *CDCLSolver) getLearnedClauseLiterals(clauseIdx int) []cnf.Literal {
	offset := s.learnedOffsets[clauseIdx]
	size := s.learnedSizes[clauseIdx]
	return s.learnedLiterals[offset : offset+size]
}

func (s *CDCLSolver) getDetailedStats() SolverStats {
	stats := SolverStats{
		Conflicts:      s.conflicts,
		Decisions:      s.decisions,
		Iterations:     s.iterations,
		LearnedClauses: s.learnedActiveCount,
		MaxLevel:       s.level,
	}

	// Calculate clause size statistics
	if s.learnedActiveCount > 0 {
		minSize := s.learnedSizes[0]
		maxSize := minSize
		totalSize := 0

		for i := range s.learnedSizes {
			size := s.learnedSizes[i]
			totalSize += size
			if size < minSize {
				minSize = size
			}
			if size > maxSize {
				maxSize = size
			}
		}

		stats.AvgClauseSize = totalSize / s.learnedActiveCount
		stats.MinClauseSize = minSize
		stats.MaxClauseSize = maxSize
	}

	// Calculate average LBD
	if s.lbdCount > 0 {
		stats.AvgLBD = s.lbdSum / s.lbdCount
	}

	return stats
}

func (s *CDCLSolver) printStats() {
	stats := s.getDetailedStats()

	fmt.Printf("c \n")
	fmt.Printf("c === Solving Statistics ===\n")
	fmt.Printf("c Variables:     %d\n", s.cnf.NumVars)
	fmt.Printf("c Clauses:       %d\n", s.cnf.NumClauses)
	fmt.Printf("c Conflicts:     %d\n", stats.Conflicts)
	fmt.Printf("c Decisions:     %d\n", stats.Decisions)
	fmt.Printf("c Iterations:    %d\n", stats.Iterations)
	fmt.Printf("c Learned:       %d\n", stats.LearnedClauses)
	fmt.Printf("c Max Level:     %d\n", stats.MaxLevel)
	if stats.AvgClauseSize > 0 {
		fmt.Printf("c Avg Clause:  %d lits (min=%d, max=%d)\n",
			stats.AvgClauseSize, stats.MinClauseSize, stats.MaxClauseSize)
	}
	if stats.AvgLBD > 0 {
		fmt.Printf("c Avg LBD:       %d\n", stats.AvgLBD)
	}
	fmt.Printf("c \n")
}

// PreprocessingConfig controls which preprocessing techniques are enabled
// Used for debugging soundness issues
type PreprocessingConfig struct {
	EnableUnitProp        bool
	EnableEquivalence     bool
	EnablePureLiteral     bool
	EnableSubsumption     bool
	EnableSelfSubsumption bool
	EnableHyperBinary     bool
}

// DefaultPreprocessingConfig returns the default (all enabled) configuration
func DefaultPreprocessingConfig() PreprocessingConfig {
	return PreprocessingConfig{
		EnableUnitProp:        true,
		EnableEquivalence:     false, // DISABLED: Soundness bug - false equivalences
		EnablePureLiteral:     true,  // ENABLED: Pure literal elimination is sound
		EnableSubsumption:     true,  // FIXED: Subsumption elimination is now sound
		EnableSelfSubsumption: false, // DISABLED: Soundness bug - incorrect clause removal
		EnableHyperBinary:     false, // DISABLED: Soundness bug - derives false empty clauses
	}
}

// preprocessConfig controls which techniques are enabled (for debugging)
var preprocessConfig = DefaultPreprocessingConfig()

// SetPreprocessingConfig sets the global preprocessing configuration
// Used for debugging to enable/disable individual techniques
func SetPreprocessingConfig(config PreprocessingConfig) {
	preprocessConfig = config
}

// GetPreprocessingConfig returns the current preprocessing configuration
func GetPreprocessingConfig() PreprocessingConfig {
	return preprocessConfig
}

func (s *CDCLSolver) preprocessAggressive() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Aggressive preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}

	// Skip on very small instances - overhead outweighs benefits
	if s.cnf.NumClauses < s.preprocessingMinClauses {
		if s.verbose {
			fmt.Printf("c [verbose] Skipping preprocessing: instance too small (%d clauses)\n", s.cnf.NumClauses)
		}
		s.cnf.RebuildLiteralPool()
		s.initWatches()
		return UNKNOWN
	}

	// Skip on VERY large instances - preprocessing too slow
	if int(s.cnf.NumVars) > s.preprocessingMaxVars || s.cnf.NumClauses > s.preprocessingMaxClauses {
		if s.verbose {
			fmt.Printf("c [verbose] Skipping preprocessing: instance too large (%d vars, %d clauses)\n",
				s.cnf.NumVars, s.cnf.NumClauses)
		}
		s.cnf.RebuildLiteralPool()
		s.initWatches()
		return UNKNOWN
	}
	
	// For large instances, use lightweight preprocessing
	isLargeInstance := s.cnf.NumVars > 10000 || s.cnf.NumClauses > 50000
	if isLargeInstance && s.verbose {
		fmt.Printf("c [verbose] Running lightweight preprocessing on large instance (%d vars, %d clauses)\n",
			s.cnf.NumVars, s.cnf.NumClauses)
	}

	initialClauses := s.cnf.NumClauses

	// For large instances, use fewer passes and skip expensive techniques
	maxPasses := 3
	if isLargeInstance {
		maxPasses = 2
	}
	if s.cnf.NumVars < 50 {
		maxPasses = 10 // More passes for small instances to eliminate variable chains
	}

	// Increase to 5 passes for more thorough preprocessing
	// Modern solvers (CaDiCaL) use 10+ passes
	// Safeguards: time limits in each technique prevent explosion
	for pass := 0; pass < maxPasses; pass++ {
		if s.verbose {
			fmt.Printf("c [verbose] Preprocessing pass %d/%d: %d clauses\n", pass+1, maxPasses, s.cnf.NumClauses)
		}

		// Run unit propagation first to catch any existing units
		if preprocessConfig.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Equivalence detection: find a↔b patterns and substitute
		// Skip on very large instances (>20K vars) - O(n^2) complexity
		if preprocessConfig.EnableEquivalence && !isLargeInstance {
			if equivResult := s.equivalenceDetection(); equivResult != UNKNOWN {
				return equivResult
			}
		}

		// Variable elimination - SOUND FIX: Only eliminate variables with pos=1
		// This guarantees correct model reconstruction: x = ¬A where (x ∨ A) is the positive clause
		// Multi-positive-clause elimination causes incorrect definitions and unsound models
		clauseDensity := float64(s.cnf.NumClauses) / float64(s.cnf.NumVars)
		if !isLargeInstance && s.cnf.NumVars < 500 && s.cnf.NumClauses < 5000 && clauseDensity > 2.0 {
			if s.verbose {
				fmt.Printf("c [verbose] Running VE: clause density %.1f > 2.0 threshold\n", clauseDensity)
			}
			veResult := s.variableElimination()
			if veResult == UNSAT {
				return UNSAT
			}
		}

		// Run unit propagation to catch new units from equivalence substitution
		if preprocessConfig.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Pure literal elimination - cheap, run on all instances
		if preprocessConfig.EnablePureLiteral {
			if pureResult := s.pureLiteralElimination(); pureResult != UNKNOWN {
				return pureResult
			}
		}

		// Subsumption elimination - skip on medium/large instances (O(n^2))
		// Threshold lowered from 50K to 1K clauses - subsumption overhead dominates on typical benchmarks
		// Always run subsumption for small instances after VE to clean up resolvents
		if preprocessConfig.EnableSubsumption && !isLargeInstance && (s.cnf.NumVars < 50 || s.cnf.NumClauses < 1000) {
			s.subsumptionElimination()
		}

		// Self-subsumption - skip on large instances (expensive)
		if preprocessConfig.EnableSelfSubsumption && !isLargeInstance {
			s.selfSubsumption()
		}

		// Hyper-binary resolution - skip on large instances (expensive)
		if preprocessConfig.EnableHyperBinary && !isLargeInstance {
			s.hyperBinaryResolution()
		}

		// Run unit propagation after hyper-binary resolution
		if preprocessConfig.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Stop if no progress made for 2 consecutive passes
		if s.cnf.NumClauses == initialClauses && pass >= 1 {
			break
		}
		initialClauses = s.cnf.NumClauses
	}

	if s.verbose {
		fmt.Printf("c [verbose] After preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}

	// Clear assignments made during preprocessing passes
	// EXCEPT for eliminated variables - keep them assigned so VSIDS doesn't select them
	for i := range s.assignments {
		// Keep eliminated variables assigned
		isEliminated := false
		for _, elimVar := range s.eliminatedVars {
			if elimVar == uint32(i) {
				isEliminated = true
				break
			}
		}
		if !isEliminated {
			s.assignments[i] = Assignment{}
			s.varLevel[i] = 0
		}
	}
	s.trail = s.trail[:0]
	s.trailHead = []int{0}
	s.level = 0
	s.qhead = 0
	for i := range s.implication {
		s.implication[i] = nil
	}

	// CRITICAL: Run unit propagation to handle unit clauses before search
	// Unit clauses are not watched by watched literals, so they must be propagated
	// before search starts. This is especially important when preprocessing techniques
	// are disabled or don't run unit propagation.
	if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
		return unitResult
	}

	// Rebuild literal pool after preprocessing (even if no clauses removed)
	s.cnf.RebuildLiteralPool()

	// Initialize watches after unit propagation
	// CRITICAL: Reset watchInitialized flag so watches are re-initialized
	s.watchInitialized = false
	s.initWatches()

	return UNKNOWN
}

// initWatches initializes watched literals for all clauses
// Called after preprocessing completes (preprocessing modifies clauses)
func (s *CDCLSolver) initWatches() {
	if s.watchInitialized {
		return
	}

	numLits := int(s.cnf.NumVars) * 2
	s.watchLists = make([][]cnf.Watch, numLits)
	
	// BINARY CLAUSE OPTIMIZATION: Separate watch lists for binary clauses
	// Binary clauses use optimized propagation path (simpler logic, no inner loop)
	s.watchListsBinary = make([][]cnf.Watch, numLits)

	// Pre-allocate watch lists with estimated capacity to avoid reallocations
	// Account for both original clauses AND expected learned clauses
	// Each clause adds 2 watches (one per watched literal)
	// Formula: (originalClauses + maxLearned) * 2 watches / num literals
	// Minimum 32 to handle uneven distribution (some literals appear in many clauses)
	totalClauses := s.cnf.NumClauses + s.maxLearned
	avgWatchesPerLit := (totalClauses * 2) / numLits
	if avgWatchesPerLit < 32 {
		avgWatchesPerLit = 32
	}
	// Cap at 256 to avoid over-allocation on small instances
	if avgWatchesPerLit > 256 {
		avgWatchesPerLit = 256
	}
	for i := range s.watchLists {
		s.watchLists[i] = make([]cnf.Watch, 0, avgWatchesPerLit)
		// Binary watch lists: estimate 40% of clauses are binary
		s.watchListsBinary[i] = make([]cnf.Watch, 0, avgWatchesPerLit*2/5)
	}

	for clauseID := 0; clauseID < s.cnf.NumClauses; clauseID++ {
		clause := &s.cnf.Clauses[clauseID]
		s.addOriginalClauseToWatches(clauseID, clause, clause.Literals)
	}

	// learnedClauseBase is already set in NewCDCLSolver to the original NumClauses value

	for learnedIdx := 0; learnedIdx < s.learnedCapacity; learnedIdx++ {
		if s.learnedSizes[learnedIdx] == 0 {
			continue // Skip tombstones
		}
		literals := s.getLearnedClauseLiterals(learnedIdx)
		// Create a temporary Clause struct for addLearnedClauseToWatches
		tmpClause := &cnf.Clause{Literals: literals, Learned: true}
		s.addLearnedClauseToWatches(learnedIdx, tmpClause, literals)
	}

	// CRITICAL: Set watchInitialized AFTER all clauses are watched
	// This flag controls whether propagateWatched() is used instead of linear propagation
	s.watchInitialized = true

	if s.verbose {
		totalWatches := 0
		for _, wl := range s.watchLists {
			totalWatches += len(wl)
		}
		totalBinaryWatches := 0
		for _, wl := range s.watchListsBinary {
			totalBinaryWatches += len(wl)
		}
		avgWatches := float64(totalWatches) / float64(numLits)
		avgBinaryWatches := float64(totalBinaryWatches) / float64(numLits)
		fmt.Printf("c [verbose] Watched literals enabled: %d watch lists, %d total watches, %.1f avg per lit\n", 
			len(s.watchLists), totalWatches, avgWatches)
		fmt.Printf("c [verbose] Binary clause watches: %d watches, %.1f avg per lit, %d binary clauses\n",
			totalBinaryWatches, avgBinaryWatches, s.binaryClauseCount)
	}
}

// addOriginalClauseToWatches adds an original clause to the watch lists
// Watches the first two literals in the clause
func (s *CDCLSolver) addOriginalClauseToWatches(clauseIdx int, clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	lit0 := literals[0]
	lit1 := literals[1]

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Add watches (symmetric watch tracking via ClauseIdx scanning)
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		Clause:    clause,
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx1),
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		Clause:    clause,
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx0),
	})
}

// addLearnedClauseToWatches adds a learned clause to the watch lists
// Watches the first two literals in the clause
// BINARY CLAUSE OPTIMIZATION: Binary clauses use separate watch lists for optimized propagation
func (s *CDCLSolver) addLearnedClauseToWatches(learnedIdx int, clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	lit0 := literals[0]
	lit1 := literals[1]

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Learned clause index is stored as negative: -learnedIdx-1
	clauseIdx := -learnedIdx - 1
	
	// BINARY CLAUSE OPTIMIZATION: Route binary clauses to specialized watch lists
	isBinary := len(literals) == 2
	watchLists := s.watchLists
	if isBinary {
		watchLists = s.watchListsBinary
		s.binaryClauseCount++
	}

	// Add watches (symmetric watch tracking via ClauseIdx scanning)
	watchLists[idx0] = append(watchLists[idx0], cnf.Watch{
		Clause:    clause,
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx1),
	})

	watchLists[idx1] = append(watchLists[idx1], cnf.Watch{
		Clause:    clause,
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx0),
	})
}

// removeLearnedClauseWatches removes all watches for a deleted learned clause
// P1 OPTIMIZATION: Remove watches immediately when clause is deleted (not during propagation)
func (s *CDCLSolver) removeLearnedClauseWatches(learnedIdx int) {
	if learnedIdx < 0 || learnedIdx >= s.learnedCapacity {
		return
	}
	
	// Get clause literals to find watched literals
	if s.learnedSizes[learnedIdx] < 2 {
		return // Clause too short to have watches
	}
	
	literals := s.getLearnedClauseLiterals(learnedIdx)
	if len(literals) < 2 {
		return
	}
	
	lit0 := literals[0]
	lit1 := literals[1]
	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)
	clauseIdx := -learnedIdx - 1
	
	// BINARY CLAUSE OPTIMIZATION: Check which watch list the clause is in
	isBinary := len(literals) == 2
	watchLists := s.watchLists
	if isBinary {
		watchLists = s.watchListsBinary
		if s.binaryClauseCount > 0 {
			s.binaryClauseCount--
		}
	}
	
	// Remove watch from lit0's watch list
	watchList0 := watchLists[idx0]
	for i := range watchList0 {
		if watchList0[i].ClauseIdx == clauseIdx {
			// Remove this watch by swapping with last
			lastIdx := len(watchList0) - 1
			if i != lastIdx {
				watchList0[i] = watchList0[lastIdx]
				// Update symmetric watch Blit
				movedWatch := watchList0[i]
				for symI := range watchLists[movedWatch.Blit] {
					if watchLists[movedWatch.Blit][symI].ClauseIdx == movedWatch.ClauseIdx {
						watchLists[movedWatch.Blit][symI].Blit = uint32(idx0)
						break
					}
				}
			}
			watchList0 = watchList0[:lastIdx]
			break
		}
	}
	watchLists[idx0] = watchList0
	
	// Remove watch from lit1's watch list
	watchList1 := watchLists[idx1]
	for i := range watchList1 {
		if watchList1[i].ClauseIdx == clauseIdx {
			// Remove this watch by swapping with last
			lastIdx := len(watchList1) - 1
			if i != lastIdx {
				watchList1[i] = watchList1[lastIdx]
				// Update symmetric watch Blit
				movedWatch := watchList1[i]
				for symI := range watchLists[movedWatch.Blit] {
					if watchLists[movedWatch.Blit][symI].ClauseIdx == movedWatch.ClauseIdx {
						watchLists[movedWatch.Blit][symI].Blit = uint32(idx1)
						break
					}
				}
			}
			watchList1 = watchList1[:lastIdx]
			break
		}
	}
	watchLists[idx1] = watchList1
}

func (s *CDCLSolver) selfSubsumption() {
	changed := true
	for changed {
		changed = false
		for i := 0; i < len(s.cnf.Clauses); i++ {
			for j := 0; j < len(s.cnf.Clauses); j++ {
				if i == j {
					continue
				}

				clauseA := s.cnf.Clauses[i]
				clauseB := s.cnf.Clauses[j]

				if len(clauseA.Literals) != 2 || len(clauseB.Literals) < 2 {
					continue
				}

				for _, litA := range clauseA.Literals {
					for _, litB := range clauseB.Literals {
						if litA.Var() == litB.Var() && litA.IsNegated() != litB.IsNegated() {
							resolvent := s.resolveOnVar(clauseA, clauseB, litA.Var())
							if resolvent != nil && s.subsumes(resolvent, &s.cnf.Clauses[j]) {
								s.cnf.Clauses[j] = *resolvent
								changed = true
								if s.verbose {
									fmt.Printf("c [verbose] Self-subsumption: strengthened clause\n")
								}
							}
							goto nextPair
						}
					}
				}
			nextPair:
			}
		}
	}
}

func (s *CDCLSolver) hyperBinaryResolution() {
	binaryUnits := make(map[uint32]bool)

	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) == 2 {
			lit1, lit2 := clause.Literals[0], clause.Literals[1]
			if s.isUnitLiteral(lit1) {
				binaryUnits[lit1.Var()] = !lit1.IsNegated()
			}
			if s.isUnitLiteral(lit2) {
				binaryUnits[lit2.Var()] = !lit2.IsNegated()
			}
		}
	}

	if len(binaryUnits) == 0 {
		return
	}

	for i := 0; i < len(s.cnf.Clauses); i++ {
		clause := s.cnf.Clauses[i]
		if len(clause.Literals) < 3 {
			continue
		}

		newLiterals := make([]cnf.Literal, 0)
		for _, lit := range clause.Literals {
			if assigned, exists := binaryUnits[lit.Var()]; exists {
				litTrue := !lit.IsNegated()
				if litTrue == assigned {
					goto satisfied
				}
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}

		if len(newLiterals) == 0 {
			if s.verbose {
				fmt.Printf("c [verbose] Hyper-binary: empty clause\n")
			}
			return
		}

		s.cnf.Clauses[i] = cnf.Clause{Literals: newLiterals, Learned: false}
	satisfied:
	}

	newClauses := make([]cnf.Clause, 0)
	for _, clause := range s.cnf.Clauses {
		satisfied := false
		for _, lit := range clause.Literals {
			if assigned, exists := binaryUnits[lit.Var()]; exists {
				litTrue := !lit.IsNegated()
				if litTrue == assigned {
					satisfied = true
					break
				}
			}
		}
		if !satisfied {
			newClauses = append(newClauses, clause)
		}
	}
	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)
}

func (s *CDCLSolver) subsumptionElimination() {
	if len(s.cnf.Clauses) == 0 {
		return
	}

	// Mark clauses for removal to avoid iteration issues with swap-delete
	toRemove := make([]bool, len(s.cnf.Clauses))
	removed := 0

	for i := 0; i < len(s.cnf.Clauses); i++ {
		if toRemove[i] {
			continue
		}
		for j := 0; j < len(s.cnf.Clauses); j++ {
			if i == j || toRemove[j] {
				continue
			}

			if s.subsumes(&s.cnf.Clauses[i], &s.cnf.Clauses[j]) {
				toRemove[j] = true
				removed++
			}
		}
	}

	// Build new clause list without removed clauses
	if removed > 0 {
		newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses)-removed)
		for i := 0; i < len(s.cnf.Clauses); i++ {
			if !toRemove[i] {
				newClauses = append(newClauses, s.cnf.Clauses[i])
			}
		}
		s.cnf.Clauses = newClauses
		s.cnf.NumClauses = len(newClauses)
	}

	if s.verbose {
		fmt.Printf("c [verbose] Subsumption elimination: removed %d clauses\n", removed)
	}
}

func (s *CDCLSolver) subsumeLearnedClauses(newClause *cnf.Clause) {
	// Mark learned clauses that are subsumed by the new clause for deletion
	removed := 0

	for i := 0; i < s.learnedActiveCount; i++ {
		lits := s.getLearnedClauseLiterals(i)
		litsCopy := make([]cnf.Literal, len(lits))
		copy(litsCopy, lits)
		tmpClause := &cnf.Clause{Literals: litsCopy, Learned: true}
		if s.subsumes(newClause, tmpClause) {

			// P1 OPTIMIZATION: Remove watches immediately when clause is deleted
			s.removeLearnedClauseWatches(i)
			
			// Mark for deletion by clearing the clause
			s.learnedOffsets[i] = 0
			s.learnedSizes[i] = 0
			s.clauseActivity[i] = 0
			s.clauseAge[i] = 0
			s.clauseLBD[i] = 999999
			s.clauseUseCount[i] = 0
			s.clausePropCount[i] = 0
			removed++
		}
	}

	// Note: Watches removed immediately above, no cleanup needed during clause deletion
	if removed > 0 && s.verbose {
		fmt.Printf("c [verbose] Learned clause subsumption: marked %d clauses for deletion\n", removed)
	}
}

func (s *CDCLSolver) isSubsumedByAny(clause cnf.Clause, clauses []cnf.Clause) bool {
	for _, other := range clauses {
		if s.subsumes(&other, &clause) {
			return true
		}
	}
	return false
}

// inprocessSubsumption applies subsumption elimination during search (inprocessing)

// Inprocessing is the application of preprocessing techniques during the search phase.
// This is crucial for maintaining a small, simplified formula throughout solving.

// What it does:
// 1. Remove original clauses subsumed by shorter original clauses
// 2. Remove original clauses subsumed by learned clauses
// 3. Remove learned clauses subsumed by other learned clauses

// Why it helps:
// - Learned clauses can subsume original clauses (especially short learned clauses)
// - Reduces the formula size, making propagation faster
// - Removes redundant constraints that slow down search

// Safeguards:
// - Only run every inprocessingInterval conflicts (expensive O(n²) operation)
// - Skip on large formulas (>inprocessingMaxClauses clauses)
// - Time limit of inprocessingTimeLimitMs to avoid slowing down search
func (s *CDCLSolver) inprocessSubsumption() {
	if s.cnf.NumClauses > s.inprocessingMaxClauses {
		return
	}

	// Skip on very large clause databases
	if s.cnf.NumClauses > 5000 {
		return
	}

	startTime := time.Now()
	timeLimit := time.Duration(s.inprocessingTimeLimitMs) * time.Millisecond

	removedOriginal := 0
	removedLearned := 0

	// Remove original clauses subsumed by learned clauses
	// This is the most impactful: learned clauses are often shorter and more general
	remainingOriginal := make([]cnf.Clause, 0, len(s.cnf.Clauses))
	for i := range s.cnf.Clauses {
		subsumed := false
	for j := range s.learnedOffsets {
		lits := s.getLearnedClauseLiterals(j)
		litsCopy := make([]cnf.Literal, len(lits))
		copy(litsCopy, lits)
		tmpClause := &cnf.Clause{Literals: litsCopy, Learned: true}
		if s.subsumes(tmpClause, &s.cnf.Clauses[i]) {
			subsumed = true
			break
		}
	}
		if !subsumed {
			remainingOriginal = append(remainingOriginal, s.cnf.Clauses[i])
		} else {
			removedOriginal++
		}
	}

	if time.Since(startTime) > timeLimit {
		return
	}

	if removedOriginal > 0 {
		s.cnf.Clauses = remainingOriginal
		s.cnf.NumClauses = len(s.cnf.Clauses)
		if s.verbose {
			fmt.Printf("c [inprocess] Removed %d original clauses subsumed by learned clauses\n", removedOriginal)
		}
	}

	// Mark learned clauses subsumed by other learned clauses for deletion
	// Keep only the most general (shortest) learned clauses
	if s.learnedActiveCount > 100 {
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedSizes[i] == 0 {
				continue // Already marked for deletion
			}
			subsumed := false
			for j := range s.learnedOffsets {
				if i == j || s.learnedSizes[j] == 0 {
					continue
				}
				litsI := s.getLearnedClauseLiterals(i)
				litsJ := s.getLearnedClauseLiterals(j)
				litsICopy := make([]cnf.Literal, len(litsI))
				copy(litsICopy, litsI)
				litsJCopy := make([]cnf.Literal, len(litsJ))
				copy(litsJCopy, litsJ)
				clauseI := &cnf.Clause{Literals: litsICopy, Learned: true}
				clauseJ := &cnf.Clause{Literals: litsJCopy, Learned: true}
				if s.subsumes(clauseJ, clauseI) {
					subsumed = true

					break
				}
			}
			if subsumed {
				// P1 OPTIMIZATION: Remove watches immediately when clause is deleted
				s.removeLearnedClauseWatches(i)
				
				// Mark for deletion
				s.learnedOffsets[i] = 0
				s.learnedSizes[i] = 0
				s.clauseActivity[i] = 0
				s.clauseAge[i] = 0
				s.clauseLBD[i] = 999999
				removedLearned++
			}
		}

		if removedLearned > 0 && s.verbose {
			fmt.Printf("c [inprocess] Marked %d learned clauses for deletion (subsumed)\n", removedLearned)
		}
	}

	totalRemoved := removedOriginal + removedLearned
	if s.verbose && totalRemoved > 0 {
		fmt.Printf("c [inprocess] Inprocessing subsumption: removed %d clauses (%d original, %d learned)\n",
			totalRemoved, removedOriginal, removedLearned)
	}
}

// inprocessSubsumptionSafe removes original clauses subsumed by OTHER original clauses
// SOUND: Original clauses are permanent (never deleted), so subsumption is safe
// Does NOT subsume original clauses by learned clauses (learned clauses can be deleted!)
func (s *CDCLSolver) inprocessSubsumptionSafe() {
	if s.cnf.NumClauses > s.inprocessingMaxClauses {
		return
	}

	// Skip on very large clause databases
	if s.cnf.NumClauses > 5000 {
		return
	}

	startTime := time.Now()
	timeLimit := time.Duration(s.inprocessingTimeLimitMs) * time.Millisecond

	removedOriginal := 0

	// OPTIMIZATION P0: Use reusable buffer for clause indices to avoid allocation
	// Mark clauses to remove instead of building new slice
	toRemove := make([]bool, len(s.cnf.Clauses))

	for i := range s.cnf.Clauses {
		subsumed := false
		for j := range s.cnf.Clauses {
			if i == j {
				continue
			}
			if s.subsumes(&s.cnf.Clauses[j], &s.cnf.Clauses[i]) {
				subsumed = true
				break
			}
		}
		if subsumed {
			toRemove[i] = true
			removedOriginal++
		}
	}

	if time.Since(startTime) > timeLimit {
		return
	}

	if removedOriginal > 0 {
		// Build new clause list only if clauses were removed
		remainingOriginal := make([]cnf.Clause, 0, len(s.cnf.Clauses)-removedOriginal)
		for i := range s.cnf.Clauses {
			if !toRemove[i] {
				remainingOriginal = append(remainingOriginal, s.cnf.Clauses[i])
			}
		}
		s.cnf.Clauses = remainingOriginal
		s.cnf.NumClauses = len(s.cnf.Clauses)
		if s.verbose {
			fmt.Printf("c [inprocess] Safe subsumption: removed %d original clauses (subsumed by other originals)\n", removedOriginal)
		}
	}
}

// inprocessBlockedClauseElimination removes blocked clauses
// SOUND: Blocked clauses are redundant (removal preserves satisfiability)
// A clause C is blocked by literal l if for every clause D with ¬l,
// there exists a literal k where k ∈ C and ¬k ∈ D
func (s *CDCLSolver) inprocessBlockedClauseElimination() {
	if s.cnf.NumClauses > 2000 {
		return // Skip on large instances
	}

	startTime := time.Now()
	timeLimit := 50 * time.Millisecond // Short time limit

	removed := 0
	keptClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))

	for _, clause := range s.cnf.Clauses {
		isBlocked := false

		// Check if clause is blocked by any of its literals
		for _, lit := range clause.Literals {
			if s.isClauseBlockedBy(clause, lit) {
				isBlocked = true
				break
			}
		}

		if isBlocked {
			removed++
		} else {
			keptClauses = append(keptClauses, clause)
		}

		if time.Since(startTime) > timeLimit {
			break // Time limit reached
		}
	}

	if removed > 0 {
		s.cnf.Clauses = keptClauses
		s.cnf.NumClauses = len(s.cnf.Clauses)
		if s.verbose {
			fmt.Printf("c [inprocess] Blocked clause elimination: removed %d clauses\n", removed)
		}
	}
}

// isClauseBlockedBy checks if a clause is blocked by a specific literal
func (s *CDCLSolver) isClauseBlockedBy(clause cnf.Clause, blockingLit cnf.Literal) bool {
	// Find all clauses containing the opposite literal
	for _, other := range s.cnf.Clauses {
		if !s.containsLiteral(other, s.negateLiteral(blockingLit)) {
			continue // This clause doesn't contain ¬blockingLit
		}

		// Check if there's a resolving literal in the blocking clause
		hasResolver := false
		for _, lit := range clause.Literals {
			if lit == blockingLit {
				continue // Don't use the blocking literal itself
			}
			if s.containsLiteral(other, s.negateLiteral(lit)) {
				hasResolver = true
				break
			}
		}

		if !hasResolver {
			return false // Not blocked - no resolver for this other clause
		}
	}
	return true // Blocked by this literal
}

// containsLiteral checks if a clause contains a literal
func (s *CDCLSolver) containsLiteral(clause cnf.Clause, lit cnf.Literal) bool {
	for _, l := range clause.Literals {
		if l == lit {
			return true
		}
	}
	return false
}

// negateLiteral returns the negation of a literal
func (s *CDCLSolver) negateLiteral(lit cnf.Literal) cnf.Literal {
	return cnf.NewLiteral(lit.Var(), !lit.IsNegated())
}

func (s *CDCLSolver) isUnitLiteral(lit cnf.Literal) bool {
	varIdx := lit.Var()
	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) == 1 && clause.Literals[0].Var() == varIdx {
			return true
		}
	}
	return false
}

func (s *CDCLSolver) resolveOnVar(c1, c2 cnf.Clause, varIdx uint32) *cnf.Clause {
	literals := make([]cnf.Literal, 0)
	foundNeg := false
	foundPos := false

	for _, lit := range c1.Literals {
		if lit.Var() == varIdx {
			if lit.IsNegated() {
				foundNeg = true
			} else {
				foundPos = true
			}
		} else {
			literals = append(literals, lit)
		}
	}

	for _, lit := range c2.Literals {
		if lit.Var() == varIdx {
			if lit.IsNegated() {
				foundNeg = true
			} else {
				foundPos = true
			}
		} else {
			literals = append(literals, lit)
		}
	}

	if !(foundNeg && foundPos) {
		return nil
	}

	return &cnf.Clause{Literals: literals, Learned: false}
}

func (s *CDCLSolver) subsumes(c1, c2 *cnf.Clause) bool {
	// c1 subsumes c2 if c1 is a subset of c2 (all literals in c1 are also in c2)
	// Example: (x1) subsumes (x1 ∨ x2) because satisfying x1 automatically satisfies (x1 ∨ x2)
	// c1 must be shorter or equal length to c2
	if len(c1.Literals) > len(c2.Literals) {
		return false
	}

	// Check if all literals in c1 are in c2 (same variable AND same polarity)
	// Clauses are typically small (< 20 literals), so O(n*m) is acceptable
	for _, lit1 := range c1.Literals {
		found := false
		for _, lit2 := range c2.Literals {
			if lit1 == lit2 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func boolToUint(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func luby(i int) int {
	k := 1
	for {
		ki := 1 << uint(k)
		if i == ki-1 {
			return 1 << uint(k-1)
		}
		if ki-1 > i {
			prevKi := 1 << uint(k-1)
			return luby(i - (prevKi - 1))
		}
		k++
	}
}

// shouldRestart determines if the solver should restart search

// Restart Policies in CDCL Solvers:
// Restarts are essential for modern SAT solvers. They escape unproductive search
// regions where the solver is making poor decisions or exploring fruitless branches.

// Two Restart Policies Implemented:

// 1. Glucose-Style Adaptive Restarts (PRIMARY, more aggressive):
//    - Monitor the LBD of learned clauses during search
//    - When current LBD > 1.5× average LBD, the search is unproductive
//    - Restart immediately to try different decisions
//    - This is reactive: restarts based on actual search quality

//    Why it works:
//    - High LBD means the learned clause spans many decision levels
//    - This indicates the search is "lost" - decisions don't connect well
//    - Restarting allows the solver to make different decisions
//    - The 1.5× threshold is empirically optimal (Glucose solver)

// 2. Luby Sequence (FALLBACK, conservative):
//    - Geometric sequence: 1, 1, 2, 1, 1, 2, 4, 1, 1, 2, ...
//    - Multiply by restartBase (default 100) for conflict threshold
//    - Guaranteed to restart periodically even if LBD criterion not met
//    - This is proactive: restarts based on conflict count

// Hybrid Approach (NOW WITH CONFIGURABLE PARAMETERS):
// - First restartGlucoseMinConflicts conflicts: Use Luby only
// - After restartGlucoseMinConflicts: Use Glucose criterion (configurable ratio)
// - If Glucose criterion not met: Fall back to Luby (configurable base)

// What Happens on Restart:
// 1. Clear the trail (all assignments)
// 2. Keep only "glue clauses" (LBD ≤ restartKeepGlueLBD) - most valuable learned clauses
// 3. Delete all other learned clauses (50-90% reduction)
// 4. Reset LBD statistics for fresh measurement
// 5. Continue search with same VSIDS scores (learnings preserved)

// Why Keep Glue Clauses?
// Glue clauses (LBD ≤ 3) are the "backbone" of the search:
// - They connect few decision levels (highly general)
// - They propagate often and prune large parts of search space
// - Deleting them would cause the solver to re-explore the same conflicts
func (s *CDCLSolver) shouldRestart() bool {
	// Check Glucose-style adaptive restart first (if past min conflicts)
	if s.conflicts >= s.restartGlucoseMinConflicts && s.lbdCount > 0 {
		avgLBD := float64(s.lbdSum) / float64(s.lbdCount)
		
		// Glucose criterion: restart when recent LBD is much worse than average
		// Configurable via restartGlucoseRatio (default 1.5×)
		recentLBD := float64(s.lastConflictLBD)
		if recentLBD > avgLBD*s.restartGlucoseRatio {
			if s.verbose {
				fmt.Printf("c [restart] Glucose: LBD %.1f > avg %.1f × %.2f\n", 
					recentLBD, avgLBD, s.restartGlucoseRatio)
			}
			return true
		}
	}
	
	// Fall back to Luby sequence (configurable base)
	lubyValue := luby(s.lubyIndex + 1)
	threshold := lubyValue * s.restartBase

	if s.conflicts-s.restartCount >= threshold {
		return true
	}

	return false
}

func (s *CDCLSolver) restart() {
	if s.verbose {
		fmt.Printf("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)
	}

	// Use stored LBD values (calculated at learning time) instead of recalculating
	// Recalculating during restart gives wrong values since assignments change
	glueCount := 0
	isGlue := make([]bool, len(s.learnedOffsets))

	for i := 0; i < len(s.learnedOffsets); i++ {
		lbd := s.clauseLBD[i]

		// Keep glue clauses (configurable via restartKeepGlueLBD, default 3)
		// LBD ≤ 2: core glue (most valuable)
		// LBD = 3: near-glue (very valuable)
		// LBD > restartKeepGlueLBD: delete (will be re-learned if needed)
		if lbd <= s.restartKeepGlueLBD {
			glueCount++
			isGlue[i] = true
		}
	}

	if s.verbose {
		fmt.Printf("c [verbose] Restart: %d glue clauses (LBD≤%d), %d total active\n", glueCount, s.restartKeepGlueLBD, s.learnedActiveCount)
	}

	// NOTE: We don't delete clauses on restart - let deleteLearnedClauses handle memory management
	// Restart is for escaping local minima, not for clause deletion
	// Deleting clauses on restart throws away potentially useful learned information

	// Clear trail and assignments
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.qhead = 0 // Reset qhead since trail is empty
	s.level = 0
	for i := range s.implication {
		s.implication[i] = nil
	}
	for i := range s.assignments {
		s.assignments[i] = Assignment{}
		s.varLevel[i] = 0
	}
	// Reset conflicts at all levels
	for i := range s.conflictsAtLevel {
		s.conflictsAtLevel[i] = 0
	}

	// IMPROVEMENT #3: Reset exploration diversity tracking
	s.decidedVars = s.decidedVars[:0]
	for i := range s.decidedVarSet {
		s.decidedVarSet[i] = false
	}
	s.restartDecisionCount = 0

	// Reset restart counters
	s.lubyIndex++
	s.restartCount = s.conflicts
	s.lbdSum = 0
	s.lbdCount = 0
	
	// CRITICAL: Clear tmpFlippedVars on restart
	// tmpFlippedVars tracks variables flipped at level 1 to detect exhaustion
	// But it must be cleared on restart since all assignments are cleared
	// Failure to clear causes false UNSAT (variable flipped in old context blocks new search)
	for k := range s.tmpFlippedVars {
		s.tmpFlippedVars[k] = false
	}
	s.lastConflictLBD = 0
	s.backjumpLevel = 0

	// CRITICAL FIX: DO NOT reset VSIDS activity on restart
	// VSIDS activity MUST persist across restarts to remember important variables
	// Standard CDCL solvers (MiniSat, Glucose) never reset VSIDS activity
	// Resetting activity causes solver to repeat same mistakes after each restart
	// This was causing conflicts at level 50+ and poor quality learned clauses

	// Clear temporary buffers after restart (assignments are cleared, levels reset)
	for i := range s.tmpLiteralInClause {
		s.tmpLiteralInClause[i] = false
		s.tmpLiteralIsNegated[i] = false
	}
	for i := range s.tmpResolved {
		s.tmpResolved[i] = false
	}
	for i := range s.tmpLevelCount {
		s.tmpLevelCount[i] = 0
	}
	for i := range s.tmpLevelSetUsed {
		s.tmpLevelSetUsed[i] = false
		s.tmpLevelCountUsed[i] = false
	}
	s.tmpLevelSet = s.tmpLevelSet[:0]
	s.tmpCandidates = s.tmpCandidates[:0]
	s.tmpResolvedVars = s.tmpResolvedVars[:0]

	// CRITICAL FIX: Re-propagate unit clauses after restart
	// Unit clauses (length 1) are NOT watched, so they won't be re-propagated
	// by the watched literals scheme. We must re-assign them manually.
	s.level = 1
	s.trailHead = []int{0}
	for i := 0; i < s.cnf.NumClauses; i++ {
		clause := &s.cnf.Clauses[i]
		if len(clause.Literals) == 1 {
			lit := clause.Literals[0]
			varIdx := lit.Var()
			if s.assignments[varIdx].Level == 0 {
				value := !lit.IsNegated()
				s.assignments[varIdx] = Assignment{
					Value: value,
					Level: 1,
				}
				s.varLevel[varIdx] = 1
				s.trail = append(s.trail, int(varIdx))
				s.implication[varIdx] = clause
			}
		}
	}
	s.trailHead = append(s.trailHead, len(s.trail))
	// CRITICAL: Update qhead to skip the unit propagations we just added
	// Otherwise propagateWatched() will re-process them, causing massive slowdown
	s.qhead = len(s.trail)

}

func (s *CDCLSolver) isTautology(clause *cnf.Clause) bool {
	seen := make(map[uint32]bool)

	for _, lit := range clause.Literals {
		varIdx := lit.Var()
		isNeg := lit.IsNegated()

		if prevNeg, exists := seen[varIdx]; exists {
			if prevNeg != isNeg {
				return true
			}
		} else {
			seen[varIdx] = isNeg
		}
	}

	return false
}

func (s *CDCLSolver) blockedClauseElimination() SolveResult {
	// Increase limit to 15000 clauses to handle Sudoku and similar instances
	// BCE is O(n²) but very effective on structured instances
	if s.cnf.NumClauses > 15000 {
		if s.verbose {
			fmt.Printf("c [verbose] Blocked clause elimination: skipped (%d clauses, limit 15000)\n", s.cnf.NumClauses)
		}
		return UNKNOWN
	}

	if s.verbose {
		fmt.Printf("c [verbose] Blocked clause elimination: checking %d clauses\n", s.cnf.NumClauses)
	}

	removedCount := 0
	changed := true

	for changed {
		changed = false
		blocked := make([]bool, len(s.cnf.Clauses))

		for clauseIdx, clause := range s.cnf.Clauses {
			if len(clause.Literals) == 0 {
				return UNSAT
			}

			for _, blockingLit := range clause.Literals {
				if s.isClauseBlockedBy(clause, blockingLit) {
					blocked[clauseIdx] = true
					removedCount++
					changed = true
					break
				}
			}
		}

		if changed {
			remaining := make([]cnf.Clause, 0)
			for i, clause := range s.cnf.Clauses {
				if !blocked[i] {
					remaining = append(remaining, clause)
				}
			}
			s.cnf.Clauses = remaining
			s.cnf.NumClauses = len(remaining)
		}
	}

	if s.verbose {
		fmt.Printf("c [verbose] Blocked clause elimination: removed %d clauses\n", removedCount)
	}

	return UNKNOWN
}


func (s *CDCLSolver) inprocessing() {
	// OPTIMIZATION: Lowered threshold from 500 to 50 clauses to enable inprocessing on PHP instances
	// PHP 6p5h: 81 clauses, PHP 7p6h: 133 clauses, PHP 8p7h: 204 clauses - all now get inprocessing
	// Inprocessing (subsumption, self-subsumption) helps reduce clause database and find conflicts faster
	if s.cnf.NumClauses < 10 {  // Lowered to enable inprocessing after aggressive VE
		return
	}

	if s.verbose {
		fmt.Printf("c [inprocess] Inprocessing at conflict %d: %d clauses\n", s.conflicts, s.cnf.NumClauses)
	}

	initialClauses := s.cnf.NumClauses
	startTime := time.Now()
	timeLimit := 500 * time.Millisecond // Limit inprocessing time

	// 1. Unit propagation (cheap, can find new units from learned clauses)
	s.inprocessUnitPropagation()
	if time.Since(startTime) > timeLimit {
		return
	}

	// 2. Variable elimination DISABLED - soundness bug with variable tracking
	// Preprocessing VE eliminates most vars, search handles the rest
	if time.Since(startTime) > timeLimit {
		return
	}

	// 3. SAFE subsumption: Only use glue clauses (LBD ≤ 2) which are never deleted
	// This is sound: subsumed original clauses are permanently redundant
	s.inprocessSubsumptionSafe()
	if time.Since(startTime) > timeLimit {
		return
	}

	// 4. Blocked clause elimination (sound, removes redundant clauses)
	// Run every 200 conflicts (more expensive than subsumption)
	if s.conflicts%200 == 0 && s.cnf.NumClauses < 2000 {
		s.inprocessBlockedClauseElimination()
	}
	if time.Since(startTime) > timeLimit {
		return
	}

	// 5. Self-subsumption (every 1000 conflicts, more expensive)
	// Further reduce clause database after other simplifications
	if s.conflicts%1000 == 0 && s.cnf.NumClauses < 5000 {
		s.selfSubsumption()
	}
	if time.Since(startTime) > timeLimit {
		return
	}

	removed := initialClauses - s.cnf.NumClauses
	if s.verbose && removed != 0 {
		elapsed := time.Since(startTime)
		fmt.Printf("c [inprocess] Inprocessing complete: removed %d clauses in %.1fms\n", removed, float64(elapsed.Nanoseconds())/1e6)
	}

	// CRITICAL: Rebuild watch lists after clause database modifications
	// Watch lists must reflect current clause database to avoid stale references
	if s.watchInitialized {
		s.watchLists = make([][]cnf.Watch, 2*s.cnf.NumVars)
		s.watchInitialized = false
	}
	s.initWatches()

	// Clear qhead to re-process all trail elements with updated watches
	s.qhead = 0
}

// inprocessVariableElimination performs lightweight variable elimination during search
// Lighter version of variableElimination() with shorter time limit and fewer iterations
// Only eliminates variables with positive deficiency (net clause reduction)
// Does NOT track eliminated variables for model reconstruction (too complex during search)
func (s *CDCLSolver) inprocessVariableElimination() {
	if s.verbose {
		fmt.Printf("c [inprocess] Variable elimination during search: %d vars, %d clauses, maxOcc=%d, maxRes=%d\n",
			s.cnf.NumVars, s.cnf.NumClauses, s.varElimMaxOccurrences, s.varElimMaxResolventSize)
	}

	startTime := time.Now()
	// Aggressive time limit for inprocessing VE - PHP instances need more time
	timeLimit := 500 * time.Millisecond
	maxIters := 2 // Eliminate only 1-2 vars per call to prevent clause explosion (MiniSat-style gradual elimination)
	eliminatedCount := 0

	for iter := 0; iter < maxIters; iter++ {
		if time.Since(startTime) > timeLimit {
			break
		}

		// Count occurrences
		posCount := make([]int, s.cnf.NumVars)
		negCount := make([]int, s.cnf.NumVars)
		posClauses := make(map[uint32][]int)
		negClauses := make(map[uint32][]int)

		for clauseIdx, clause := range s.cnf.Clauses {
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if lit.IsNegated() {
					negCount[varIdx]++
					negClauses[varIdx] = append(negClauses[varIdx], clauseIdx)
				} else {
					posCount[varIdx]++
					posClauses[varIdx] = append(posClauses[varIdx], clauseIdx)
				}
			}
		}

		// Find best eliminatable variable
		bestVar := uint32(0)
		// For inprocessing, use conservative thresholds to avoid clause explosion
		bestResolventSize := 1000 // Only eliminate if resolvent is small
		bestDeficiency := -100.0  // Allow some clause blowup but not extreme
		hasEliminatable := false

		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			posOcc := posCount[varIdx]
			negOcc := negCount[varIdx]

			if posOcc == 0 || negOcc == 0 {
				continue
			}

			totalOcc := posOcc + negOcc
			// For inprocessing, use conservative thresholds
			maxOcc := 500 // Skip high-occurrence variables
			if totalOcc > maxOcc {
				continue
			}

			resolventSize := posOcc * negOcc

			// For inprocessing, only eliminate if resolvent is manageable
			maxResolvent := 2000 // Skip if resolvent would be too large
			if resolventSize > maxResolvent {
				continue
			}

			deficiency := float64(posOcc + negOcc) - float64(resolventSize)

			if s.verbose && iter == 0 && varIdx < 5 {
				fmt.Printf("c [inprocess] VE: var %d pos=%d neg=%d total=%d resolvent=%d deficiency=%.1f\n",
					varIdx, posOcc, negOcc, totalOcc, resolventSize, deficiency)
			}

			if resolventSize < bestResolventSize ||
				(resolventSize == bestResolventSize && deficiency > bestDeficiency) {
				bestVar = varIdx
				bestResolventSize = resolventSize
				bestDeficiency = deficiency
				hasEliminatable = true
			}
		}

		if !hasEliminatable {
			if s.verbose && iter == 0 {
				fmt.Printf("c [inprocess] VE: no eliminatable variables found\n")
			}
			break
		}

		if s.verbose && iter == 0 {
			fmt.Printf("c [inprocess] VE: checking %d vars, bestVar=%d, bestResolvent=%d, bestDeficiency=%.1f\n",
				s.cnf.NumVars, bestVar, bestResolventSize, bestDeficiency)
		}

		// Eliminate the variable
		varIdx := bestVar
		posCls := posClauses[varIdx]
		negCls := negClauses[varIdx]

		// Generate resolvents
		newResolvents := make([]*cnf.Clause, 0)
		resolventHashes := make(map[uint64]bool)

		for _, pIdx := range posCls {
			for _, nIdx := range negCls {
				posClause := s.cnf.Clauses[pIdx]
				negClause := s.cnf.Clauses[nIdx]

				resolvent := s.resolveOnVarElim(posClause, negClause, varIdx)
				if resolvent != nil {
					if len(resolvent.Literals) == 0 {
						// Empty clause found - should not happen during search on satisfiable instances
						return
					}

					if s.isTautology(resolvent) {
						continue
					}

					hash := s.clauseHash(resolvent)
					if !resolventHashes[hash] {
						resolventHashes[hash] = true
						newResolvents = append(newResolvents, resolvent)
					}
				}
			}
		}

		// Remove old clauses and add resolvents
		// Mark clauses for removal (those containing varIdx)
		toRemove := make(map[int]bool)
		for _, idx := range posCls {
			toRemove[idx] = true
		}
		for _, idx := range negCls {
			toRemove[idx] = true
		}

		// Build new clause list
		newClauses := make([]cnf.Clause, 0, s.cnf.NumClauses-len(posCls)-len(negCls)+len(newResolvents))
		for i, clause := range s.cnf.Clauses {
			if !toRemove[i] {
				newClauses = append(newClauses, clause)
			}
		}
		for _, resolvent := range newResolvents {
			newClauses = append(newClauses, *resolvent)
		}

		s.cnf.Clauses = newClauses
		s.cnf.NumClauses = len(newClauses)
		s.cnf.RebuildLiteralPool()

		eliminatedCount++
	}

	if s.verbose && eliminatedCount > 0 {
		elapsed := time.Since(startTime)
		fmt.Printf("c [inprocess] VE eliminated %d variables in %.1fms, now %d clauses\n",
			eliminatedCount, float64(elapsed.Nanoseconds())/1e6, s.cnf.NumClauses)
	}
}

// inprocessPureLiteralElimination performs pure literal elimination during search
// Assigns variables appearing in only one polarity, removes satisfied clauses
// Can create cascade: assigning pure literals may make other variables pure
// Does NOT track eliminated variables for model reconstruction (too complex during search)
func (s *CDCLSolver) inprocessPureLiteralElimination() {
	if s.verbose {
		fmt.Printf("c [inprocess] Pure literal elimination during search: %d vars, %d clauses\n",
			s.cnf.NumVars, s.cnf.NumClauses)
	}

	startTime := time.Now()
	timeLimit := 50 * time.Millisecond // Short time limit for inprocessing
	assignedCount := 0

	changed := true
	for changed {
		if time.Since(startTime) > timeLimit {
			break
		}

		changed = false

		// Scan for pure literals
		hasPositive := make([]bool, s.cnf.NumVars)
		hasNegative := make([]bool, s.cnf.NumVars)

		for _, clause := range s.cnf.Clauses {
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if lit.IsNegated() {
					hasNegative[varIdx] = true
				} else {
					hasPositive[varIdx] = true
				}
			}
		}

		// Assign pure literals
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			isPure := false
			pureValue := false

			if hasPositive[varIdx] && !hasNegative[varIdx] {
				isPure = true
				pureValue = true
			} else if hasNegative[varIdx] && !hasPositive[varIdx] {
				isPure = true
				pureValue = false
			}

			if isPure {
				// Assign the pure literal
				s.assignments[varIdx] = Assignment{
					Value: pureValue,
					Level: 1,
				}
				changed = true
				assignedCount++

				// Remove satisfied clauses
				newClauses := make([]cnf.Clause, 0, s.cnf.NumClauses)
				for _, clause := range s.cnf.Clauses {
					satisfied := false
					for _, lit := range clause.Literals {
						if lit.Var() == varIdx {
							if (lit.IsNegated() && !pureValue) || (!lit.IsNegated() && pureValue) {
								satisfied = true
								break
							}
						}
					}
					if !satisfied {
						newClauses = append(newClauses, clause)
					}
				}
				s.cnf.Clauses = newClauses
				s.cnf.NumClauses = len(newClauses)

				if s.verbose {
					fmt.Printf("c [inprocess] Pure literal: assigned var %d = %v, now %d clauses\n",
						varIdx, pureValue, s.cnf.NumClauses)
				}
			}
		}
	}

	if s.verbose && assignedCount > 0 {
		elapsed := time.Since(startTime)
		fmt.Printf("c [inprocess] Pure literal eliminated %d variables in %.1fms, now %d clauses\n",
			assignedCount, float64(elapsed.Nanoseconds())/1e6, s.cnf.NumClauses)
	}
}

// clauseHash computes a simple hash for a clause (for duplicate detection)
func (s *CDCLSolver) clauseHash(clause *cnf.Clause) uint64 {
	hash := uint64(len(clause.Literals))
	for _, lit := range clause.Literals {
		hash = hash*31 + uint64(lit)
	}
	return hash
}

func (s *CDCLSolver) unitPropagationPreprocess() SolveResult {
	// Clear any previous assignments before starting unit propagation
	// This is critical when called multiple times in preprocessing loop
	for i := range s.assignments {
		s.assignments[i] = Assignment{}
	}

	// Initialize trail for preprocessing
	s.trail = make([]int, 0)
	s.trailHead = []int{0}
	s.level = 1

	changed := true
	for changed {
		changed = false

		clauseCount := len(s.cnf.Clauses)
		for clauseIdx := 0; clauseIdx < clauseCount; clauseIdx++ {
			clause := s.cnf.Clauses[clauseIdx]

			satisfied := false
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal

			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level != 0 {
					assign := s.assignments[varIdx]
					isTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
					if isTrue {
						satisfied = true
						break
					}
					falseCount++
				} else {
					unassignedCount++
					unassignedLit = lit
				}
			}

			if satisfied {
				continue
			}

			if unassignedCount == 0 && falseCount > 0 {
				if s.verbose {
					fmt.Printf("c [verbose] Preprocessing: conflict in unit propagation\n")
				}
				return UNSAT
			}

			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				varIdx := unassignedLit.Var()
				value := !unassignedLit.IsNegated()
				s.assignments[varIdx] = Assignment{
					Value: value,
					Level: 1,
				}
				s.varLevel[varIdx] = 1
				s.trail = append(s.trail, int(varIdx))
				changed = true
				// Don't modify clauses - just track assignments in trail
			}
		}
	}

	// Set up trail for search
	s.trailHead = []int{0, len(s.trail)}
	s.level = 1

	return UNKNOWN
}

// inprocessUnitPropagation performs unit propagation during search
// Unlike unitPropagationPreprocess, this runs on the current trail state
// and doesn't reset assignments. It's safe to call during search.
func (s *CDCLSolver) inprocessUnitPropagation() {
	// SOUND INPROCESSING: Level-0 unit propagation
	// Only process original clauses (learned clauses change too frequently)
	// Assignments are made at level 0 (permanent, never backtracked)
	// This is sound: equivalent to preprocessing unit propagation
	if s.cnf.NumClauses > 10000 {
		return
	}

	// Collect all unit clauses first (avoid modifying during iteration)
	type unitClause struct {
		varIdx uint32
		value  bool
	}
	units := make([]unitClause, 0, 16)

	// Phase 1: Scan for unit clauses
	for i := 0; i < s.cnf.NumClauses && i < len(s.cnf.Clauses); i++ {
		clause := &s.cnf.Clauses[i]
		if len(clause.Literals) != 1 {
			continue
		}

		lit := clause.Literals[0]
		varIdx := lit.Var()

		// Check if already assigned
		if s.assignments[varIdx].Level != 0 {
			continue
		}

		units = append(units, unitClause{
			varIdx: varIdx,
			value:  !lit.IsNegated(),
		})
	}

	// Phase 2: Assign all units at level 0 (permanent)
	for _, unit := range units {
		s.assignments[unit.varIdx] = Assignment{
			Value: unit.value,
			Level: 0, // CRITICAL: Level 0, not current level
		}
		s.varLevel[unit.varIdx] = 0

		if s.verbose {
			fmt.Printf("c [inprocess] Unit propagation: var %d = %v (level 0)\n", unit.varIdx, unit.value)
		}
	}
}

func (s *CDCLSolver) simplifyAfterAssignment(varIdx uint32, value bool) bool {
	newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))
	for _, clause := range s.cnf.Clauses {
		satisfied := false
		newLiterals := make([]cnf.Literal, 0, len(clause.Literals))

		for _, lit := range clause.Literals {
			if lit.Var() == varIdx {
				litValue := !lit.IsNegated()
				if litValue == value {
					satisfied = true
					break
				}
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}

		if satisfied {
			continue
		}

		if len(newLiterals) == 0 {
			return true
		}

		newClauses = append(newClauses, cnf.Clause{Literals: newLiterals, Learned: false})
	}
	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)
	return false
}

func (s *CDCLSolver) equivalenceDetection() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] EquivalenceDetection() called with %d clauses\n", s.cnf.NumClauses)
	}

	// Detect equivalence relations from binary clauses
	// Pattern: (¬a ∨ b) ∧ (¬b ∨ a) means a ↔ b
	// Build equivalence classes and substitute representatives
	// Enhanced to detect transitive chains: a ↔ b and b ↔ c implies a ↔ c

	// Step 1: Find all bidirectional implications
	// Store as adjacency list for finding bidirectional edges
	type implication struct {
		from, to uint32
	}
	implications := make([]implication, 0)
	binaryCount := 0

	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) != 2 {
			continue
		}

		binaryCount++
		lit1 := clause.Literals[0]
		lit2 := clause.Literals[1]

		// Only detect (¬a ∨ b) pattern for equivalence
		if lit1.IsNegated() && !lit2.IsNegated() {
			// (¬a ∨ b) = a → b
			implications = append(implications, implication{lit1.Var(), lit2.Var()})
		} else if !lit1.IsNegated() && lit2.IsNegated() {
			// (a ∨ ¬b) = b → a
			implications = append(implications, implication{lit2.Var(), lit1.Var()})
		}
		// Skip (a ∨ b) and (¬a ∨ ¬b) - not equivalence patterns
	}

	if s.verbose && len(implications) > 0 {
		fmt.Printf("c [verbose] Equivalence detection: found %d implications from %d binary clauses\n",
			len(implications), binaryCount)
	}

	// Step 2: Build bidirectional graph using 2D slice (faster than map)
	// hasEdge[a][b] = true if a → b exists
	hasEdge := make([][]bool, s.cnf.NumVars)
	for i := range hasEdge {
		hasEdge[i] = make([]bool, s.cnf.NumVars)
	}
	for _, imp := range implications {
		hasEdge[imp.from][imp.to] = true
	}

	// Step 3: Use union-find to group equivalent variables
	parent := make([]uint32, s.cnf.NumVars)
	for i := range parent {
		parent[i] = uint32(i)
	}

	var find func(uint32) uint32
	find = func(x uint32) uint32 {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}

	union := func(x, y uint32) {
		px, py := find(x), find(y)
		if px != py {
			parent[px] = py
		}
	}

	// Find bidirectional implications and union them
	for a := uint32(0); a < s.cnf.NumVars; a++ {
		for b := uint32(0); b < s.cnf.NumVars; b++ {
			if hasEdge[a][b] && hasEdge[b][a] {
				// Found: a → b and b → a, so a ↔ b
				union(a, b)
			}
		}
	}

	// Step 4: Count equivalence classes and build substitution map
	classMembers := make(map[uint32][]uint32)
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		root := find(varIdx)
		classMembers[root] = append(classMembers[root], varIdx)
	}

	type substitution struct {
		rep     uint32
		samePol bool // always true for standard equivalence
	}
	substMap := make(map[uint32]substitution)

	for _, members := range classMembers {
		if len(members) < 2 {
			continue
		}

		// Representative is the lowest index
		rep := members[0]
		for _, m := range members[1:] {
			substMap[m] = substitution{rep: rep, samePol: true}
		}
	}

	if len(substMap) == 0 {
		if s.verbose && len(implications) > 0 {
			fmt.Printf("c [verbose] Equivalence detection: no bidirectional implications found\n")
		}
		return UNKNOWN
	}

	// Step 5: Substitute throughout formula
	newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))

	if s.verbose {
		fmt.Printf("c [debug] Equivalence detection: processing %d clauses\n", len(s.cnf.Clauses))
	}

	for _, clause := range s.cnf.Clauses {
		newLiterals := make([]cnf.Literal, 0, len(clause.Literals))
		clauseChanged := false

		for _, lit := range clause.Literals {
			varIdx := lit.Var()

			if subst, exists := substMap[varIdx]; exists {
				// Preserve polarity when substituting: if lit is negated, new lit is negated
				newLit := cnf.NewLiteral(subst.rep, lit.IsNegated())
				newLiterals = append(newLiterals, newLit)
				clauseChanged = true
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}

		// Remove duplicate literals and detect tautologies
		if clauseChanged {
			seen := make(map[uint32]bool)
			uniqueLiterals := make([]cnf.Literal, 0)
			hasBothPolarities := false

			for _, lit := range newLiterals {
				varIdx := lit.Var()
				if _, exists := seen[varIdx]; exists {
					// Duplicate variable - check if opposite polarity
					if seen[varIdx] != lit.IsNegated() {
						hasBothPolarities = true
						break
					}
				} else {
					seen[varIdx] = lit.IsNegated()
					uniqueLiterals = append(uniqueLiterals, lit)
				}
			}

			if hasBothPolarities {
				continue // Tautology
			}
			newLiterals = uniqueLiterals
		}

		if len(newLiterals) == 0 {
			if s.verbose {
				fmt.Printf("c [verbose] Equivalence detection: empty clause created (UNSAT)\n")
			}
			return UNSAT
		}

		newClauses = append(newClauses, cnf.Clause{Literals: newLiterals, Learned: clause.Learned})
	}

	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)

	// Zero out activity for eliminated variables
	for varIdx := range substMap {
		s.vsids.activity[varIdx] = 0.0
	}

	if s.verbose {
		fmt.Printf("c [verbose] Equivalence detection: substituted %d variables, resulting in %d clauses\n",
			len(substMap), s.cnf.NumClauses)
	}

	return UNKNOWN
}

func (s *CDCLSolver) pureLiteralElimination() SolveResult {
	changed := true
	for changed {
		changed = false

		hasPositive := make([]bool, s.cnf.NumVars)
		hasNegative := make([]bool, s.cnf.NumVars)

		for _, clause := range s.cnf.Clauses {
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level != 0 {
					continue
				}
				if lit.IsNegated() {
					hasNegative[varIdx] = true
				} else {
					hasPositive[varIdx] = true
				}
			}
		}

		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level != 0 {
				continue
			}

			isPure := false
			pureValue := false

			if hasPositive[varIdx] && !hasNegative[varIdx] {
				isPure = true
				pureValue = true
			} else if hasNegative[varIdx] && !hasPositive[varIdx] {
				isPure = true
				pureValue = false
			}

			if isPure {
				s.assignments[varIdx] = Assignment{
					Value: pureValue,
					Level: 1,
				}
				changed = true

				conflict := s.simplifyAfterAssignment(varIdx, pureValue)
				if conflict {
					if s.verbose {
						fmt.Printf("c [verbose] Pure literal elimination: empty clause created\n")
					}
					return UNSAT
				}

				if s.verbose {
					fmt.Printf("c [verbose] Pure literal elimination: assigned var %d = %v\n", varIdx, pureValue)
				}
			}
		}
	}

	if len(s.cnf.Clauses) == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Pure literal elimination: all clauses satisfied\n")
		}
		// CRITICAL: Don't return SAT if there are eliminated variables
		// The eliminated variables need to be reconstructed from their definitions
		// Let CDCL solve the (now empty) formula, then reconstruct
		if len(s.eliminatedVars) > 0 {
			if s.verbose {
				fmt.Printf("c [verbose] Pure literal elimination: deferring SAT to allow model reconstruction (%d eliminated vars)\n",
					len(s.eliminatedVars))
			}
			return UNKNOWN
		}
		// Assign all remaining unassigned variables arbitrarily
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level == 0 {
				// Use saved phase or default to false
				varValue := false
				if varIdx < uint32(len(s.savedPhase)) {
					varValue = s.savedPhase[varIdx]
				}
				s.assignments[varIdx] = Assignment{
					Value: varValue,
					Level: 1,
				}
			}
		}
		return SAT
	}

	return UNKNOWN
}

// variableElimination eliminates variables via resolution and tracks definitions for model reconstruction
// Returns UNSAT if empty clause found, UNKNOWN otherwise
// Implements bounded variable elimination (BVE) with:
//   - Occurrence cutoff: skip variables appearing in too many clauses
//   - Deficiency heuristic: only eliminate if resolvents < original clauses
//   - Resolvent size bound: skip if product of pos/neg occurrences exceeds threshold
//   - Subsumption check: filter resolvents subsumed by existing clauses
//   - Time limit: abort if VE takes too long
//   - Iteration limit: max variables eliminated per pass
// CRITICAL: Processes one variable at a time and re-computes elimination candidates after each elimination
func (s *CDCLSolver) variableElimination() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] VE DEBUG: starting with %d variables, %d clauses\n",
			s.cnf.NumVars, s.cnf.NumClauses)
	}
	




	eliminatedCount := 0
	clausesRemoved := 0
	startTime := time.Now()
	iterCount := 0

	// Keep eliminating variables until no more can be eliminated
	for {
		// Check time limit
		if s.varElimMaxTimeMs > 0 {
			elapsed := time.Since(startTime).Milliseconds()
			if elapsed > int64(s.varElimMaxTimeMs) {
				if s.verbose {
					fmt.Printf("c [verbose] Variable elimination: time limit reached (%dms)\n", elapsed)
				}
				break
			}
		}
		
		// Check iteration limit
		if s.varElimMaxIterations > 0 && iterCount >= s.varElimMaxIterations {
			if s.verbose {
				fmt.Printf("c [verbose] Variable elimination: iteration limit reached (%d vars)\n", iterCount)
			}
			break
		}
		// Count occurrences of each variable (positive and negative)
		posCount := make([]int, s.cnf.NumVars)
		negCount := make([]int, s.cnf.NumVars)
		posClauses := make(map[uint32][]int) // varIdx -> clause indices
		negClauses := make(map[uint32][]int)

		for clauseIdx, clause := range s.cnf.Clauses {
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if lit.IsNegated() {
					negCount[varIdx]++
					negClauses[varIdx] = append(negClauses[varIdx], clauseIdx)
				} else {
					posCount[varIdx]++
					posClauses[varIdx] = append(posClauses[varIdx], clauseIdx)
				}
			}
		}

		// Find best eliminatable variable using bounded heuristics
		// CRITICAL FIX: Only eliminate variables with posOcc=1 for sound model reconstruction
		// When pos=1, the definition is simply: x = ¬A (where (x ∨ A) is the positive clause)
		// When pos>1, concatenating all positive clause literals produces incorrect definitions
		bestVar := uint32(0)
		maxResolventSize := s.varElimMaxResolventSize + 1
		bestResolventSize := maxResolventSize
		bestDeficiency := -1.0 // Higher = better (more clauses removed than added)
		hasEliminatable := false

		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			posOcc := posCount[varIdx]
			negOcc := negCount[varIdx]
			
			// Skip if variable doesn't appear in both polarities
			if posOcc == 0 || negOcc == 0 {
				continue
			}

			// CRITICAL: Only eliminate variables with SINGLE positive clause (pos=1)
			// This guarantees sound model reconstruction
			if posOcc != 1 {
				continue
			}

			// OCCURRENCE CUTOFF: Skip variables appearing in too many clauses
			totalOcc := posOcc + negOcc
			if totalOcc > s.varElimMaxOccurrences {
				continue
			}

			// RESOLVENT SIZE BOUND: Skip if product exceeds threshold
			resolventSize := posOcc * negOcc
			if resolventSize > s.varElimMaxResolventSize {
				continue
			}

			// DEFICIENCY HEURISTIC: Only eliminate if we're removing more clauses than adding
			// deficiency = (posClauses + negClauses) - (posClauses * negClauses)
			// Positive deficiency = net clause reduction
			deficiency := float64(posOcc + negOcc) - float64(resolventSize)
			
			// Apply minimum deficiency threshold if configured
			if s.varElimMinDeficiency > 0.0 && deficiency < s.varElimMinDeficiency {
				continue
			}

			// Select variable with: 1) smallest resolvent, 2) highest deficiency as tiebreaker
			if resolventSize < bestResolventSize || 
			   (resolventSize == bestResolventSize && deficiency > bestDeficiency) {
				bestVar = varIdx
				bestResolventSize = resolventSize
				bestDeficiency = deficiency
				hasEliminatable = true
			}
		}

		if !hasEliminatable {
			if s.verbose && eliminatedCount == 0 {
				fmt.Printf("c [verbose] Variable elimination: no variables to eliminate\n")
			}
			break // No more variables to eliminate
		}

		// Eliminate the best variable
		varIdx := bestVar
		posCls := posClauses[varIdx]
		negCls := negClauses[varIdx]
		originalClauses := len(posCls) + len(negCls)

		if s.verbose {
			fmt.Printf("c [debug] Eliminating var %d (resolvent size=%d, deficiency=%.1f, pos=%d, neg=%d)\n",
				varIdx, bestResolventSize, bestDeficiency, len(posCls), len(negCls))
		}

		// Generate resolvents with subsumption filtering
		newResolvents := make([]cnf.Clause, 0)
		resolventHashes := make(map[uint64]bool) // Duplicate detection
		
		for _, pIdx := range posCls {
			for _, nIdx := range negCls {
				posClause := s.cnf.Clauses[pIdx]
				negClause := s.cnf.Clauses[nIdx]

				// Resolve on varIdx
				resolvent := s.resolveOnVarElim(posClause, negClause, varIdx)
				if resolvent != nil {
					// Check for empty clause (UNSAT)
					if len(resolvent.Literals) == 0 {
						if s.verbose {
							fmt.Printf("c [verbose] Variable elimination: empty clause found (UNSAT)\n")
						}
						return UNSAT
					}
					
					// Skip tautologies
					if s.isTautology(resolvent) {
						continue
					}
					
					// DUPLICATE DETECTION: Skip if we already generated this resolvent
					hash := computeCanonicalHash(resolvent.Literals, s.tmpSortedLits)
					if resolventHashes[hash] {
						continue
					}
					resolventHashes[hash] = true
					
					// SUBSUMPTION CHECK: Skip if subsumed by existing clause
					// (expensive, so only check for small resolvents)
					if len(resolvent.Literals) <= 5 {
						subsumed := false
						for _, existing := range s.cnf.Clauses {
							if s.subsumes(&existing, resolvent) {
								subsumed = true
								break
							}
						}
						if subsumed {
							continue
						}
					}
					
					newResolvents = append(newResolvents, *resolvent)
				}
			}
		}

		// Check deficiency again with actual resolvent count (after filtering)
		actualDeficiency := float64(originalClauses) - float64(len(newResolvents))
		if s.varElimMinDeficiency > 0.0 && actualDeficiency < s.varElimMinDeficiency {
			// Not worth eliminating after filtering
			if s.verbose {
				fmt.Printf("c [debug] Skipping var %d: actual deficiency %.1f < threshold %.1f\n",
					varIdx, actualDeficiency, s.varElimMinDeficiency)
			}
			// Mark this variable as uneliminatable by zeroing its counts
			posCount[varIdx] = 0
			negCount[varIdx] = 0
			continue
		}

		// Store definition for model reconstruction
		// With pos=1 restriction, there's exactly one positive clause: (x ∨ A)
		// Reconstruction rule: x = ¬A (x is true iff A is false)
		allALits := make([]cnf.Literal, 0)
		for _, pIdx := range posCls {
			posClause := s.cnf.Clauses[pIdx]
			for _, lit := range posClause.Literals {
				if lit.Var() != varIdx {
					allALits = append(allALits, lit)
				}
			}
		}
		s.varElimDefinition[varIdx] = allALits
		s.varElimPolarity[varIdx] = true

		s.eliminatedVars = append(s.eliminatedVars, varIdx)
		
		if s.verbose {
			fmt.Printf("c [debug] Eliminating var %d (resolvent size=%d, deficiency=%.1f, pos=%d, neg=%d)\n",
				varIdx, bestResolventSize, bestDeficiency, len(posCls), len(negCls))
		}
		// Assign eliminated variable so VSIDS does not select it
		s.assignments[varIdx] = Assignment{
			Value: s.varElimPolarity[varIdx],
			Level: 1,
		}
		eliminatedCount++
		iterCount++
		clausesRemoved += originalClauses

		// Build new clause list: keep clauses that don't contain varIdx, add resolvents
		newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses)-originalClauses+len(newResolvents))
		for _, clause := range s.cnf.Clauses {
			keep := true
			for _, lit := range clause.Literals {
				if lit.Var() == varIdx {
					keep = false
					break
				}
			}
			if keep {
				newClauses = append(newClauses, clause)
			}
		}
		newClauses = append(newClauses, newResolvents...)
		s.cnf.Clauses = newClauses
		s.cnf.NumClauses = len(newClauses)

		// Zero out VSIDS activity for eliminated variable
		s.vsids.activity[varIdx] = 0.0
	}

	// Zero out activity for all eliminated variables
	for _, varIdx := range s.eliminatedVars {
		s.vsids.activity[varIdx] = 0.0
	}

	if s.verbose && eliminatedCount > 0 {
		fmt.Printf("c [VE DEBUG] Eliminated %d variables, removed %d clauses, %d clauses remaining\n",
			eliminatedCount, clausesRemoved, s.cnf.NumClauses)

	}
	


	return UNKNOWN
}

// resolveOnVarElim resolves two clauses on a variable (for variable elimination)
// Returns nil if resolvent is empty or tautological
func (s *CDCLSolver) resolveOnVarElim(clause1, clause2 cnf.Clause, varIdx uint32) *cnf.Clause {
	// Find the literals for varIdx
	var lit1, lit2 cnf.Literal
	found1, found2 := false, false

	for _, lit := range clause1.Literals {
		if lit.Var() == varIdx {
			lit1 = lit
			found1 = true
			break
		}
	}
	for _, lit := range clause2.Literals {
		if lit.Var() == varIdx {
			lit2 = lit
			found2 = true
			break
		}
	}

	if !found1 || !found2 {
		return nil
	}

	// Check if they have opposite polarity (required for resolution)
	if lit1.IsNegated() == lit2.IsNegated() {
		return nil
	}

	// Build resolvent (all literals except the resolved variable)
	resolventLits := make([]cnf.Literal, 0, len(clause1.Literals)+len(clause2.Literals)-2)
	for _, lit := range clause1.Literals {
		if lit.Var() != varIdx {
			resolventLits = append(resolventLits, lit)
		}
	}
	for _, lit := range clause2.Literals {
		if lit.Var() != varIdx {
			resolventLits = append(resolventLits, lit)
		}
	}

	if len(resolventLits) == 0 {
		// Empty clause = UNSAT
		return &cnf.Clause{Literals: make([]cnf.Literal, 0)}
	}

	return &cnf.Clause{Literals: resolventLits, Learned: false}
}

// reconstructEliminatedVars reconstructs assignments for eliminated variables
// Must be called after solving returns SAT
func (s *CDCLSolver) reconstructEliminatedVars() {
	if len(s.eliminatedVars) == 0 {
		return
	}

	if s.verbose {
		fmt.Printf("c [verbose] Reconstructing %d eliminated variables\n", len(s.eliminatedVars))
	}

	// Reconstruct in reverse elimination order (last eliminated first)
	for i := len(s.eliminatedVars) - 1; i >= 0; i-- {
		varIdx := s.eliminatedVars[i]
		defLits, exists := s.varElimDefinition[varIdx]
		if !exists {
			continue
		}

		// Evaluate the definition for pos=1 elimination
		// Variable x was eliminated with single positive clause: (x ∨ A₁ ∨ A₂ ∨ ... ∨ Aₙ)
		// Reconstruction rule: x = ¬(A₁ ∨ A₂ ∨ ... ∨ Aₙ) = ¬A₁ ∧ ¬A₂ ∧ ... ∧ ¬Aₙ
		// In other words: x is true IFF all literals in the definition are false
		allFalse := true
		for _, lit := range defLits {
			litVar := lit.Var()
			if int(litVar) >= len(s.assignments) {
				continue
			}
			assign := s.assignments[litVar]
			if assign.Level == 0 {
				// Unassigned variable in definition - can't determine, treat as false (conservative)
				continue
			}
			litValue := assign.Value
			if lit.IsNegated() {
				litValue = !litValue
			}
			if litValue {
				// A literal in the definition is true, so x must be false
				allFalse = false
				break
			}
		}
		// x = ¬(OR of definition literals)
		defValue := allFalse

		// If definition is empty (unit clause (x) was eliminated), x must be true
		if len(defLits) == 0 {
			defValue = true
		}

		s.assignments[varIdx] = Assignment{
			Value: defValue,
			Level: 1, // Mark as assigned (not a decision)
		}

		if s.verbose {
			fmt.Printf("c [debug] Reconstructed var %d = %v from definition\n", varIdx, defValue)
		}
	}
}

// Solve determines if the CNF formula is satisfiable.
//
// Returns true if SAT, false if UNSAT or UNKNOWN.
// For detailed results, use SolveWithResult().
//
// This is the main solving interface. The solver uses CDCL with:
//   - Watched literals propagation
//   - 1-UIP conflict analysis
//   - VSIDS variable selection
//   - Clause learning and deletion
//
// Example:
//
//	solver := solver.NewCDCLSolver(cnf)
//	if solver.Solve() {
//	    fmt.Println("SAT")
//	} else {
//	    fmt.Println("UNSAT or UNKNOWN")
//	}
func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

// SolveWithPreprocessing runs aggressive preprocessing before solving.
//
// Preprocessing techniques include:
//   - Unit propagation
//   - Equivalence detection and substitution
//   - Pure literal elimination
//   - Subsumption elimination
//   - Self-subsumption
//   - Hyper-binary resolution
//
// Preprocessing is skipped on very small (<50 clauses) or very large
// (>10K vars, >50K clauses) instances.
//
// Returns SAT, UNSAT, or UNKNOWN. Use GetAssignments() to retrieve
// the model if SAT.
func (s *CDCLSolver) SolveWithPreprocessing() SolveResult {
	// Run aggressive preprocessing pipeline
	preprocResult := s.preprocessAggressive()
	if preprocResult != UNKNOWN {
		if s.verbose {
			s.printStats()
		}
		return preprocResult
	}

	// Initialize VSIDS with clause-length weighted activity BEFORE search
	// Variables in shorter clauses get higher activity (more constrained = more important)
	// Binary clauses get 100x base weight to strongly bias initial variable selection

	// Re-assign eliminated variables so VSIDS does not select them
	for _, varIdx := range s.eliminatedVars {
		s.assignments[varIdx] = Assignment{
			Value: s.varElimPolarity[varIdx],
			Level: 1,
		}
	}
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	// IMPROVEMENT #1: Improved VSIDS initialization after preprocessing
	// After VE, clause structure reflects remaining variable importance
	// Variables in more/shorter clauses are more constrained = higher activity
	for i := range s.assignments {
		if s.assignments[i].Level == 0 {
			// Count occurrences in remaining clauses
			occurrences := 0
			for _, clause := range s.cnf.Clauses {
				for _, lit := range clause.Literals {
					if lit.Var() == uint32(i) {
						occurrences++
						break
					}
				}
			}
			// Base activity + bonus for constrained variables
			s.vsids.activity[i] = 1.0 + float64(occurrences) * 0.5
		}
	}
	s.vsids.heapValid = false // Force heap rebuild

	for {
		s.iterations++
		if s.iterations%IterationReportInterval == 0 && s.verbose {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			fmt.Printf("c [debug] Iter %d, Conflicts %d, Level %d, Learned %d, Alloc=%dMB\n",
				s.iterations, s.conflicts, s.level, s.learnedActiveCount, mem.Alloc/1024/1024)
		}
		if s.maxIter > 0 && s.iterations > s.maxIter {
			if s.verbose {
				fmt.Printf("c [verbose] Iteration limit reached (%d)\n", s.maxIter)
				s.printStats()
			}
			return UNKNOWN
		}

		// CRITICAL FIX: Propagate unit learned clauses BEFORE normal propagation
		// This ensures unit clauses are propagated before any decisions are made
		unitConflict := false
		var unitConflictLit cnf.Literal
		for unitKey := range s.unitLearnedClauses {
			varIdx := unitKey & ^(uint32(1) << 31)
			isNegated := (unitKey & (1 << 31)) != 0
			value := !isNegated
			if s.assignments[varIdx].Level == 0 {
				s.assignments[varIdx] = Assignment{Value: value, Level: s.level}
				s.varLevel[varIdx] = s.level
				s.trail = append(s.trail, int(varIdx))
				lit := cnf.NewLiteral(varIdx, isNegated)
				s.implication[varIdx] = &cnf.Clause{Literals: []cnf.Literal{lit}, Learned: true}
				s.propagations++
			} else if s.assignments[varIdx].Value != value {
				// Conflict with existing assignment - this is a fundamental conflict
				unitConflict = true
				unitConflictLit = cnf.NewLiteral(varIdx, isNegated)
				break
			}
		}
		
		var conflict bool
		var conflictClause *cnf.Clause
		if unitConflict {
			conflict = true
			conflictClause = &cnf.Clause{Literals: []cnf.Literal{unitConflictLit}, Learned: true}
		} else {
			conflict, conflictClause = s.propagate()
		}
		if conflict {
			s.handleConflict(conflictClause)
			if s.conflicts%50 == 0 && s.verbose {
				propsPerDec := 0.0
				if s.decisions > 0 {
					propsPerDec = float64(s.propagations) / float64(s.decisions)
				}
				fmt.Printf("c [verbose] Conflict %d, level %d, learned %d, decisions %d, propagations %d, props/dec %.1f\n",
					s.conflicts, s.level, s.learnedActiveCount, s.decisions, s.propagations, propsPerDec)
			}
			if !s.backtrack() {
				if s.verbose {
					s.printStats()
				}
				return UNSAT
			}
			s.backjumpLevel = 0

			// Trigger inprocessing at configured interval
			// For small instances (< 100 vars), trigger earlier but not too frequently
			inprocessingInterval := s.inprocessingInterval
			if s.cnf.NumVars < 100 {
				inprocessingInterval = 50 // Trigger every 50 conflicts on small instances
			}
			if s.conflicts > 0 && s.conflicts%inprocessingInterval == 0 && s.cnf.NumClauses >= s.preprocessingMinClauses {
				if s.verbose {
					fmt.Printf("c [inprocess] Triggering inprocessing at conflict %d (interval=%d, vars=%d, clauses=%d)\n", s.conflicts, inprocessingInterval, s.cnf.NumVars, s.cnf.NumClauses)
				}
				s.inprocessing()
			}

			if s.shouldRestart() {
				s.restart()
			}

			continue
		}

		if s.allAssigned() {
			if s.verbose {
				s.printStats()
			}
			return SAT
		}

		if !s.decide() {
			if s.verbose {
				s.printStats()
			}
			return UNSAT
		}
	}
}

// SolveWithResult determines satisfiability with detailed result.
//
// Returns:
//   - SAT: Formula is satisfiable, use GetAssignments() to get model
//   - UNSAT: Formula is unsatisfiable
//   - UNKNOWN: Timeout, iteration limit, or inconclusive
//
// This is the primary solving method. It performs:
//  1. Unit propagation preprocessing
//  2. Watch initialization
//  3. VSIDS activity initialization
//  4. Main CDCL search loop with:
//     - Watched literals propagation
//     - 1-UIP conflict analysis
//     - Clause learning and deletion
//     - Adaptive restarts
//     - Inprocessing (on large instances)
//
// The solver maintains internal state; create a new solver for each formula.
func (s *CDCLSolver) SolveWithResult() SolveResult {
	// Unit propagation preprocessing - sound and safe
	// Repeatedly propagate unit clauses until fixpoint
	unitResult := s.unitPropagationPreprocess()
	if unitResult != UNKNOWN {
		if s.verbose {
			s.printStats()
		}
		return unitResult
	}

	// Rebuild literal pool after unit propagation modifies clauses
	s.cnf.RebuildLiteralPool()

	// Initialize watches after unit propagation
	s.initWatches()

	// Initialize VSIDS with clause-length weighted activity BEFORE search
	// Variables in shorter clauses get higher activity (more constrained = more important)
	// Binary clauses get 100x base weight to strongly bias initial variable selection
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	// IMPROVEMENT #1: Improved VSIDS initialization after preprocessing
	// After VE, clause structure reflects remaining variable importance
	// Variables in more/shorter clauses are more constrained = higher activity
	for i := range s.assignments {
		if s.assignments[i].Level == 0 {
			// Count occurrences in remaining clauses
			occurrences := 0
			for _, clause := range s.cnf.Clauses {
				for _, lit := range clause.Literals {
					if lit.Var() == uint32(i) {
						occurrences++
						break
					}
				}
			}
			// Base activity + bonus for constrained variables
			s.vsids.activity[i] = 1.0 + float64(occurrences) * 0.5
		}
	}
	s.vsids.heapValid = false // Force heap rebuild

	// Re-assign eliminated variables so VSIDS does not select them
	for _, varIdx := range s.eliminatedVars {
		s.assignments[varIdx] = Assignment{
			Value: s.varElimPolarity[varIdx],
			Level: 1,
		}
	}

	for {
		s.iterations++
		if s.iterations%IterationReportInterval == 0 && s.verbose {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			fmt.Printf("c [debug] Iter %d, Conflicts %d, Level %d, Learned %d, Alloc=%dMB\n",
				s.iterations, s.conflicts, s.level, s.learnedActiveCount, mem.Alloc/1024/1024)
		}
		if s.maxIter > 0 && s.iterations > s.maxIter {
			if s.verbose {
				fmt.Printf("c [verbose] Iteration limit reached (%d)\n", s.maxIter)
				s.printStats()
			}
			return UNKNOWN
		}

		conflict, conflictClause := s.propagate()
		if conflict {
			s.handleConflict(conflictClause)
			if s.conflicts%50 == 0 && s.verbose {
				propsPerDec := 0.0
				if s.decisions > 0 {
					propsPerDec = float64(s.propagations) / float64(s.decisions)
				}
				fmt.Printf("c [verbose] Conflict %d, level %d, learned %d, decisions %d, propagations %d, props/dec %.1f\n",
					s.conflicts, s.level, s.learnedActiveCount, s.decisions, s.propagations, propsPerDec)
			}
			if !s.backtrack() {
				if s.verbose {
					s.printStats()
				}
				return UNSAT
			}
			s.backjumpLevel = 0

			// Trigger inprocessing at configured interval
			// For small instances (< 100 vars), trigger earlier but not too frequently
			inprocessingInterval := s.inprocessingInterval
			if s.cnf.NumVars < 100 {
				inprocessingInterval = 50 // Trigger every 50 conflicts on small instances
			}
			if s.conflicts > 0 && s.conflicts%inprocessingInterval == 0 && s.cnf.NumClauses >= s.preprocessingMinClauses {
				if s.verbose {
					fmt.Printf("c [inprocess] Triggering inprocessing at conflict %d (interval=%d, vars=%d, clauses=%d)\n", s.conflicts, inprocessingInterval, s.cnf.NumVars, s.cnf.NumClauses)
				}
				s.inprocessing()
			}

			if s.shouldRestart() {
				s.restart()
			}
			continue
		}

		if s.allAssigned() {
			// Reconstruct eliminated variables before returning model
			s.reconstructEliminatedVars()

			// Verify model satisfies all clauses
			if !s.verifyModel() {
				if s.verbose {
					fmt.Printf("c [ERROR] Model verification failed - continuing search\n")
				}
				// Model is invalid - this shouldn't happen, indicates a bug
				// For now, return UNSAT to avoid returning wrong SAT
				return UNSAT
			}
			if s.verbose {
				s.printStats()
			}
			return SAT
		}

		if !s.decide() {
			if s.verbose {
				s.printStats()
			}
			return UNSAT
		}
	}
}

// GetAssignments returns the satisfying assignment if Solve() returned SAT.
//
// Returns a slice of Assignment structs where:
//   - assignments[i].Level > 0: Variable i+1 is assigned
//   - assignments[i].Value: The truth value (true/false)
//
// The model can be printed in DIMACS format:
//
//	model := solver.GetAssignments()
//	for i, assign := range model {
//	    if assign.Level > 0 {
//	        val := i + 1
//	        if !assign.Value {
//	            val = -(i + 1)
//	        }
//	        fmt.Printf("%d ", val)
//	    }
//	}
//	fmt.Println("0")
func (s *CDCLSolver) GetAssignments() []Assignment {
	return s.assignments
}

func (s *CDCLSolver) allAssigned() bool {
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			return false
		}
	}
	return true
}

// verifyModel checks if the current assignment satisfies all clauses
// Returns true if model is valid, false otherwise
func (s *CDCLSolver) verifyModel() bool {
	for _, clause := range s.cnf.Clauses {
		clauseSat := false
		for _, lit := range clause.Literals {
			assign := s.assignments[lit.Var()]
			litTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
			if litTrue {
				clauseSat = true
				break
			}
		}
		if !clauseSat {
			if s.verbose {
				fmt.Printf("c [ERROR] Clause not satisfied!\n")
			}
			return false
		}
	}
	return true
}

// propagateWatched performs unit propagation using watched literals
// Returns (conflict, conflictClauseIndex) where conflictClauseIndex is:
// - >= 0 for original clauses
// - < 0 for learned clauses (encoded as -learnedIdx-1)
//
// CRITICAL: Process ALL trail elements (trailIndex starts at 0), not just current level.
// Skipping trail elements from lower levels causes missed conflicts and unsoundness.
// propagateBinaryWatches performs unit propagation on binary clauses only
// BINARY CLAUSE OPTIMIZATION: Simplified logic for 2-literal clauses (no inner loop)
// Returns (conflict, conflictClause) if found
func (s *CDCLSolver) propagateBinaryWatches() (bool, *cnf.Clause) {
	if s.qhead >= len(s.trail) {
		return false, nil
	}

	for trailIndex := s.qhead; trailIndex < len(s.trail); trailIndex++ {
		lit := s.trail[trailIndex]

		varIdx := uint32(lit)
		value := s.assignments[varIdx].Value
		
		// Inline LitToIndex
		watchIdx := int(varIdx)<<1
		if value {
			watchIdx |= 1 // negated literal watches false when var is true
		}

		// Process binary clause watches - OPTIMIZED PATH (no inner loop!)
		watchList := s.watchListsBinary[watchIdx]

		for readIdx := 0; readIdx < len(watchList); readIdx++ {
			watch := watchList[readIdx]
			
			// Get the other literal in the binary clause (it's always at index 1-blitIndex)
			blitIdx := watch.Blit
			
			// Inline IndexToLit
			blitVarIdx := blitIdx >> 1
			blitNegated := (blitIdx & 1) != 0
			
			// Use varLevel cache
			blitLevel := s.varLevel[blitVarIdx]
			
			if blitLevel != 0 {
				blitValue := s.assignments[blitVarIdx].Value
				blitLitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)
				if blitLitTrue {
					continue // Other literal already true - clause satisfied
				}
			}

			// BINARY CLAUSE: No replacement search needed!
			// Only two literals: one is false (watchIdx), check if other (blitIdx) is unassigned
			if blitLevel == 0 {
				// Other literal is unassigned - propagate it
				blitLit := cnf.IndexToLit(int(blitIdx))
				
				// Get clause for conflict reporting
				var clause *cnf.Clause
				if watch.ClauseIdx >= 0 {
					clause = watch.Clause
				} else {
					learnedIdx := -watch.ClauseIdx - 1
					// SAFETY CHECK: Skip deleted clauses
					if learnedIdx >= len(s.learnedSizes) || s.learnedSizes[learnedIdx] == 0 {
						continue
					}
					literals := s.getLearnedClauseLiterals(learnedIdx)
					// CRITICAL FIX: Copy literals to avoid stale references when memory pool changes
					literalsCopy := make([]cnf.Literal, len(literals))
					copy(literalsCopy, literals)
					clause = &cnf.Clause{Literals: literalsCopy, Learned: true}
				}
				
				s.assignLiteralByClause(blitLit, s.level, clause)
				s.propagations++
				continue
			}

			// Both literals are false - CONFLICT!
			blitValue := s.assignments[blitVarIdx].Value
			blitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)

			if !blitTrue {

				// Return conflict clause
				if watch.ClauseIdx >= 0 {
					return true, watch.Clause
				} else {
					learnedIdx := -watch.ClauseIdx - 1
					// SAFETY CHECK: Skip deleted clauses
					if learnedIdx >= len(s.learnedSizes) || s.learnedSizes[learnedIdx] == 0 {
						continue
					}
					literals := s.getLearnedClauseLiterals(learnedIdx)
					literalsCopy := make([]cnf.Literal, len(literals))
					copy(literalsCopy, literals)
					return true, &cnf.Clause{Literals: literalsCopy, Learned: true}
				}
			}
		}

		s.watchListsBinary[watchIdx] = watchList
	}

	return false, nil
}

func (s *CDCLSolver) propagateWatched() (bool, *cnf.Clause) {
	if !s.watchInitialized {
		return s.propagate()
	}

	propagationCount := 0

	// CRITICAL FIX: Propagate unit learned clauses from dedicated map (never deleted)
	// Unit clauses are NOT watched and get lost from learned database, so we track them separately
	for unitKey := range s.unitLearnedClauses {
		varIdx := unitKey & ^(uint32(1) << 31)  // Clear polarity bit
		isNegated := (unitKey & (1 << 31)) != 0
		value := !isNegated
		
		
		
		if s.assignments[varIdx].Level == 0 {
			// Not assigned - propagate
			s.assignments[varIdx] = Assignment{Value: value, Level: s.level}
			s.varLevel[varIdx] = s.level
			s.trail = append(s.trail, int(varIdx))
			lit := cnf.NewLiteral(varIdx, isNegated)
			s.implication[varIdx] = &cnf.Clause{Literals: []cnf.Literal{lit}, Learned: true}
			propagationCount++
			s.propagations++
			
		} else if s.assignments[varIdx].Value != value {
			// Assigned opposite value - CONFLICT!
			
			lit := cnf.NewLiteral(varIdx, isNegated)
			return true, &cnf.Clause{Literals: []cnf.Literal{lit}, Learned: true}
		}
	}

	// Use persistent qhead pointer (MiniSat-style) to avoid re-processing trail elements
	if s.qhead >= len(s.trail) {
		return false, nil
	}

	// BINARY CLAUSE OPTIMIZATION: Process binary clauses first (faster path)
	if conflict, clause := s.propagateBinaryWatches(); conflict {
		return true, clause
	}

	for trailIndex := s.qhead; trailIndex < len(s.trail); trailIndex++ {
		lit := s.trail[trailIndex]

		varIdx := uint32(lit)
		value := s.assignments[varIdx].Value
		
		// OPTIMIZATION: Inline LitToIndex - avoids function call overhead
		// lit index = varIdx * 2 + (1 if negated else 0)
		watchIdx := int(varIdx)<<1
		if value {
			watchIdx |= 1 // negated literal watches false when var is true
		}

		// Process watches for this literal using swap-with-last deletion
		watchList := s.watchLists[watchIdx]

		for readIdx := 0; readIdx < len(watchList); readIdx++ {
			watch := watchList[readIdx]

			// Get current clause data - for learned clauses, use ClauseIdx to avoid stale pointer after swap-remove
			// P1 OPTIMIZATION: No deleted clause checks needed - watches removed immediately when clauses deleted
			var clause *cnf.Clause
			var clauseLits []cnf.Literal
			if watch.ClauseIdx >= 0 {
				clause = watch.Clause
				clauseLits = clause.Literals
			} else {
				learnedIdx := -watch.ClauseIdx - 1
				// SAFETY CHECK: Skip deleted clauses (shouldn't happen if watch removal works correctly)
				if learnedIdx >= len(s.learnedSizes) || s.learnedSizes[learnedIdx] == 0 {
					continue
				}
				clauseLits = s.getLearnedClauseLiterals(learnedIdx)
				clauseLitsCopy := make([]cnf.Literal, len(clauseLits))
				copy(clauseLitsCopy, clauseLits)
				clause = &cnf.Clause{Literals: clauseLitsCopy, Learned: true}
			}
			blitIdx := watch.Blit
			
			// Inline IndexToLit
			blitVarIdx := blitIdx >> 1
			blitNegated := (blitIdx & 1) != 0
			
			// OPTIMIZATION 1B: Use varLevel cache instead of assignments[].Level
			blitLevel := s.varLevel[blitVarIdx]
			
			if blitLevel != 0 {
				blitValue := s.assignments[blitVarIdx].Value
				blitLitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)
				if blitLitTrue {
					continue
				}
			}

			// Look for replacement watch
			foundReplacement := false
			// OPTIMIZATION 1C: Move loop-invariant computation outside inner loop
			// OPTIMIZATION 4: Pre-compute watchLitVar and blitLitVar as uint32 for fast comparison
			watchLitVar := uint32(watchIdx >> 1)
			blitLitVar := blitVarIdx
			// OPTIMIZATION 1A: Cache clause literals pointer to avoid repeated field access
			// clauseLits already set above
			for j := 0; j < len(clauseLits); j++ {
				clauseLit := clauseLits[j]
				clauseLitVar := clauseLit.Var()
				
				// Skip the watched literals themselves
				// OPTIMIZATION 4: Compare uint32 variables instead of full Literal type
				if clauseLitVar == watchLitVar || clauseLitVar == blitLitVar {
					continue
				}

				// OPTIMIZATION 1B: Use varLevel cache instead of assignments[].Level
				litLevel := s.varLevel[clauseLitVar]
				litValue := s.assignments[clauseLitVar].Value
				litNegated := clauseLit.IsNegated()
				litTrue := (!litNegated && litValue) || (litNegated && !litValue)

				if litTrue || litLevel == 0 {
					// Found replacement - move watch from falseLit to clauseLit
					// OPTIMIZATION: Inline LitToIndex
					newWatchIdx := int(clauseLitVar) << 1
					if litNegated {
						newWatchIdx |= 1
					}

					// Add new watch to clauseLit's watch list
					s.watchLists[newWatchIdx] = append(s.watchLists[newWatchIdx], cnf.Watch{
						Clause:    clause,
						ClauseIdx: watch.ClauseIdx,
						Blit:      blitIdx,
					})

					// Update symmetric watch Blit by scanning for ClauseIdx
					for symI := range s.watchLists[blitIdx] {
						if s.watchLists[blitIdx][symI].ClauseIdx == watch.ClauseIdx {
							s.watchLists[blitIdx][symI].Blit = uint32(newWatchIdx)
							break
						}
					}

					foundReplacement = true
					break
				}
			}

			if foundReplacement {
				// Watch moved successfully - remove old watch using swap-with-last
				lastIdx := len(watchList) - 1
				if readIdx != lastIdx {
					// Move last watch to current position
					watchList[readIdx] = watchList[lastIdx]
					// Update symmetric watch Blit by scanning for ClauseIdx
					movedWatch := watchList[readIdx]
					for symI := range s.watchLists[movedWatch.Blit] {
						if s.watchLists[movedWatch.Blit][symI].ClauseIdx == movedWatch.ClauseIdx {
							s.watchLists[movedWatch.Blit][symI].Blit = uint32(watchIdx)
							break
						}
					}
					// Don't increment readIdx - need to process the moved watch
					readIdx--
				}
				// Truncate (remove last element which is now duplicated)
				watchList = watchList[:lastIdx]
				s.watchLists[watchIdx] = watchList
				continue
			}

			// No replacement found - check if we can propagate or have conflict
			// Re-read blitLevel - may have changed during replacement search
			blitLevel = s.assignments[blitVarIdx].Level
			
			if blitLevel == 0 {
				blitLit := cnf.IndexToLit(int(blitIdx))
				s.assignLiteralByClause(blitLit, s.level, clause)
				propagationCount++
				s.propagations++
				continue
			}

			// Re-check blit value - may have been assigned TRUE during replacement search
			blitValue := s.assignments[blitVarIdx].Value
			blitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)

			if !blitTrue {
				if s.verbose {
					fmt.Printf("c [PROP CONFLICT] Watch idx=%d, clauseIdx=%d, learnedIdx=%d, blit=%d, level=%d\n",
						watchIdx, watch.ClauseIdx, -watch.ClauseIdx-1, watch.Blit, s.level)
					if watch.ClauseIdx < 0 {
						learnedIdx := -watch.ClauseIdx - 1
						if learnedIdx < len(s.learnedSizes) {
							fmt.Printf("c   Clause size=%d, LBD=%d\n", s.learnedSizes[learnedIdx], s.clauseLBD[learnedIdx])
						}
					}
					// Print conflict clause literals
					fmt.Printf("c   Conflict clause: ")
					for _, cl := range clause.Literals {
						fmt.Printf("%d%c ", cl.Var()+1, map[bool]byte{true:'-', false:'+'}[cl.IsNegated()])
					}
					fmt.Printf("\n")
				}
				s.watchLists[watchIdx] = watchList
				return true, clause
			}
		}

		// Store the modified watch list back
		s.watchLists[watchIdx] = watchList
	}

	// Update qhead to end of trail
	s.qhead = len(s.trail)

	return false, nil
}

func (s *CDCLSolver) propagate() (bool, *cnf.Clause) {
	// Use watched literals propagation if enabled
	if s.watchInitialized {
		return s.propagateWatched()
	}

	// Fallback to linear propagation
	trailIndex := s.trailHead[s.level]

	firstPass := true
	for firstPass || trailIndex < len(s.trail) {
		firstPass = false
		unitPropagated := false

		// Optimized propagation for original clauses using contiguous literal pool
		numOriginalClauses := s.cnf.NumOriginalClauses()
		for clauseIdx := 0; clauseIdx < numOriginalClauses; clauseIdx++ {
			clause := &s.cnf.Clauses[clauseIdx]
			offset, size := s.cnf.GetOriginalClauseInfo(clauseIdx)
			pool := s.cnf.GetLiteralPool()

			satisfiedCount := 0
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal

			// Inline literal iteration (avoids range overhead)
			for i := 0; i < size; i++ {
				lit := cnf.Literal(pool[offset+i])
				varIdx := lit.Var()
				litLevel := s.assignments[varIdx].Level
				if litLevel == 0 {
					unassignedCount++
					unassignedLit = lit
				} else {
					assign := s.assignments[varIdx]
					// Inlined literalIsTrue check (avoids function call)
					isTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
					if isTrue {
						satisfiedCount++
					} else {
						falseCount++
					}
				}
			}

			if satisfiedCount > 0 {
				continue
			}

			if unassignedCount == 0 && falseCount > 0 {
				return true, clause
			}

			if unassignedCount == 1 && falseCount == size-1 {
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				s.assignLiteralByClause(unassignedLit, assignLevel, clause)
				unitPropagated = true
				break
			}
		}

		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}

		// CRITICAL: Must check learned clauses during propagation!
		// Disabling this causes infinite loops: learned clauses don't prevent same conflict

		// Simple linear scanning of learned clauses (O(n) but correct)
		for learnedIdx := 0; learnedIdx < s.learnedCapacity; learnedIdx++ {
			if s.learnedSizes[learnedIdx] == 0 {
				continue // Skip tombstones
			}
			literals := s.getLearnedClauseLiterals(learnedIdx)
			clauseSize := s.learnedSizes[learnedIdx]

			satisfiedCount := 0
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal

			for _, lit := range literals {
				varIdx := lit.Var()
				litLevel := s.assignments[varIdx].Level
				if litLevel == 0 {
					unassignedCount++
					unassignedLit = lit
				} else {
					assign := s.assignments[varIdx]
					isTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
					if isTrue {
						satisfiedCount++
					} else {
						falseCount++
					}
				}
			}

			if satisfiedCount > 0 {
				continue
			}

			if unassignedCount == 0 && falseCount > 0 {
				tmpClause := &cnf.Clause{Literals: literals, Learned: true}
				return true, tmpClause
			}

			if unassignedCount == 1 && falseCount == clauseSize-1 {
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				tmpClause := &cnf.Clause{Literals: literals, Learned: true}
				s.assignLiteralByClause(unassignedLit, assignLevel, tmpClause)
				unitPropagated = true
				// Track propagation count for this learned clause
				if learnedIdx < len(s.clausePropCount) {
					s.clausePropCount[learnedIdx]++
				}
				break
			}
		}

		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}

		trailIndex++
	}

	return false, nil
}

// selectRandomUnassigned selects a random unassigned variable
// OPTIMIZATION: Uses persistent tmpUnassignedVars buffer to avoid allocation

// isEliminatedVar checks if a variable was eliminated during preprocessing
func (s *CDCLSolver) isEliminatedVar(varIdx uint32) bool {
	for _, elimVar := range s.eliminatedVars {
		if elimVar == varIdx {
			return true
		}
	}
	return false
}

func (s *CDCLSolver) selectRandomUnassigned() uint32 {
	// Reuse persistent buffer - no allocation!
	s.tmpUnassignedVars = s.tmpUnassignedVars[:0]
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			s.tmpUnassignedVars = append(s.tmpUnassignedVars, i)
		}
	}

	if len(s.tmpUnassignedVars) == 0 {
		return 0
	}

	// Use XORShift64 PRNG for deterministic random selection
	// Update seed: x ^= x << 13; x ^= x >> 7; x ^= x << 17
	seed := s.randomSeed
	seed ^= seed << 13
	seed ^= seed >> 7
	seed ^= seed << 17
	s.randomSeed = seed

	// Use lower bits to select index
	idx := int(seed % uint64(len(s.tmpUnassignedVars)))
	return s.tmpUnassignedVars[idx]
}

func (s *CDCLSolver) decide() bool {
	if !s.vsids.hasUnassigned(s.assignments, s.cnf.NumVars) {
		return false
	}

	// Track conflicts at current level
	if s.level > 0 && s.level < len(s.conflictsAtLevel) {
		s.conflictsAtLevel[s.level]++
	}

	// Determine if we should make a random decision
	// Two modes:
	// 1. Configurable random rate (s.randomDecisionRate) - only after 100 conflicts
	// 2. Diversification when stuck (existing logic)
	makeRandom := false

	// Check configurable random rate (only after initial search phase)
	if s.randomDecisionRate > 0.0 && s.conflicts >= 100 {
		if float64(s.conflicts%1000)/1000.0 < s.randomDecisionRate {
			makeRandom = true
		}
	}

	// Diversification: force random decision if severely stuck
	if !makeRandom {
		stuckThreshold := 1000 // conflicts at same level before forcing random
		forceRandom := false

		if s.level > 0 && s.level < len(s.conflictsAtLevel) && s.conflictsAtLevel[s.level] > stuckThreshold {
			// Severely stuck - force random decision
			if s.conflicts-s.lastRandomDecision > 500 { // At least 500 conflicts since last random
				forceRandom = true
			}
		}

		// Add periodic random decisions as fallback (configurable frequency, 0 = disabled)
		if !forceRandom && s.randomDecisionPeriod > 0 && s.conflicts > 0 && s.conflicts%s.randomDecisionPeriod == 0 {
			forceRandom = true
		}

		if forceRandom {
			makeRandom = true
		}
	}

	var varIdx uint32
	var phase bool

	if makeRandom {
		// Select random unassigned variable
		varIdx = s.selectRandomUnassigned()
		// Random phase
		phase = s.conflicts%2 == 0
		s.lastRandomDecision = s.conflicts

		// CRITICAL FIX: Skip variables with unit learned clauses even for random decisions
		posUnitKey := varIdx
		negUnitKey := varIdx | (1 << 31)
		if s.unitLearnedClauses[posUnitKey] || s.unitLearnedClauses[negUnitKey] {
			// Variable has a unit clause - find an alternative
			for i := range s.assignments {
				if i >= int(s.cnf.NumVars) {
					break
				}
				if s.assignments[i].Level == 0 && !s.isEliminatedVar(uint32(i)) {
					posU := uint32(i)
					negU := uint32(i) | (1 << 31)
					if !s.unitLearnedClauses[posU] && !s.unitLearnedClauses[negU] {
						varIdx = uint32(i)
						phase = s.conflicts%2 == 0
						break
					}
				}
			}
		}

		if s.verbose && s.conflicts%1000 == 0 {
			fmt.Printf("c [verbose] Random decision at conflict %d, level %d (rate=%.2f)\n", s.conflicts, s.level, s.randomDecisionRate)
		}

		// Reset conflicts at this level after random decision
		if s.level > 0 && s.level < len(s.conflictsAtLevel) {
			s.conflictsAtLevel[s.level] = 0
		}

		// Reset flip tracking
		s.consecutiveFlips = 0
	} else {
		// Select variable using VSIDS heuristic
		varIdx, phase = s.vsids.selectVariableWithPhase(s.assignments, s.savedPhase)
		
		// CRITICAL FIX: Skip variables with unit learned clauses
		// Deciding on such variables causes immediate conflicts
		posUnitKey := varIdx
		negUnitKey := varIdx | (1 << 31)
		if s.unitLearnedClauses[posUnitKey] || s.unitLearnedClauses[negUnitKey] {
			// Variable has a unit clause - find an alternative
			for i := range s.assignments {
				if i >= int(s.cnf.NumVars) {
					break
				}
				if s.assignments[i].Level == 0 && !s.isEliminatedVar(uint32(i)) {
					posU := uint32(i)
					negU := uint32(i) | (1 << 31)
					if !s.unitLearnedClauses[posU] && !s.unitLearnedClauses[negU] {
						varIdx = uint32(i)
						if int(varIdx) < len(s.savedPhase) {
							phase = s.savedPhase[varIdx]
						}
						break
					}
				}
			}
		}
		
		// IMPROVEMENT #3: Force exploration diversity when stuck
		// Only override VSIDS if same variable selected too many times
		if int(varIdx) < len(s.decidedVarSet) && s.decidedVarSet[varIdx] {
			s.restartDecisionCount++
			// Force alternative if same var selected > 10 times this restart
			if s.restartDecisionCount > 10 {
				for i := range s.assignments {
					if i >= int(s.cnf.NumVars) {
						break
					}
					if s.assignments[i].Level == 0 && !s.isEliminatedVar(uint32(i)) && !s.decidedVarSet[i] {
						varIdx = uint32(i)
						s.restartDecisionCount = 0
						break
					}
				}
			}
		} else {
			s.restartDecisionCount = 0
		}
		
		// Track this decision for diversity
		if int(varIdx) < len(s.decidedVarSet) && !s.decidedVarSet[varIdx] {
			s.decidedVarSet[varIdx] = true
			s.decidedVars = append(s.decidedVars, varIdx)
		}
		
		// Use saved phase from previous decisions (phase saving heuristic)
		// This remembers the polarity that worked well in previous search attempts
		if int(varIdx) < len(s.savedPhase) {
			phase = s.savedPhase[varIdx]
		} else {
			phase = true // Default to positive phase
		}

		// Detect variable flipping (same variable chosen consecutively)
		if s.conflicts > 0 && varIdx == s.lastDecisionVar {
			s.consecutiveFlips++

			// DISABLED: Aggressive diversification was counterproductive on structured instances
			// It resets VSIDS activity, preventing convergence on the right variables
			// Instead, let VSIDS naturally escape local minima through decay and restarts
		} else {
			s.consecutiveFlips = 0
		}
		s.lastDecisionVar = varIdx
	}

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, phase), s.level, nil)
	s.decisions++
	// SYMMETRY BREAKING: Track this decision to apply recency penalty
	s.vsids.TrackDecision(varIdx, s.conflicts)
	if s.verbose {
		fmt.Printf("c [DECIDE] Level %d (was %d): var %d = %v (decision), trailHead len=%d\n",
			s.level, s.level-1, varIdx+1, phase, len(s.trailHead))
	}
	return true
}

func (s *CDCLSolver) assignLiteral(lit cnf.Literal, level int, clause *cnf.Clause) {
	varIdx := lit.Var()

	if s.assignments[varIdx].Level != 0 {
		if s.verbose && level > 0 {
			reasonStr := "propagation"
			if clause == nil {
				reasonStr = "decision"
			}
			fmt.Printf("c [ALERT] assignLiteral: var %d already assigned at level %d, trying to assign at level %d (%s)\n",
				varIdx+1, s.assignments[varIdx].Level, level, reasonStr)
		}
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value: value,
		Level: level,
	}
	s.varLevel[varIdx] = level
	s.trail = append(s.trail, int(varIdx))
	s.implication[varIdx] = clause

	// Save the phase (polarity) for ALL assignments (phase saving heuristic)
	// OPTIMIZATION: Save phase for propagations too, not just decisions
	// This maintains consistent polarity patterns on structured instances (PHP, Tseitin)
	// Variables forced to same polarity by clauses will remember that polarity on re-decision
	// Standard in modern solvers (MiniSat, Glucose) - helps escape local minima after restarts
	s.savedPhase[varIdx] = value

	if s.verbose && level > 0 {
		reasonStr := "propagation"
		if clause == nil {
			reasonStr = "decision"
		}
		fmt.Printf("c [ASSIGN] Level %d: var %d = %v (%s)", level, varIdx+1, value, reasonStr)
		if clause != nil {
			if clause.Learned {
				fmt.Printf(" from learned clause")
			} else {
				fmt.Printf(" from original clause")
			}
		}
		fmt.Printf("\n")
	}
}

// assignLiteralByClause assigns a literal with a clause pointer as reason
// O(1) - just stores the pointer directly
func (s *CDCLSolver) assignLiteralByClause(lit cnf.Literal, level int, clause *cnf.Clause) {
	varIdx := lit.Var()

	if s.assignments[varIdx].Level != 0 {
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value: value,
		Level: level,
	}
	s.varLevel[varIdx] = level
	s.trail = append(s.trail, int(varIdx))

	// Store clause pointer directly - O(1), no lookup needed!
	s.implication[varIdx] = clause

	// Save phase for ALL assignments (consistent with assignLiteral)
	s.savedPhase[varIdx] = value
}

func (s *CDCLSolver) literalIsTrue(lit cnf.Literal) bool {
	assign := s.assignments[lit.Var()]
	if assign.Level == 0 {
		return false // Unassigned literals are not true
	}
	if lit.IsNegated() {
		return !assign.Value
	}
	return assign.Value
}

func (s *CDCLSolver) handleConflict(conflictClause *cnf.Clause) {
	s.conflicts++

	// Get the conflicting clause literals directly
	conflictLits := conflictClause.Literals

	// CRITICAL FIX: Handle unit clause conflicts specially
	// If the conflict clause is unit, we can't learn anything new
	// This happens when a unit learned clause conflicts with an existing decision
	// We must backtrack to the decision level and flip it
	if len(conflictLits) == 1 {
		// Unit clause conflict - backjump to the level where the variable was decided
		lit := conflictLits[0]
		varIdx := lit.Var()
		decisionLevel := s.assignments[varIdx].Level
		// Set backjump level to force flipping the decision at that level
		s.backjumpLevel = decisionLevel
		if s.verbose {
			fmt.Printf("c [handleConflict] Unit clause conflict on var %d at level %d - backjumping to flip\n",
				varIdx+1, decisionLevel)
		}
		// Decay VSIDS activity
		s.vsids.decay()
		s.vsids.decayLBD()
		if s.conflicts % 100 == 0 {
			for i := range s.clauseActivity {
				s.clauseActivity[i] *= ClauseActivityDecay
			}
		}
		return
	}

	// Bump activity for learned clause involved in conflict
	if conflictClause.Learned {
		// Find matching clause by comparing literals (expensive, so skip for now)
	}

	s.vsids.bumpClause(conflictLits)

	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	// Decay VSIDS activity every conflict (standard)
	s.vsids.decay()
	s.vsids.decayLBD()
	
	// OPTIMIZATION 2A: Lazy clause activity decay
	// Decay clause activity every 100 conflicts instead of every conflict
	// This reduces GC pressure and CPU overhead while maintaining search quality
	// Standard solvers (MiniSat, Glucose) use lazy decay for both variables and clauses
	if s.conflicts % 100 == 0 {
		for i := range s.clauseActivity {
			s.clauseActivity[i] *= ClauseActivityDecay
		}
	}

	// Inprocessing runs every 2000 conflicts on large instances (>500 clauses)
	// See inprocessing() method and solve loop integration
}

// learnClause performs 1-UIP conflict analysis to learn a new clause

// 1-UIP (First Unique Implication Point) Algorithm:
// The goal is to find the earliest point in the implication graph where the
// conflict can be explained with exactly one literal at the current decision level.

// Algorithm:
// 1. Start with the conflict clause (all literals are false)
// 2. While there is more than one literal at current level:
//    - Pick the most recently decided literal at current level
//    - Resolve with its reason clause (the clause that forced it)
//    - This eliminates the literal and adds the reason's literals
// 3. The result is the 1-UIP learned clause with exactly one literal at current level

// Why 1-UIP?
// - Produces shorter, more general learned clauses than other schemes
// - The UIP literal is the "bottleneck" through which all paths to conflict pass
// - Backjumping to the second-highest level in the learned clause is sound

// Example:
// Decision: x=1, y=1, z=1 (level 3)
// Propagate: ¬x∨¬y∨a, a=0 (level 3)
// Propagate: ¬a∨¬z∨b, b=0 (level 3)
// Conflict: ¬b∨¬z (both false at level 3)

// Resolution:
// Start: {b, z} (conflict clause)
// Resolve on b with reason (¬a∨¬z∨b): {z, ¬a, ¬z} = {¬a} (z cancels)
// Now only ¬a at level 3 - this is the 1-UIP!
// Learned clause: (a ∨ ¬z) - backjump to level of ¬z

// Backjump Level Calculation:
// The backjump level is the second-highest decision level in the learned clause.
// This is the highest level we can backjump to while still preventing the conflict.
// We backjump to this level and flip the decision there.

// LBD (Literal Block Distance):
// LBD = number of distinct decision levels in the learned clause.
// Lower LBD = better clause (involves fewer decision levels).
// Clauses with LBD=2 are "glue clauses" - most valuable, never delete.
func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) int {
	// Note: s.conflicts already incremented in handleConflict()

	if s.verbose && s.conflicts <= DebugConflictLimit {
		fmt.Printf("c [debug] Conflict %d, iter %d, level %d, learned %d, trail %d\n",
			s.conflicts, s.iterations, s.level, s.learnedActiveCount, len(s.trail))
	}

	// Fast cleanup from previous conflict: reset only touched variables (O(k) instead of O(n))
	for _, varIdx := range s.tmpTouchedVars {
		s.tmpLiteralInClause[varIdx] = false
		s.tmpLiteralIsNegated[varIdx] = false
	}
	// Clear only resolved variables (O(k) instead of O(n))
	for _, varIdx := range s.tmpResolvedVars {
		s.tmpResolved[varIdx] = false
	}
	s.tmpResolvedVars = s.tmpResolvedVars[:0]
	// Clear only used levels (O(k) instead of O(max_level))
	for _, lvl := range s.tmpLevelSet {
		s.tmpLevelCount[lvl] = 0
		s.tmpLevelSetUsed[lvl] = false
		s.tmpLevelCountUsed[lvl] = false
	}
	s.tmpTouchedVars = s.tmpTouchedVars[:0]
	s.tmpCandidates = s.tmpCandidates[:0]
	s.tmpLevelSet = s.tmpLevelSet[:0]

	// Ensure buffers are large enough for current level
	requiredSize := s.level + 1
	if requiredSize > len(s.tmpLevelCount) {
		s.tmpLevelCount = make([]int, requiredSize+1)
	}
	if requiredSize > len(s.tmpLevelSetUsed) {
		s.tmpLevelSetUsed = make([]bool, requiredSize+1)
	}

	// Add all literals from the conflicting clause
	if s.verbose {
		fmt.Printf("c [1-UIP] ===== Conflict %d: %d literals at level %d =====\n", s.conflicts, len(conflictLits), s.level)
		for i, lit := range conflictLits {
			fmt.Printf("c   INIT[%d]: var=%d, neg=%v, level=%d\n", i, lit.Var()+1, lit.IsNegated(), s.assignments[lit.Var()].Level)
		}
	}
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = true
			s.tmpLiteralIsNegated[varIdx] = lit.IsNegated()
			s.tmpTouchedVars = append(s.tmpTouchedVars, varIdx)
			lvl := s.assignments[varIdx].Level
			if lvl <= s.level {
				if !s.tmpLevelCountUsed[lvl] {
					s.tmpLevelCountUsed[lvl] = true
					s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				}
				s.tmpLevelCount[lvl]++
			}
		}
	}

	// 1-UIP: Resolve until exactly 1 literal remains at the current decision level
	// KEY FIX: Dynamically find candidates by scanning trail, not pre-computing
	// This ensures newly introduced literals at current level are also resolved

	currentSize := len(conflictLits)
	resolvedCount := 0

	// 1-UIP: Resolve until exactly 1 literal remains at current decision level
	// Uses MiniSat-style trail scanning (backwards from end) to respect temporal order
	// This ensures we resolve on the most recently assigned literal at each step,
	// which guarantees finding the true First Unique Implication Point (UIP).

	// Track which variables we've resolved on to prevent cycles
	for i := range s.tmpResolved {
		s.tmpResolved[i] = false
	}

	// OPTIMIZATION: Count current-level trail elements first to pre-allocate candidate list
	// This avoids reallocations during resolution when new literals are added
	currentLevelCount := 0
	for i := len(s.trail) - 1; i >= 0; i-- {
		if s.varLevel[uint32(s.trail[i])] == s.level {
			currentLevelCount++
		}
	}

	// Pre-allocate candidate list to exact size needed
	if cap(s.tmpCandidates) < currentLevelCount {
		s.tmpCandidates = make([]resolveCandidate, currentLevelCount)
	}
	s.tmpCandidates = s.tmpCandidates[:0]

	// Build list of trail positions at current level (in reverse trail order)
	// This avoids scanning lower-level trail elements on every resolution step
	// Only done once per conflict, saves O(trail_size) work per resolution
	for i := len(s.trail) - 1; i >= 0; i-- {
		if s.varLevel[uint32(s.trail[i])] == s.level {
			varIdx := uint32(s.trail[i])
			if s.tmpLiteralInClause[varIdx] {
				// Get reason clause size for potential sorting heuristics
				reasonClause := s.implication[varIdx]
				reasonSize := 0
				if reasonClause != nil {
					reasonSize = len(reasonClause.Literals)
				}
				s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{
					varIdx:     varIdx,
					trailPos:   i,
					reasonSize: reasonSize,
				})
			}
		}
	}

	// Process candidates in trail order (most recently assigned first)
	// This is the standard MiniSat approach and guarantees finding the 1-UIP correctly
	// Trail order respects the temporal sequence of implications
	//
	// OPTIMIZATION ATTEMPTED: Sort by reason clause size
	// Result: Made performance WORSE on PHP instances (6085 vs 2364 conflicts)
	// Reason: Trail order produces better 1-UIP clauses even if larger
	// The first UIP found via trail order leads to better backjumping
	//
	// Conclusion: Keep standard MiniSat trail order - it's already optimal
	candidateIdx := 0
	pathC := s.tmpLevelCount[s.level]

	for pathC > 1 && candidateIdx < len(s.tmpCandidates) {
		// Get next candidate from pre-filtered list
		candidate := s.tmpCandidates[candidateIdx]
		candidateIdx++
		varIdx := candidate.varIdx

		// Skip if already resolved
		if s.tmpResolved[varIdx] {
			continue
		}

		// Check if this literal has a reason (not a decision)
		if int(varIdx) >= len(s.implication) {
			break
		}
		reasonClause := s.implication[varIdx]
		if reasonClause == nil {
			// Decision literal - cannot resolve
			// CRITICAL FIX: If this is at current level, we can't achieve 1-UIP
			// Break and return what we have (will trigger 1-UIP WARNING)
			lvl := s.assignments[varIdx].Level
			if lvl == s.level {
	
				break
			}
			// Decision at lower level - skip but continue resolving
			continue
		}

		// Get reason clause literals directly from pointer
		reasonLits := reasonClause.Literals

		// Resolve: remove varIdx, add reason literals
		s.tmpLiteralInClause[varIdx] = false
		s.tmpResolved[varIdx] = true
		s.tmpResolvedVars = append(s.tmpResolvedVars, varIdx)
		s.tmpLevelCount[s.level]--
		pathC--

		// Add reason literals (except the one we resolved on)
		// CORRECT RESOLUTION: Handle polarity - opposite polarities cancel
		newLiterals := 0

		for _, lit := range reasonLits {
			v := lit.Var()
			if v == varIdx {
				continue
			}
			litNegated := lit.IsNegated()
			if s.tmpLiteralInClause[v] {
				// Variable already in clause - check polarity
				if s.tmpLiteralIsNegated[v] != litNegated {
					// Opposite polarity - they cancel! Remove from clause

					s.tmpLiteralInClause[v] = false
					// CRITICAL FIX: Don't reset tmpLiteralIsNegated - it's used to track polarity
					// when the variable is re-added. Actually, we should track cancelled state.
					s.tmpLevelCount[s.varLevel[v]]--
					if s.varLevel[v] == s.level {
						pathC--
					}
				}
				// Same polarity - do nothing (already in clause)
			} else {
				// Variable not in clause - add it
				s.tmpLiteralInClause[v] = true
				s.tmpLiteralIsNegated[v] = litNegated
				s.tmpTouchedVars = append(s.tmpTouchedVars, v)

				lvl := s.varLevel[v]
				if lvl <= s.level {
					if !s.tmpLevelCountUsed[lvl] {
						s.tmpLevelCountUsed[lvl] = true
						s.tmpLevelSet = append(s.tmpLevelSet, lvl)
					}
					s.tmpLevelCount[lvl]++
					if lvl == s.level {
						pathC++
						// CRITICAL FIX: Find trail position for this variable
						// 1-UIP requires processing in trail order (most recently assigned first)
						trailPos := -1
						for i := len(s.trail) - 1; i >= 0; i-- {
							if uint32(s.trail[i]) == v {
								trailPos = i
								break
							}
						}
						s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{
							varIdx:   v,
							trailPos: trailPos,
						})
					}
				}
				newLiterals++
			}
		}

		resolvedCount++
		currentSize = currentSize - 1 + newLiterals
		
		// CRITICAL FIX: Re-sort candidates by trail position after adding new literals
		// New literals from reason clauses may have been assigned earlier than remaining candidates
		// Processing in correct trail order is essential for finding the true 1-UIP
		if newLiterals > 0 && len(s.tmpCandidates) > candidateIdx+1 {
			// Sort remaining candidates by trail position (descending - most recent first)
			for i := candidateIdx; i < len(s.tmpCandidates); i++ {
				for j := i + 1; j < len(s.tmpCandidates); j++ {
					if s.tmpCandidates[j].trailPos > s.tmpCandidates[i].trailPos {
						s.tmpCandidates[i], s.tmpCandidates[j] = s.tmpCandidates[j], s.tmpCandidates[i]
					}
				}
			}
		}
	}

	// Calculate LBD BEFORE building learned clause (tmpLiteralInClause is cleared during build)
	lbd := 0
	maxLevel := 0
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			lvl := s.assignments[varIdx].Level
			if lvl >= 0 && !s.tmpLevelSetUsed[lvl] {
				s.tmpLevelSetUsed[lvl] = true
				s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				lbd++
			}
			if lvl > maxLevel && lvl < s.level {
				maxLevel = lvl
			}
		}
	}

	// Build the learned clause from remaining literals (using reusable buffer)
	s.tmpLearnedLits = s.tmpLearnedLits[:0] // Clear but keep capacity
	litsAtCurrentLevel := 0

	// CRITICAL: Clear tmpLiteralInClause as we add literals to prevent duplicates
	// tmpTouchedVars may have duplicate entries from multiple resolution steps
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = false // Clear to prevent duplicate
			lit := cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx])
			s.tmpLearnedLits = append(s.tmpLearnedLits, lit)
			lvl := s.assignments[varIdx].Level
			if lvl == s.level {
				litsAtCurrentLevel++
			}
		}
	}

	// CRITICAL: Check for empty learned clause (UNSAT)
	// This happens when 1-UIP analysis resolves away all literals

	if s.verbose {
		fmt.Printf("c   FINAL: %d literals, LBD=%d, litsAtCurrent=%d\n", len(s.tmpLearnedLits), lbd, litsAtCurrentLevel)
		if len(s.tmpLearnedLits) <= 10 {
			for i, lit := range s.tmpLearnedLits {
				fmt.Printf("c     [%d] var=%d%c level=%d\n", i, lit.Var()+1, map[bool]byte{true:'-', false:'+'}[lit.IsNegated()], s.assignments[lit.Var()].Level)
			}
		}
	}
	if len(s.tmpLearnedLits) == 0 {
		if s.verbose {
			fmt.Printf("c [learnClause] *** EMPTY CLAUSE at conflict %d - UNSAT ***\n", s.conflicts)
		}
		// Mark for immediate UNSAT detection
		s.emptyClauseFound = true
		return 0 // Will trigger UNSAT in backtrack
	}

	// If 1-UIP didn't reduce to exactly 1 literal at current level, handle it
	// This means 1-UIP analysis failed - still learn the clause but don't backjump based on it
	if litsAtCurrentLevel != 1 {
		if s.verbose {
			fmt.Printf("c [1-UIP WARNING] Failed to find UIP: %d literals at level %d (expected 1)\n", litsAtCurrentLevel, s.level)
		}
		// Return max level in clause for backjumping (standard backtracking)
		// CRITICAL FIX: If maxLevel is 0 (all literals at current level), use current level - 1
		bjLevel := maxLevel
		if bjLevel == 0 {
			bjLevel = s.level - 1
			if bjLevel < 1 {
				bjLevel = 1
			}
		}
		return bjLevel
	}

	// CLAUSE MINIMIZATION via self-subsumption
	// Try to remove literals from the learned clause by resolving with reason clauses
	// This produces smaller, more general learned clauses
	originalSize := len(s.tmpLearnedLits)
	if originalSize <= s.minimizationMaxSize && lbd <= s.minimizationMaxLBD {
		s.tmpLearnedLits = s.minimizeLearnedClause(s.tmpLearnedLits)
		if s.verbose && len(s.tmpLearnedLits) < originalSize {
			fmt.Printf("c [minimize] Clause reduced from %d to %d literals\n", originalSize, len(s.tmpLearnedLits))
		}
	}

	// Only learn non-empty clauses
	// TWO-TIER APPROACH: Separate glue clauses (LBD ≤ 3) from normal clauses
	// Glue clauses: watched, kept forever, high priority
	// Normal clauses: linear scan, deleted aggressively, keep only ~5K

	if s.verbose && s.conflicts <= DebugConflictLimit {
		fmt.Printf("c [debug] learnClause: conflict=%d, s.tmpLearnedLits=%d, lbd=%d\n", s.conflicts, len(s.tmpLearnedLits), lbd)
	}

	if len(s.tmpLearnedLits) > 0 {
		// QUALITY FILTER: Don't learn very high-LBD clauses (LBD > LowQualityLBDThreshold)
		// These clauses are too weak to be useful for propagation
		// They span too many decision levels and don't prune search effectively
		// Glucose standard: LBD ≤ 8, we match this for better clause database quality
		LowQualityLBDThreshold := 8
		if lbd > LowQualityLBDThreshold {
			if s.verbose && s.conflicts <= DebugConflictLimit {
				fmt.Printf("c [debug] Skipping low-quality clause: LBD=%d, size=%d (threshold: LBD<=8)\n", lbd, len(s.tmpLearnedLits))
			}
			// Use precomputed maxLevel from single pass above
			backjumpLevel := maxLevel
			if backjumpLevel == 0 {
				backjumpLevel = 1
			}
			return backjumpLevel
		}

		// DUPLICATE DETECTION: Skip if this clause already exists
		// OPTIMIZATION: Use hash table with canonical ordering for O(1) lookup
		// Canonical hash ensures A∨B and B∨A are detected as duplicates
		canonicalHash := computeCanonicalHash(s.tmpLearnedLits, s.tmpSortedLits)

		if s.learnedClauseHashes[canonicalHash] {
			// Don't learn this clause, but still return backjump level
			return s.level - 1
		}

		// Check if we need to delete clauses
		// Aggressive deletion: trigger when we have too many non-glue clauses
		maxNormalClauses := 1500  // Reduced from 2000 for tighter database

		// Immediate deletion trigger for high-LBD clauses
		if lbd > 10 && s.learnedActiveCount > 1000 {
			// Learned a very high-LBD clause and database is large - delete now
			s.deleteLearnedClauses()
		}

		if s.normalClauseCount >= maxNormalClauses && lbd > GlueLBDThreshold {
			// Delete oldest 50% of normal clauses (by age)
			if s.verbose {
				fmt.Printf("c [verbose] Deleting old normal clauses: %d normal clauses (limit %d)\n", s.normalClauseCount, maxNormalClauses)
			}

			// OPTIMIZATION: Use sort-based selection instead of O(n²) age ranking
			// Collect all normal clause indices with their ages
			type clauseAgeInfo struct {
				idx  int
				age  int
			}
			normalClauses := make([]clauseAgeInfo, 0, s.normalClauseCount)
			for i := 0; i < s.learnedCapacity; i++ {
				if s.learnedSizes[i] == 0 {
					continue // Skip deleted
				}
				if s.clauseLBD[i] <= 3 {
					continue // Skip glue clauses
				}
				normalClauses = append(normalClauses, clauseAgeInfo{
					idx: i,
					age: s.currentAge - s.clauseAge[i],
				})
			}
			
			// Sort by age (oldest first = highest age value)
			// Using simple insertion sort for small arrays, falls back to sort.Slice for larger
			if len(normalClauses) <= 32 {
				// Insertion sort for small arrays (faster than sort.Slice for n < 32)
				for i := 1; i < len(normalClauses); i++ {
					key := normalClauses[i]
					j := i - 1
					for j >= 0 && normalClauses[j].age < key.age {
						normalClauses[j+1] = normalClauses[j]
						j--
					}
					normalClauses[j+1] = key
				}
			} else {
				// Use sort.Slice for larger arrays
				sort.Slice(normalClauses, func(i, j int) bool {
					return normalClauses[i].age > normalClauses[j].age // Oldest first
				})
			}
			
			// Delete oldest 50% of normal clauses
			toDelete := len(normalClauses) / 2
			if toDelete > len(normalClauses) {
				toDelete = len(normalClauses)
			}
			
			deletedCount := 0
			for i := 0; i < toDelete; i++ {
				idx := normalClauses[i].idx
				
				// P1 OPTIMIZATION: Remove watches immediately when clause is deleted
				s.removeLearnedClauseWatches(idx)
				
				// Mark for deletion and track free literal slot
				offset := s.learnedOffsets[idx]
				size := s.learnedSizes[idx]
				s.literalFreeSlots = append(s.literalFreeSlots, literalFreeSlot{offset: offset, size: size})
				
				s.learnedSizes[idx] = 0
				s.clauseActivity[idx] = 0
				s.clauseAge[idx] = 0
				s.clauseLBD[idx] = 999999
				s.clauseUseCount[idx] = 0
				s.clausePropCount[idx] = 0
				deletedCount++
			}
			
			s.normalClauseCount -= deletedCount
			s.learnedActiveCount -= deletedCount
		}

		// Store learned clause literals in contiguous pool (reuse free slots if available)
		var offset int
		if len(s.literalFreeSlots) > 0 {
			// Reuse a free slot
			slot := s.literalFreeSlots[len(s.literalFreeSlots)-1]
			s.literalFreeSlots = s.literalFreeSlots[:len(s.literalFreeSlots)-1]
			offset = slot.offset
			// Truncate literals array to reuse this region
			if offset+len(s.tmpLearnedLits) > len(s.learnedLiterals) {
				s.learnedLiterals = append(s.learnedLiterals, make([]cnf.Literal, offset+len(s.tmpLearnedLits)-len(s.learnedLiterals))...)
			}
		} else {
			// Append to end
			offset = len(s.learnedLiterals)
			s.learnedLiterals = append(s.learnedLiterals, make([]cnf.Literal, len(s.tmpLearnedLits))...)
		}
		
		// Copy literals to the slot
copy(s.learnedLiterals[offset:offset+len(s.tmpLearnedLits)], s.tmpLearnedLits)
		
		// Append metadata (use swap-remove compatible approach)
		s.learnedOffsets = append(s.learnedOffsets, offset)
		s.learnedSizes = append(s.learnedSizes, len(s.tmpLearnedLits))
		s.clauseActivity = append(s.clauseActivity, 0.0)
		s.clauseAge = append(s.clauseAge, s.currentAge)
		s.clauseSize = append(s.clauseSize, len(s.tmpLearnedLits))
		s.clauseLBD = append(s.clauseLBD, lbd)
		s.clauseUseCount = append(s.clauseUseCount, 0)
		s.clausePropCount = append(s.clausePropCount, 0)
		if lbd > 3 {
			s.normalClauseCount++
		}
		s.currentAge++
		s.learnedActiveCount++
		s.learnedCapacity++

		// Get clause index and literals for watch addition
		learnedIdx := s.learnedActiveCount - 1
		literals := s.getLearnedClauseLiterals(learnedIdx)
		
		// Add learned clause to watches
		if s.watchInitialized {
			tmpClause := &cnf.Clause{Literals: literals, Learned: true}
			s.addLearnedClauseToWatches(learnedIdx, tmpClause, literals)
		}

		// OPTIMIZATION: Add canonical hash to hash table for O(1) duplicate detection
		s.learnedClauseHashes[canonicalHash] = true

		// DEBUG: Print learned clause
		if s.verbose {
			fmt.Printf("c [LEARNED #%d] LBD=%d, size=%d: ", s.conflicts, lbd, len(s.tmpLearnedLits))
			for _, lit := range s.tmpLearnedLits {
				fmt.Printf("%d%c ", lit.Var()+1, map[bool]byte{true:'-', false:'+'}[lit.IsNegated()])
			}
			fmt.Printf("\n")
		}

		// CRITICAL FIX: Track and propagate unit learned clauses immediately
		if len(s.tmpLearnedLits) == 1 {
			lit := s.tmpLearnedLits[0]
			// Store as var index with polarity bit (bit 31 = negated)
			unitKey := lit.Var()
			if lit.IsNegated() {
				unitKey |= (1 << 31)
			}
			s.unitLearnedClauses[unitKey] = true
			
			// IMMEDIATE PROPAGATION: Propagate unit clause now at backjump level
			// This prevents the solver from deciding the opposite value
			varIdx := lit.Var()
			value := !lit.IsNegated()
			if s.assignments[varIdx].Level == 0 {
				// Not assigned - propagate immediately at current level (will be backjump level after backtrack)
				s.assignments[varIdx] = Assignment{Value: value, Level: s.level}
				s.varLevel[varIdx] = s.level
				s.trail = append(s.trail, int(varIdx))
				s.implication[varIdx] = &cnf.Clause{Literals: s.tmpLearnedLits, Learned: true}
				s.propagations++
				if s.verbose {
					fmt.Printf("c [UNIT IMMEDIATE] Propagated %d%c at level %d\n",
						varIdx+1, map[bool]byte{true:'+', false:'-'}[value], s.level)
				}
			} else if s.assignments[varIdx].Value != value {
				// Already assigned opposite value - this is a conflict that should have been caught
				// This shouldn't happen if unit propagation is working correctly
				
			}
		}

		// Mark LBD order as dirty - will be rebuilt on next propagation
		s.lbdOrderDirty = true

		// Enforce maxLearned limit by deleting clauses when exceeded
		if s.learnedActiveCount > s.maxLearned {
			s.deleteLearnedClauses()
		}

		// LBD-based VSIDS: bump variables in low-LBD clauses
		s.vsids.bumpLBD(s.tmpLearnedLits, lbd)
	}

	// Calculate backjump level
	backjumpLevel := 0
	maxLevelInClause := 0
	for varIdx, inClause := range s.tmpLiteralInClause {
		if inClause {
			lvl := s.assignments[varIdx].Level
			if lvl > maxLevelInClause {
				maxLevelInClause = lvl
			}
			if lvl > backjumpLevel && lvl < s.level {
				backjumpLevel = lvl
			}
		}
	}

	// CRITICAL FIX: Handle unit clauses at current level
	// When the learned clause is unit (only one literal at current level),
	// there's no second-highest level to backjump to. We must flip the
	// decision at the current level, so backjump to current level.
	if backjumpLevel == 0 {
		if len(s.tmpLearnedLits) == 1 && s.assignments[s.tmpLearnedLits[0].Var()].Level == s.level {
			// Unit clause at current level - backjump to current level to flip decision
			backjumpLevel = s.level
		} else {
			backjumpLevel = 1
		}
	}

	if s.verbose && s.conflicts <= DebugConflictLimit {
		fmt.Printf("c [debug] Backjump level calculated: %d (max in clause: %d, current level: %d)\n",
			backjumpLevel, maxLevelInClause, s.level)
	}

	s.lastConflictLBD = lbd
	s.lbdSum += lbd
	s.lbdCount++

	return backjumpLevel
}

// deleteLearnedClauses removes low-quality learned clauses to control memory usage

// Clause Database Management Strategy:
// Learned clauses can grow unbounded, causing memory explosion and slowing down
// propagation. We use a quality-based deletion scheme that considers:

// 1. LBD (Literal Block Distance): PRIMARY QUALITY METRIC
//    - LBD = number of distinct decision levels in the clause
//    - Lower LBD = better clause (spans fewer decision levels)
//    - LBD=2: "Glue clauses" - most valuable, connect decision levels
//    - LBD=3: Very good clauses
//    - LBD>5: Usually not useful long-term

// 2. Age: SECONDARY FACTOR
//    - Old clauses may become irrelevant as search progresses
//    - Even good LBD clauses can become stale after hundreds of conflicts
//    - Force deletion of clauses older than 500 conflicts

// 3. Size: TERTIARY FACTOR
//    - Large clauses (>15 literals) are rarely useful
//    - Small clauses are more general and propagate more often
//    - Force deletion of clauses larger than 15 literals

// 4. Activity: PROTECTION FACTOR
//    - Clauses involved in recent conflicts are more relevant
//    - Activity decays over time (like VSIDS)
//    - High activity provides some protection against deletion

// Protection Rules (clauses never/ rarely deleted):
// - LBD=2 AND size≤4 AND age<100: Core glue, NEVER delete (score=-1000)
// - LBD=3 AND size≤3 AND age<50: Very good, protect unless very old (score=-500)

// Deletion Trigger:
// When learned clause count exceeds maxLearned (default 10,000), delete down to
// minLearned (default 5,000) - aggressive 50% reduction.

// Scoring Formula:
// score = age*10 + LBD*50 + size*5 - activity*20 + bonuses/penalties
// Higher score = more likely to delete

// minimizeLearnedClause reduces the size of a learned clause via self-subsumption
func (s *CDCLSolver) minimizeLearnedClause(learnedLits []cnf.Literal) []cnf.Literal {
	if len(learnedLits) <= 2 {
		return learnedLits
	}

	// Mark all literals in learned clause
	for i := range s.tmpLiteralInClause {
		s.tmpLiteralInClause[i] = false
	}
	for _, lit := range learnedLits {
		s.tmpLiteralInClause[lit.Var()] = true
	}

	// Single-pass minimization (multi-pass causes infinite loops)
	for _, lit := range learnedLits {
		varIdx := lit.Var()

		// Skip if already removed
		if !s.tmpLiteralInClause[varIdx] {
			continue
		}

		canRemove := false
		reasonClause := s.implication[varIdx]
		if reasonClause != nil {
			reasonLits := reasonClause.Literals

			if reasonLits != nil {
				// OPTIMIZATION: Skip large reason clauses (diminishing returns)
				if len(reasonLits) > s.minimizationMaxReasonSize {
					continue
				}

				// Check if all reason literals (except varIdx) are in the clause
				allCovered := true
				for _, reasonLit := range reasonLits {
					if reasonLit.Var() == varIdx {
						continue
					}
					if !s.tmpLiteralInClause[reasonLit.Var()] {
						allCovered = false
						break
					}
				}

				if allCovered && len(reasonLits) > 1 {
					canRemove = true
				}
			}
		}

		if canRemove {
			s.tmpLiteralInClause[varIdx] = false
		}
	}

	// Build final minimized clause from remaining literals
	minimized := make([]cnf.Literal, 0, len(learnedLits))
	for _, lit := range learnedLits {
		if s.tmpLiteralInClause[lit.Var()] {
			minimized = append(minimized, lit)
		}
	}

	return minimized
}

func (s *CDCLSolver) deleteLearnedClauses() {
	// LBD-based clause deletion using SWAP-REMOVE to avoid array rebuilding
	// This preserves memory pool benefits and eliminates allocations during deletion

	// Step 1: Score all active clauses
	clauses := make([]clauseInfo, 0, s.learnedActiveCount)

	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedSizes[i] == 0 {
			continue // Skip tombstones
		}
		lbd := s.clauseLBD[i]
		size := s.learnedSizes[i]
		age := s.currentAge - s.clauseAge[i]
		activity := s.clauseActivity[i]
		useCount := s.clauseUseCount[i]
		propCount := s.clausePropCount[i]

		// Calculate deletion score (higher = delete first)
		score := float64(lbd) * s.clauseDeletionLBDWeight
		score += float64(age) * s.clauseDeletionAgeWeight
		score += float64(size) * s.clauseDeletionSizeWeight
		score -= activity * s.clauseDeletionActivityWeight

		if useCount > s.clauseDeletionUseCountHigh {
			score -= float64(useCount) * 15.0
		} else if useCount > 0 {
			score -= float64(useCount) * 3.0
		}
		if propCount > s.clauseDeletionPropCountHigh {
			score -= float64(propCount) * 8.0
		} else if propCount > 0 {
			score -= float64(propCount) * 2.0
		}

		// Protection for glue clauses
		if lbd <= s.coreGlueLBDThreshold {
			score = -10000.0
			if size == 1 && s.verbose {
				fmt.Printf("c [DELETE DEBUG] Protecting unit clause at index %d with LBD=%d\n", i, lbd)
			}
		} else if lbd == s.coreGlueLBDThreshold+1 {
			score = -5000.0
		} else if lbd == s.coreGlueLBDThreshold+2 && size <= 5 {
			score = -1000.0
		}

		// Force deletion for high-LBD clauses
		if lbd > s.clauseDeletionHighLBD1 {
			score += s.clauseDeletionHighLBDBonus1
		}
		if lbd > s.clauseDeletionHighLBD2 {
			score += s.clauseDeletionHighLBDBonus2
		}

		clauses = append(clauses, clauseInfo{
			idx:       i,
			lbd:       lbd,
			size:      size,
			age:       age,
			activity:  activity,
			useCount:  useCount,
			propCount: propCount,
			score:     score,
		})
	}

	// Sort by score (descending - worst clauses first)
	sort.Sort(clauseInfoSlice(clauses))

	// Calculate how many to delete
	toKeep := int(float64(s.learnedActiveCount) * s.clauseDeletionKeepRatio)
	if toKeep < s.minLearned {
		toKeep = s.minLearned
	}
	if toKeep > s.learnedActiveCount {
		toKeep = s.learnedActiveCount
	}
	toDelete := len(clauses) - toKeep
	if toDelete <= 0 {
		return // Nothing to delete
	}

	// Step 2: Mark clauses for deletion and track free literal slots
	// CRITICAL FIX: Remove watches BEFORE marking as deleted
	deleted := make([]bool, s.learnedCapacity)
	for i := 0; i < toDelete; i++ {
		if clauses[i].score < 0 {
			break // Don't delete protected clauses
		}
		idx := clauses[i].idx
		deleted[idx] = true
		
		// CRITICAL: Remove watches for deleted clause (P1 lazy watch removal)
		s.removeLearnedClauseWatches(idx)
		
		// Track literal region as free for reuse
		offset := s.learnedOffsets[idx]
		size := s.learnedSizes[idx]
		s.literalFreeSlots = append(s.literalFreeSlots, literalFreeSlot{offset: offset, size: size})
	}

	// Step 3: Swap-remove - move active clauses into deleted slots
	// This avoids rebuilding arrays and preserves memory pool
	writeIdx := 0
	for readIdx := 0; readIdx < s.learnedCapacity; readIdx++ {
		if deleted[readIdx] {
			continue // Skip deleted slots
		}
		
		if writeIdx != readIdx {
			// Move clause metadata from readIdx to writeIdx
			s.learnedOffsets[writeIdx] = s.learnedOffsets[readIdx]
			s.learnedSizes[writeIdx] = s.learnedSizes[readIdx]
			s.clauseActivity[writeIdx] = s.clauseActivity[readIdx]
			s.clauseAge[writeIdx] = s.clauseAge[readIdx]
			s.clauseSize[writeIdx] = s.clauseSize[readIdx]
			s.clauseLBD[writeIdx] = s.clauseLBD[readIdx]
			s.clauseUseCount[writeIdx] = s.clauseUseCount[readIdx]
			s.clausePropCount[writeIdx] = s.clausePropCount[readIdx]
			
			// CRITICAL: Update all watches referencing this clause
			s.updateWatchClauseIndices(writeIdx, readIdx)
		}
		writeIdx++
	}

	// Step 4: Update active count and capacity
	s.learnedActiveCount = writeIdx
	s.learnedCapacity = writeIdx
	
	// Truncate metadata arrays (no reallocation, just update length)
	s.learnedOffsets = s.learnedOffsets[:writeIdx]
	s.learnedSizes = s.learnedSizes[:writeIdx]
	s.clauseActivity = s.clauseActivity[:writeIdx]
	s.clauseAge = s.clauseAge[:writeIdx]
	s.clauseSize = s.clauseSize[:writeIdx]
	s.clauseLBD = s.clauseLBD[:writeIdx]
	s.clauseUseCount = s.clauseUseCount[:writeIdx]
	s.clausePropCount = s.clausePropCount[:writeIdx]

	// Step 5: Rebuild hash table from remaining clauses
	s.learnedClauseHashes = make(map[uint64]bool, s.learnedActiveCount)
	for i := 0; i < s.learnedActiveCount; i++ {
		lits := s.getLearnedClauseLiterals(i)
		hash := computeCanonicalHash(lits, s.tmpSortedLits)
		s.learnedClauseHashes[hash] = true
	}

	// Mark LBD order as dirty
	s.lbdOrderDirty = true

	if s.verbose {
		fmt.Printf("c [verbose] Deleted %d learned clauses via swap-remove, kept %d\n", toDelete, s.learnedActiveCount)
	}
}

// updateWatchClauseIndices updates all watch references when a clause is moved from oldIdx to newIdx
// This is called during swap-remove in deleteLearnedClauses()
func (s *CDCLSolver) updateWatchClauseIndices(newIdx, oldIdx int) {
	// Scan all watch lists to find and update references
	// Watch stores ClauseIdx as negative for learned clauses: -learnedIdx-1
	oldClauseIdx := -oldIdx - 1
	newClauseIdx := -newIdx - 1
	
	for litIdx := range s.watchLists {
		watchList := s.watchLists[litIdx]
		for i := range watchList {
			if watchList[i].ClauseIdx == oldClauseIdx {
				watchList[i].ClauseIdx = newClauseIdx
			}
		}
	}
}

// backtrack backtracks (or backjumps) to a lower decision level
// Returns false if backtracking to level 0 (UNSAT)

// Backjumping vs Chronological Backtracking:
// Traditional DPLL backtracks one level at a time (chronological).
// CDCL solvers use backjumping (non-chronological backtracking) to skip
// irrelevant decision levels.

// How Backjumping Works:
// 1. After 1-UIP conflict analysis, the learned clause has exactly one literal
//    at the current decision level (the UIP - Unique Implication Point)
// 2. The backjump level is the second-highest level in the learned clause
// 3. Instead of backtracking to level-1, we jump directly to backjumpLevel
// 4. At backjumpLevel, we flip the decision that led to the conflict

// Why Backjumping is Sound:
// The learned clause explains why the conflict occurred. All literals in the
// learned clause except the UIP are already false at levels < current.
// By backjumping to the second-highest level and flipping that decision,
// we ensure the learned clause becomes unit and propagates the UIP literal
// to false, preventing the same conflict.

// Example:
// Decisions: x=1 (level 1), y=1 (level 2), z=1 (level 3)
// Conflict at level 3
// Learned clause: (¬x ∨ ¬y ∨ ¬z) with LBD=3 (levels 1,2,3)
// Backjump level = 2 (second-highest in learned clause)
// After backjump: trail = [x=1, y=0], z is unassigned
// The learned clause is now unit: ¬z is forced at level 2

// This skips exploring the entire subtree under (x=1, y=1) at level 2,
// which would all lead to the same conflict.
func (s *CDCLSolver) backtrack() bool {
	// Check for empty learned clause (UNSAT detected during 1-UIP analysis)
	if s.emptyClauseFound {
		if s.verbose {
			fmt.Printf("c [BACKTRACK] Empty clause found - returning UNSAT\n")
		}
		return false
	}

	if len(s.trailHead) <= 1 {
		if s.verbose {
			fmt.Printf("c [BACKTRACK] Returning false: trailHead len=%d\n", len(s.trailHead))
		}
		return false
	}

	// Use backjump level if available, otherwise backtrack one level
	bjLevel := s.backjumpLevel
	if bjLevel <= 0 {
		bjLevel = s.level - 1
	}
	// CRITICAL FIX: Allow bjLevel == s.level for unit clause flips
	// When bjLevel == s.level, we stay at current level and flip the decision
	// This is correct for unit learned clauses where the UIP is the decision literal
	// Only adjust if bjLevel > s.level (which would be a bug)
	if bjLevel > s.level {
		bjLevel = s.level - 1
	}
	if bjLevel < 1 {
		// Backjump level calculation failed - this indicates a problem with 1-UIP
		// For level > 1, something is wrong with 1-UIP - return UNSAT
		if s.verbose {
			fmt.Printf("c [BACKTRACK] bjLevel=%d invalid at level %d - returning UNSAT\n", bjLevel, s.level)
		}
		return false
	}

	// Find the decision point at the backjump level
	decisionPoint := s.trailHead[bjLevel]
	if decisionPoint >= len(s.trail) {
		if s.verbose {
			fmt.Printf("c [BACKTRACK] FAIL: decision point %d >= trail len %d at conflict %d\n", decisionPoint, len(s.trail), s.conflicts)
		}
		return false
	}

	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value

	if s.verbose && s.conflicts <= DebugConflictLimit {
		fmt.Printf("c [BACKTRACK] Backjumping from level %d to %d, var %d, trail[%d:%d]\n",
			s.level, bjLevel, decisionVar+1, decisionPoint, len(s.trail))
	}

	// Clear all assignments from decisionPoint onwards
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.varLevel[varIdx] = 0
		s.implication[varIdx] = nil
	}
	s.trail = s.trail[:decisionPoint]
	// Reset qhead to decisionPoint - the flipped decision needs to be propagated
	// and all subsequent trail elements have been cleared
	s.qhead = decisionPoint
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel

	// Reset conflicts at levels > bjLevel since we're backtracking
	for i := bjLevel + 1; i < len(s.conflictsAtLevel); i++ {
		s.conflictsAtLevel[i] = 0
	}

	// Flip the decision at the backjump level
	s.assignments[decisionVar] = Assignment{
		Value: !decisionValue,
		Level: bjLevel,
	}
	s.varLevel[decisionVar] = bjLevel
	s.trail = append(s.trail, int(decisionVar))

	// CRITICAL FIX: Update trailHead[bjLevel] to point to the flipped decision
	// Without this, 1-UIP analysis uses wrong trail range and learns duplicate clauses
	s.trailHead[bjLevel] = len(s.trail) - 1

	return true
}

// getReasonLBD returns the LBD of a variable's reason clause
// For original clauses: returns size (approximation, original clauses don't have LBD tracking)
// For learned clauses: returns stored LBD
// Used during 1-UIP analysis to prefer resolving with low-LBD reason clauses
func (s *CDCLSolver) getReasonLBD(varIdx uint32) int {
	reasonClause := s.implication[varIdx]
	if reasonClause == nil {
		return 0 // Decision, no reason
	}

	if reasonClause.Learned {
		// Find learned clause to get LBD by comparing literals
		reasonLits := reasonClause.Literals
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedSizes[i] != len(reasonLits) {
				continue
			}
			lits := s.getLearnedClauseLiterals(i)
			// Simple comparison - check if first literal matches (fast path)
			if len(lits) > 0 && len(reasonLits) > 0 && lits[0] == reasonLits[0] {
				if i < len(s.clauseLBD) {
					return s.clauseLBD[i]
				}
				break
			}
		}
		return 999
	}

	// Original clause - use size as approximation
	return len(reasonClause.Literals)
}

// rebuildLBDOrder rebuilds the learned clause order sorted by LBD (lowest first)
// Called periodically to prioritize glue clauses during propagation
func (s *CDCLSolver) rebuildLBDOrder() {
	n := s.learnedActiveCount
	if n == 0 {
		s.learnedClauseOrder = s.learnedClauseOrder[:0]
		s.lbdOrderDirty = false
		s.lbdOrderLastRebuild = s.conflicts
		return
	}

	// Ensure order slice has correct size
	if cap(s.learnedClauseOrder) < n {
		s.learnedClauseOrder = make([]int, n)
	}
	s.learnedClauseOrder = s.learnedClauseOrder[:n]

	// Initialize with sequential indices
	for i := 0; i < n; i++ {
		s.learnedClauseOrder[i] = i
	}

	// Sort by LBD (ascending - low LBD first)
	sort.Slice(s.learnedClauseOrder, func(i, j int) bool {
		return s.clauseLBD[s.learnedClauseOrder[i]] < s.clauseLBD[s.learnedClauseOrder[j]]
	})

	s.lbdOrderDirty = false
	s.lbdOrderLastRebuild = s.conflicts
}

// shouldRebuildLBDOrder returns true if the LBD order should be rebuilt
// Rebuild every 100 conflicts or after clause deletion
func (s *CDCLSolver) shouldRebuildLBDOrder() bool {
	if s.lbdOrderDirty {
		return true
	}
	// Rebuild periodically to account for new clauses with different LBD
	return s.conflicts-s.lbdOrderLastRebuild >= 100
}

// SolveDPLL solves using plain DPLL algorithm (no clause learning, no CDCL)
// This is useful for comparison and debugging
func (s *CDCLSolver) SolveDPLL() SolveResult {
	if s.verbose {
		fmt.Printf("c Using plain DPLL algorithm (no clause learning)\n")
	}

	// Create a simple DPLL solver
	dpll := NewSolver(s.cnf)

	// Run DPLL
	if dpll.Solve() {
		// Copy DPLL assignments to CDCL solver for model extraction
		for i := range dpll.assignments {
			s.assignments[i] = dpll.assignments[i]
		}
		return SAT
	}
	return UNSAT
}

// PropagateBenchmark exposes propagate for benchmarking
func (s *CDCLSolver) PropagateBenchmark() {
	s.propagate()
}

// Trail returns the trail for benchmarking
func (s *CDCLSolver) Trail() []int {
	return s.trail
}

// ResetTrail resets the trail for benchmarking
func (s *CDCLSolver) ResetTrail() {
	s.trail = s.trail[:0]
	s.level = 0
	for i := range s.assignments {
		s.assignments[i].Level = 0
	}
}
