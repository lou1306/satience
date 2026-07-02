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
	// Base clause database size - scaled with instance size by calculateMaxLearned()
	DefaultMaxLearnedBase   = 2000  // Base clause database limit
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

// calculateMaxLearned scales the clause database limit with instance size.
// MiniSat-style: base limit proportional to variables, grows with conflicts
func calculateMaxLearned(numVars uint32, numClauses int) int {
	// DYNAMIC LIMIT: Based on both variables AND clauses
	// Key insight: Learned clause database should scale with instance size
	// - Small instances (<1000 clauses): Need room to learn (min 300)
	// - Medium instances: 10-20% of original clause count
	// - Large instances: Cap to prevent memory explosion
	
	// Base: percentage of original clauses (primary factor)
	baseLimit := int(float64(numClauses) * 0.15)  // 15% of original clauses
	
	// Also consider variable count (secondary factor)
	varLimit := int(numVars) * 6
	if varLimit > baseLimit {
		baseLimit = varLimit
	}
	
	// Scale with density for very sparse/dense instances
	if numVars > 0 {
		density := float64(numClauses) / float64(numVars)
		if density > 10.0 {
			// Dense instance: can handle more learned clauses
			baseLimit = int(float64(baseLimit) * 1.3)
		} else if density < 3.0 {
			// Sparse instance: be conservative
			baseLimit = int(float64(baseLimit) * 0.8)
		}
	}
	
	// Absolute bounds
	if baseLimit < 300 {
		baseLimit = 300  // Minimum for tiny instances
	}
	if baseLimit > 100000 {
		baseLimit = 100000
	}

	return baseLimit
}

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

type CDCLSolver struct {
	cnf          *cnf.CNF
	assignments  []Assignment
	trail        []int
	varLevel     []int // Cache of variable levels (avoids random assignments[].Level access)
	trailHead    []int
	level        int
	vsids        *VSIDS
	conflicts    int
	implication  []int // Reason clause: >=0 original; <=-5 learned (-learnedIdx-5); -1 decision; -2 unit-prop preprocess; -3 pure-literal preprocess; -4 reserved
	iterations   int
	propagations int // Total propagations (assignments by unit propagation)
	maxIter      int
	// Memory pool for learned clauses - contiguous literal storage to eliminate per-clause allocations
	learnedLiterals      []cnf.Literal         // All learned clause literals in one contiguous slice
	learnedOffsets       []int                 // Start offset in learnedLiterals for each clause
	learnedSizes         []int                 // Number of literals in each clause (0 = deleted/tombstone)
	learnedMetadata      []cnf.ClauseMetadata  // OPTIMIZATION: Packed metadata (LBD, age, activity, useCount, propCount, score)
	learnedWatchIdx0     []int                 // First watched literal index (for fast watch removal)
	learnedWatchIdx1     []int                 // Second watched literal index (for fast watch removal)
	normalClauseCount    int                   // Track number of non-glue clauses (LBD > 3)
	learnedActiveCount   int                   // Number of active clauses (excludes tombstones)
	learnedCapacity      int                   // Total capacity including tombstones
	currentAge           int
	verbose              bool
	debugVerify          bool      // Enable expensive clause verification (debug builds)
	decisions            int
	backjumpLevel        int
	maxLearned           int
	minLearned           int // Minimum clauses to keep (aggressive deletion target)
	savedPhase           []bool
	restartBase          int
	restartCount         int
	lubyIndex            int
	lbdSum               int
	lbdCount             int
	lastConflictLBD      int
	conflictsAtLevel     []int           // Track conflicts per decision level
	lastRandomDecision   int             // Last conflict where we made random decision
	randomDecisionRate   float64         // Probability of making a random decision (0.0 = never, 1.0 = always)
	unitLearnedClauses   map[uint32]bool // Map of variables with unit learned clauses (bit 31 = polarity)
	randomDecisionPeriod int             // Period for forced random decisions (default 0=disabled, causes O(n) overhead)
	randomSeed           uint64          // Seed for deterministic random selection
	lastDecisionVar      uint32          // Last variable chosen for decision
	consecutiveFlips     int             // Count of consecutive decisions on same variable
	unitLearnedList      []int           // List of learned clause indices that are unit clauses (for O(1) propagation)
	// Exploration diversity tracking (IMPROVEMENT #3)
	decidedVars               []uint32 // Variables decided during current search phase
	decidedVarSet             []bool   // Fast lookup for decided variables
	restartDecisionCount      int      // Decisions since last restart (for diversity reset)
	minimizationMaxSize       int      // Skip minimization for clauses > this size (0=all)
	minimizationMaxLBD        int      // Skip minimization for clauses with LBD > this (0=all)
	minimizationMaxReasonSize int      // Skip resolution with reason clauses > this size
	// Reusable buffers for conflict analysis (avoid per-conflict allocation)
	tmpLiteralInClause   []bool
	tmpSeenVar           []bool // Pre-allocated bitset for duplicate/tautology checks (replaces per-conflict maps)
	tmpLiteralIsNegated  []bool
	tmpLevelCount        []int
	tmpLevelCountUsed    []bool // Track which levels have non-zero tmpLevelCount
	tmpCandidates        []resolveCandidate
	tmpLevelSet          []int         // For LBD calculation (replaces map)
	tmpLevelSetUsed      []bool        // Track which levels are in tmpLevelSet
	tmpResolved          []bool        // Track resolved variables in 1-UIP to prevent cycles
	tmpResolvedVars      []uint32      // Track which variables were resolved (for fast reset)
	tmpClauseHash        uint64        // Hash for duplicate detection
	tmpFlippedVars       []bool        // Track flipped variables at level 1 to prevent infinite loops
	tmpTouchedVars       []uint32      // Track which variables were modified (for fast reset)
	tmpUnassignedVars    []uint32      // Reusable buffer for random variable selection (avoids allocation)
	tmpLearnedLits       []cnf.Literal // Reusable buffer for learned clause literals
	tmpSortedLits        []cnf.Literal // Temporary buffer for canonical clause sorting
	tmpMinimizedLits     []cnf.Literal // Reusable buffer for clause minimization (avoids allocation)
	conflictClauseBuf   cnf.Clause    // Pre-allocated conflict clause (avoids per-conflict heap alloc)
	conflictLitsBuf     []cnf.Literal // Pre-allocated buffer for conflict clause literal copies
	tmpIsGlue            []bool        // Bitmap for glue clause selection during restart (avoids allocation)
	tmpHasPositive       []bool        // Reusable buffer for pure literal detection in inprocessing
	tmpHasNegative       []bool        // Reusable buffer for pure literal detection in inprocessing

	// Reusable buffers for clause deletion (avoid per-deletion allocation)
	tmpClauseInfo         []clauseInfo         // Buffer for clause scoring
	tmpDeleted            []bool               // Bitmap for deleted clauses
	tmpClauseUsedAsReason []bool               // Track clauses used as implications
	tmpDeletionOffsets    []int                // Pre-allocated buffer for new offsets during deletion
	tmpDeletionSizes      []int                // Pre-allocated buffer for new sizes during deletion
	tmpDeletionMetadata   []cnf.ClauseMetadata // Pre-allocated buffer for new metadata during deletion
	tmpDeletionLiterals   []cnf.Literal        // Pre-allocated buffer for new literals during deletion
	tmpClauseIndexMap     []int                // Pre-allocated buffer for old->new clause index mapping
	tmpKeepIndices        []int                // Pre-allocated buffer for indices of clauses to keep

	// Watched literals infrastructure
	watchLists        [][]cnf.Watch // watchLists[lit] = clauses watching lit
	watchInitialized  bool          // True if watches have been initialized
	learnedClauseBase int           // Base ID for learned clause watches (fixed at initialization)

	// LBD-based learned clause ordering for propagation prioritization
	learnedClauseOrder []int // Indices into learnedClauses/clauseLBD sorted by LBD

	// Variable elimination tracking for model reconstruction
	emptyClauseFound    bool                     // Set when empty learned clause derived (UNSAT)
	lbdOrderDirty       bool                     // True if order needs rebuilding
	lbdOrderLastRebuild int                      // Conflict count when order was last rebuilt
	compactPending      bool                     // Set when learned-clause tombstone ratio is high; compaction runs at the next restart (level 0)

	qhead int // Watched literals: next trail index to process

	// Configurable parameters (exposed for tuning)
	preprocessingMinClauses  int     // Skip preprocessing if < N clauses (default 50)
	preprocessingMaxVars     int     // Skip preprocessing if > N vars (default 50000)
	preprocessingMaxClauses  int     // Skip preprocessing if > N clauses (default 500000)
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
	restartGlucoseRatio        float64 // Glucose-style restart when LBD > ratio × avg (default 1.5)
	restartGlucoseMinConflicts int     // Min conflicts before Glucose restarts kick in (default 50)
	restartKeepGlueLBD         int     // Keep clauses with LBD ≤ this during restart (default 3)
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
	maxLearned := calculateMaxLearned(formula.NumVars, formula.NumClauses)
	minLearned := maxLearned / 2
	restartBase := DefaultRestartBase

	// Ensure literal pool is built for efficient propagation
	formula.RebuildLiteralPool()

	solver := &CDCLSolver{
		cnf:                  formula,
		assignments:          make([]Assignment, formula.NumVars),
		trail:                make([]int, 0, formula.NumVars),
		varLevel:             make([]int, formula.NumVars),
		trailHead:            make([]int, 1),
		qhead:                0,
		level:                0,
		vsids:                NewVSIDS(formula.NumVars),
		conflicts:            0,
		implication:          make([]int, formula.NumVars), // -1 = decision (no clause)
		iterations:           0,
		maxIter:              0,
		// P0: Pre-allocate learned clause arrays with generous capacity to avoid growth
		// learnedLiterals: 8 literals per clause average (covers most learned clauses)
		learnedLiterals:      make([]cnf.Literal, 0, maxLearned*8),
		learnedOffsets:       make([]int, 0, maxLearned),
		learnedSizes:         make([]int, 0, maxLearned),
		learnedMetadata:      make([]cnf.ClauseMetadata, 0, maxLearned), // Packed metadata
		learnedWatchIdx0:     make([]int, 0, maxLearned), // Watched literal indices
		learnedWatchIdx1:     make([]int, 0, maxLearned),
		learnedActiveCount:   0,
		learnedCapacity:      0,
		unitLearnedList:      make([]int, 0, 64), // Pre-allocate for unit clause tracking
		currentAge:           0,
		verbose:              false,
		decisions:            0,
		backjumpLevel:        0,
		maxLearned:           maxLearned,
		minLearned:           minLearned,
		savedPhase:           make([]bool, formula.NumVars),
		restartBase:          restartBase,
		restartCount:         0,
		lubyIndex:            0,
		lbdSum:               0,
		lbdCount:             0,
		randomDecisionRate:   0.0,
		randomDecisionPeriod: 0, // Default: DISABLED (causes 65% overhead with no measurable benefit)
		randomSeed:           0,
		learnedClauseOrder:   make([]int, 0),
		lbdOrderDirty:        true,
		lbdOrderLastRebuild:  0,
		lastConflictLBD:      0,
		conflictsAtLevel:     make([]int, formula.NumVars+1),
		decidedVars:          make([]uint32, 0, formula.NumVars),
		decidedVarSet:        make([]bool, formula.NumVars),
		restartDecisionCount: 0,
		lastRandomDecision:   -1000,
		// P1: Pre-allocate reusable buffers with generous capacity to avoid reallocation
		tmpLiteralInClause:   make([]bool, formula.NumVars),
		tmpSeenVar:           make([]bool, formula.NumVars),
		tmpLiteralIsNegated:  make([]bool, formula.NumVars),
		tmpLevelCount:        make([]int, formula.NumVars+1),
		tmpLevelCountUsed:    make([]bool, formula.NumVars+1),
		tmpCandidates:        make([]resolveCandidate, 0, 200),  // Increased from 100
		tmpLevelSet:          make([]int, 0, formula.NumVars),
		tmpLevelSetUsed:      make([]bool, formula.NumVars+1),
		tmpResolved:          make([]bool, formula.NumVars),
		tmpResolvedVars:      make([]uint32, 0, formula.NumVars),
		tmpFlippedVars:       make([]bool, formula.NumVars),
		tmpTouchedVars:       make([]uint32, 0, formula.NumVars),
		tmpUnassignedVars:    make([]uint32, 0, formula.NumVars),
		// P1: Increased buffer capacity from 64 to 256 to handle larger learned clauses
		tmpLearnedLits:       make([]cnf.Literal, 0, 256),
		tmpSortedLits:        make([]cnf.Literal, 0, 256),
		tmpMinimizedLits:     make([]cnf.Literal, 0, 256),
		conflictLitsBuf:     make([]cnf.Literal, 0, 256),
		tmpIsGlue:            make([]bool, maxLearned),
		tmpHasPositive:       make([]bool, formula.NumVars),
		tmpHasNegative:       make([]bool, formula.NumVars),
		// Clause deletion buffers - pre-allocate to maxLearned to avoid reallocation
		tmpClauseInfo:         make([]clauseInfo, 0, maxLearned),
		tmpDeleted:            make([]bool, maxLearned),
		tmpClauseUsedAsReason: make([]bool, maxLearned),
		tmpDeletionOffsets:    make([]int, 0, maxLearned),
		tmpDeletionSizes:      make([]int, 0, maxLearned),
		tmpDeletionMetadata:   make([]cnf.ClauseMetadata, 0, maxLearned),
		tmpDeletionLiterals:   make([]cnf.Literal, 0, maxLearned*4),
		tmpClauseIndexMap:     make([]int, maxLearned),
		tmpKeepIndices:        make([]int, 0, maxLearned),
		learnedClauseBase:     int(formula.NumClauses),
		// Minimization thresholds - aggressive for better clause quality
		minimizationMaxSize:       0, // Always minimize (no size limit)
		minimizationMaxLBD:        0, // Always minimize (no LBD limit)
		minimizationMaxReasonSize: 0, // Always try all reason clauses
		// Configurable parameters with defaults
		preprocessingMinClauses:  10,
		preprocessingMaxVars:     50000,
		preprocessingMaxClauses:  500000,
		clauseDeletionMinLBD:     3,
		glueClauseLBDThreshold:   2,
		coreGlueLBDThreshold:     2,
		largeClauseSizeThreshold: 15,
		maxClauseAgeThreshold:    500,
		clauseActivityDecay:      0.95,
		tmpCandidateBufferSize:   100,
		tmpLearnedLitBufferSize:  64,
		learnedClauseHashInitial: 2500,
		// Restart policy defaults (aggressive Glucose-style for better performance)
		restartGlucoseRatio:        1.5, // Standard Glucose value (aggressive restarts)
		restartGlucoseMinConflicts: 50,  // Start Glucose restarts early
		restartKeepGlueLBD:         3,   // Keep LBD≤3 glue clauses
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

	// FIX: Initialize all assignments as unassigned (Level=-1)
	for i := range solver.assignments {
		solver.assignments[i] = Assignment{Level: -1}
		solver.varLevel[i] = -1
	}

	// Initialize savedPhase to true (default positive phase)
	for i := range solver.savedPhase {
		solver.savedPhase[i] = true
	}

	// Enable LBD-based VSIDS for better variable selection
	// Variables in low-LBD clauses get higher priority
	solver.vsids.EnableLBD()

	// TUNE VSIDS parameters based on instance size
	// Small instances need faster decay to quickly identify important variables
	if formula.NumVars < 1000 {
		// Faster decay (0.90 vs 0.95) = quicker adaptation to search progress
		// Lower initial decay means activity drops faster, focusing on recent conflicts
		solver.vsids.SetInitialDecayFactor(0.90)
		// Faster ramp-up to max decay (5000 vs 10000 conflicts)
		// Stay in aggressive exploration phase longer
		solver.vsids.SetDecayRampUpConflicts(5000)
		// Moderate bump amount - large bumps can cause activity inflation
		solver.vsids.SetBaseBumpAmount(25.0)
		// Moderate LBD bonus - too high causes over-prioritization of glue clauses
		solver.vsids.SetLBDBonusScale(2000.0)
		// More aggressive clause minimization for small instances
		solver.minimizationMaxReasonSize = 50 // Allow larger reason clauses for more minimization
		// More aggressive restarts for small instances
		// Luby base: 100 → 10 (restart 10× more frequently)
		solver.restartBase = 10
		// Glucose restart: start earlier and more aggressive
		solver.restartGlucoseRatio = 1.2 // Restart when LBD > 1.2× avg (very aggressive)
		solver.restartGlucoseMinConflicts = 20 // Start adaptive restarts after 20 conflicts
	}

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







// SetClauseActivityDecay sets the clause activity decay factor (default 0.95)
func (s *CDCLSolver) SetClauseActivityDecay(decay float64) {
	if decay < 0.5 || decay > 1.0 {
		decay = 0.95
	}
	s.clauseActivityDecay = decay
}



// SetDecayInterval sets the VSIDS decay interval (conflicts between activity decays)
// Higher values = fewer heap rebuilds but slower activity differentiation
// Lower values = more frequent decay but more heap rebuilds
// Default is 10, which provides good balance for most instances
func (s *CDCLSolver) SetDecayInterval(interval int) {
	s.vsids.SetDecayInterval(interval)
}

// SetInitialDecay sets the VSIDS initial decay factor (default 0.95)
// Lower values = more aggressive decay = more exploration
func (s *CDCLSolver) SetInitialDecay(factor float64) {
	s.vsids.SetInitialDecayFactor(factor)
}

// SetMaxDecay sets the VSIDS maximum decay factor (default 0.999)
// Higher values = slower decay = more focused search on important variables
func (s *CDCLSolver) SetMaxDecay(factor float64) {
	s.vsids.SetMaxDecayFactor(factor)
}

// SetDecayRampup sets the number of conflicts to reach max decay (default 10000)
func (s *CDCLSolver) SetDecayRampup(conflicts int) {
	s.vsids.SetDecayRampUpConflicts(conflicts)
}

// SetLBDBonusScale sets the LBD bonus scale for VSIDS (default 2000.0)
// Higher values = stronger preference for low-LBD (glue) clauses
func (s *CDCLSolver) SetLBDBonusScale(scale float64) {
	s.vsids.SetLBDBonusScale(scale)
}

// SetBumpAmount sets the base bump amount for conflicts (default 50.0)
// Higher values = more aggressive activity increase for conflict variables
func (s *CDCLSolver) SetBumpAmount(amount float64) {
	s.vsids.SetBaseBumpAmount(amount)
}

// SetClauseInitWeights sets the clause initialization weights
// baseWeight: base weight for all clauses (default 10.0)
// binaryWeight: weight multiplier for binary clauses (default 100.0)
func (s *CDCLSolver) SetClauseInitWeights(baseWeight, binaryWeight float64) {
	s.vsids.SetClauseInitWeights(baseWeight, binaryWeight)
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

	s.Log("c \n")
	s.Log("c === Solving Statistics ===\n")
	s.Log("c Variables:     %d\n", s.cnf.NumVars)
	s.Log("c Clauses:       %d\n", s.cnf.NumClauses)
	s.Log("c Conflicts:     %d\n", stats.Conflicts)
	s.Log("c Decisions:     %d\n", stats.Decisions)
	s.Log("c Iterations:    %d\n", stats.Iterations)
	s.Log("c Learned:       %d / %d (maxLearned)\n", stats.LearnedClauses, s.maxLearned)
	s.Log("c Max Level:     %d\n", stats.MaxLevel)
	if stats.AvgClauseSize > 0 {
		s.Log("c Avg Clause:  %d lits (min=%d, max=%d)\n",
			stats.AvgClauseSize, stats.MinClauseSize, stats.MaxClauseSize)
	}
	if stats.AvgLBD > 0 {
		s.Log("c Avg LBD:       %d\n", stats.AvgLBD)
	}
	s.Log("c \n")
}

// InstanceStructure captures metrics about CNF structure for adaptive preprocessing
type InstanceStructure struct {
	Density          float64 // clauses / vars
	BinaryRatio      float64 // binary clauses / total
	TernaryRatio     float64 // 3-literal clauses / total
	SmallClauseRatio float64 // (binary + ternary) / total
	StructuredScore  float64 // 0.0 = random, 1.0 = highly structured
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
	MaxPasses             int
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
		MaxPasses:             3,
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

// analyzeInstanceStructure computes metrics to detect structured vs random instances
// Structured instances (Tseitin, hardware, combinatorial) benefit from aggressive preprocessing
// Random instances benefit from lightweight preprocessing only
func (s *CDCLSolver) analyzeInstanceStructure() InstanceStructure {
	structure := InstanceStructure{}

	// Density: clauses / vars
	if s.cnf.NumVars > 0 {
		structure.Density = float64(s.cnf.NumClauses) / float64(s.cnf.NumVars)
	}

	// Clause size distribution
	binaryCount := 0
	ternaryCount := 0
	smallCount := 0

	for i := 0; i < s.cnf.NumClauses; i++ {
		_, size := s.cnf.GetOriginalClauseInfo(i)
		if size == 2 {
			binaryCount++
			smallCount++
		} else if size == 3 {
			ternaryCount++
			smallCount++
		}
	}

	if s.cnf.NumClauses > 0 {
		structure.BinaryRatio = float64(binaryCount) / float64(s.cnf.NumClauses)
		structure.TernaryRatio = float64(ternaryCount) / float64(s.cnf.NumClauses)
		structure.SmallClauseRatio = float64(smallCount) / float64(s.cnf.NumClauses)
	}

	// Structured score: weighted combination of metrics
	// Key insight: structured instances have HIGH BINARY ratio + mixed clause sizes
	// Random k-SAT has uniform clause sizes (all 3-literal), low binary ratio
	// 
	// Components:
	// 1. Binary ratio (weight 0.6): structured instances often have many binary clauses
	// 2. Density (weight 0.2): moderate contribution
	// 3. Mixed sizes (weight 0.2): structured instances have varied clause sizes
	
	binaryScore := structure.BinaryRatio
	
	densityScore := 0.0
	if structure.Density > 0 {
		// Normalize: density of 5+ gets full score
		densityScore = structure.Density / 5.0
		if densityScore > 1.0 {
			densityScore = 1.0
		}
	}
	
	// Mixed size score: penalize uniform distributions
	// If all clauses are same size (e.g., all ternary), this is 0
	// If mixed (binary + ternary + larger), this approaches 1.0
	mixedSizeScore := 0.0
	if structure.BinaryRatio > 0 && structure.TernaryRatio > 0 {
		// Has both binary and ternary - good mix
		mixedSizeScore = 1.0
	} else if structure.BinaryRatio > 0 || structure.TernaryRatio > 0 {
		// Has some small clauses but not mixed
		mixedSizeScore = 0.3
	}
	
	structure.StructuredScore = binaryScore*0.6 + densityScore*0.2 + mixedSizeScore*0.2

	return structure
}

// getAdaptivePreprocessingConfig returns preprocessing config based on instance structure
func (s *CDCLSolver) getAdaptivePreprocessingConfig() PreprocessingConfig {
	structure := s.analyzeInstanceStructure()

	s.Log("c [structure] Density=%.2f, Binary=%.1f%%, Ternary=%.1f%%, Structured=%.2f\n",
			structure.Density,
			structure.BinaryRatio*100,
			structure.TernaryRatio*100,
			structure.StructuredScore)


	// Random-like instances (StructuredScore < 0.7): NO preprocessing
	// Unit propagation on random/mixed instances causes 76x more conflicts
	if structure.StructuredScore < 0.7 {
			s.Log("c [preprocessing] Random-like instance (score=%.2f) - disabling preprocessing\n", structure.StructuredScore)
		// Configure extremely aggressive VSIDS decay for random instances
		s.vsids.SetAggressiveDecay()
		// Configure very aggressive restarts (Luby base=5, glucose ratio=1.1)
		s.restartBase = 5
		s.restartGlucoseRatio = 1.1
		s.restartGlucoseMinConflicts = 10
		return PreprocessingConfig{
			EnableUnitProp:        false,
			EnableEquivalence:     false,
			EnablePureLiteral:     false,
			EnableSubsumption:     false,
			EnableSelfSubsumption: false,
			EnableHyperBinary:     false,
			MaxPasses:             0,
		}
	}

	// Highly structured (score >= 0.7): unit propagation only
		s.Log("c [preprocessing] Highly structured instance (score=%.2f) - enabling unit propagation only\n", structure.StructuredScore)
	return PreprocessingConfig{
		EnableUnitProp:        true,
		EnableEquivalence:     false,
		EnablePureLiteral:     false,
		EnableSubsumption:     false,
		EnableSelfSubsumption: false,
		EnableHyperBinary:     false,
		MaxPasses:             1,
	}
}

// hasEmptyClause returns true if any original clause has zero literals,
// which makes the formula immediately UNSAT.
func (s *CDCLSolver) hasEmptyClause() bool {
	for i := range s.cnf.Clauses {
		if len(s.cnf.Clauses[i].Literals) == 0 {
			return true
		}
	}
	return false
}

func (s *CDCLSolver) preprocessAggressive() SolveResult {
	// Empty original clause = immediately UNSAT (watched literals skip clauses <2 lits,
	// so an empty clause would be invisible to propagation and yield UNKNOWN instead of UNSAT)
	if s.hasEmptyClause() {
		s.printStats()
		return UNSAT
	}

	s.Log("c [verbose] Aggressive preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)


	// Skip on very small instances - overhead outweighs benefits
	// DISABLED: Unit clauses must always be propagated, even on tiny instances
	// if s.cnf.NumClauses < s.preprocessingMinClauses {
	// 	if s.verbose {
	// 		s.Log("c [verbose] Skipping preprocessing: instance too small (%d clauses)\n", s.cnf.NumClauses)
	// 	}
	// 	s.cnf.RebuildLiteralPool()
	// 	s.initWatches()
	// 	return UNKNOWN
	// }

	// Skip on VERY large instances - preprocessing too slow
	if int(s.cnf.NumVars) > s.preprocessingMaxVars || s.cnf.NumClauses > s.preprocessingMaxClauses {
			s.Log("c [verbose] Skipping preprocessing: instance too large (%d vars, %d clauses)\n",
			s.cnf.NumVars, s.cnf.NumClauses)
		s.cnf.RebuildLiteralPool()
		s.initWatches()
		return UNKNOWN
	}

	// Get adaptive preprocessing config based on instance structure
	config := s.getAdaptivePreprocessingConfig()
	initialClauses := s.cnf.NumClauses
	maxPasses := config.MaxPasses

	// Increase to 5 passes for more thorough preprocessing
	// Modern solvers (CaDiCaL) use 10+ passes
	// Safeguards: time limits in each technique prevent explosion
	for pass := 0; pass < maxPasses; pass++ {
			s.Log("c [verbose] Preprocessing pass %d/%d: %d clauses\n", pass+1, maxPasses, s.cnf.NumClauses)

		// Run unit propagation first to catch any existing units
		if config.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Equivalence detection: find a↔b patterns and substitute
		if config.EnableEquivalence {
			if equivResult := s.equivalenceDetection(); equivResult != UNKNOWN {
				return equivResult
			}
		}

		// Run unit propagation to catch new units from equivalence substitution
		if config.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Pure literal elimination - cheap, run on all instances
		if config.EnablePureLiteral {
			if pureResult := s.pureLiteralElimination(); pureResult != UNKNOWN {
				return pureResult
			}
		}

		// Self-subsumption
		if config.EnableSelfSubsumption {
			s.selfSubsumption()
		}

		// Hyper-binary resolution
		if config.EnableHyperBinary {
			s.hyperBinaryResolution()
		}

		// Run unit propagation after hyper-binary resolution
		if config.EnableUnitProp {
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

	s.Log("c [verbose] After preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)


	// FIX: Do NOT clear preprocessing assignments (e.g., from pure literal elimination)
	// These are permanent assignments that must be part of the final model.
	// Only clear the trail and reset search state.
	// Count preprocessing assignments for statistics
	preprocAssignments := 0
	for i := range s.assignments {
		if s.assignments[i].Level >= 0 {
			preprocAssignments++
		}
	}
	
	s.trail = s.trail[:0]
	s.trailHead = []int{0}
	s.level = 0
	s.qhead = 0
	// FIX: Only clear implication for unassigned variables
	// Preprocessing assignments (Level >= 0) must keep their implication to prevent re-propagation
	for i := range s.implication {
		if s.assignments[i].Level < 0 {
			s.implication[i] = -1
		}
		// For assigned variables, implication stays as-is (set by unit propagation or pure literal elimination)
	}
	
	if s.verbose && preprocAssignments > 0 {
		s.Log("c [verbose] Preserving %d preprocessing assignments for final model\n", preprocAssignments)
	}

	// CRITICAL FIX: Rebuild trail from all preprocessing assignments
	// Pure literal elimination and other techniques assign variables but may not
	// add them to the trail. The watch system needs all assignments in the trail
	// to process watches correctly when search starts.
	s.trail = s.trail[:0]
	for i := range s.assignments {
		if s.assignments[i].Level == 0 {
			s.trail = append(s.trail, int(i))
		}
	}
	s.trailHead = []int{0}  // Only level 0 exists after preprocessing
	// CRITICAL FIX: Set qhead to trail length - preprocessing assignments already processed
	// Without this, propagateWatched re-processes all 364 preprocessing assignments
	s.qhead = len(s.trail)

	// Rebuild literal pool after preprocessing (even if no clauses removed)
	s.cnf.RebuildLiteralPool()

	// Initialize watches after unit propagation
	// CRITICAL: Reset watchInitialized flag so watches are re-initialized
	s.watchInitialized = false
	s.initWatches()

	// CRITICAL: Propagate original unit clauses (not watched by watched literals)
	// Unit clauses have only 1 literal and are not added to watch lists
	for clauseIdx := 0; clauseIdx < s.cnf.NumClauses; clauseIdx++ {
		clause := &s.cnf.Clauses[clauseIdx]
		if len(clause.Literals) != 1 {
			continue
		}
		lit := clause.Literals[0]
		varIdx := lit.Var()
		if s.assignments[varIdx].Level >= 0 {
			// Already assigned - check for conflict
			litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
			if !litTrue {
				// Conflict with unit clause - UNSAT
				return UNSAT
			}
			continue
		}
		// Propagate unit clause
		value := !lit.IsNegated()
		s.assignments[varIdx] = Assignment{Value: value, Level: 0}
		s.varLevel[varIdx] = 0
		s.trail = append(s.trail, int(varIdx))
		s.implication[varIdx] = -2 // Mark as unit propagation
	}
	// Update qhead since we added to trail
	s.qhead = len(s.trail)

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
		avgWatches := float64(totalWatches) / float64(numLits)
		s.Log("c [verbose] Watched literals enabled: %d watch lists, %d total watches, %.1f avg per lit\n",
			len(s.watchLists), totalWatches, avgWatches)
	}
}

// chooseWatchPositions selects two literal positions (indices into literals)
// to watch, preferring non-false (unassigned or true) literals so the
// watched-literal invariant (at most one watched literal is false) holds
// after setup. Returns positions (-1 if fewer than two candidates found).
// Shared by original/learned clause watch setup and compaction rebuild.
func (s *CDCLSolver) chooseWatchPositions(literals []cnf.Literal) (int, int) {
	watch0 := -1
	watch1 := -1

	for i, lit := range literals {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level >= 0 {
			// Assigned - check if true
			litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
			if !litTrue {
				// False - skip unless we have no other choice
				if watch0 < 0 {
					watch0 = i
				} else if watch1 < 0 {
					watch1 = i
				}
				continue
			}
		}
		// Unassigned or true - prefer this
		if watch0 < 0 {
			watch0 = i
		} else if watch1 < 0 {
			watch1 = i
			break
		}
	}

	return watch0, watch1
}

// addOriginalClauseToWatches adds an original clause to the watch lists
// Watches the first two literals that are not both false
func (s *CDCLSolver) addOriginalClauseToWatches(clauseIdx int, clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	watch0, watch1 := s.chooseWatchPositions(literals)
	if watch0 < 0 || watch1 < 0 {
		// Clause has < 2 literals - shouldn't happen after preprocessing
		return
	}

	lit0 := literals[watch0]
	lit1 := literals[watch1]

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Add watches (symmetric watch tracking via ClauseIdx scanning)
	// OPTIMIZATION: No Clause pointer - use ClauseIdx for all accesses
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx1),
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx0),
	})
}

// addLearnedClauseToWatches adds a learned clause to the watch lists
// Watches the first two literals that are not both false
// BINARY CLAUSE OPTIMIZATION: Binary clauses use separate watch lists for optimized propagation
func (s *CDCLSolver) addLearnedClauseToWatches(learnedIdx int, clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	// Choose watched literals that are not both false (shared helper).
	watch0, watch1 := s.chooseWatchPositions(literals)
	if watch0 < 0 || watch1 < 0 {
		return
	}

	lit0 := literals[watch0]
	lit1 := literals[watch1]

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Learned clause index is stored as negative: -learnedIdx-1
	clauseIdx := -learnedIdx - 1

	// Add watches (symmetric watch tracking via ClauseIdx scanning)
	// OPTIMIZATION: No Clause pointer - use ClauseIdx for all accesses
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx1),
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: clauseIdx,
		Blit:      uint32(idx0),
	})

	// Watch indices now stored in learnClause() for consistency
}

// removeLearnedClauseWatches removes all watches for a deleted learned clause
// FIX: Use stored watched literal indices instead of reading from potentially corrupted literals
func (s *CDCLSolver) removeLearnedClauseWatches(learnedIdx int) {
	if learnedIdx < 0 || learnedIdx >= s.learnedCapacity {
		return
	}

	if s.learnedSizes[learnedIdx] < 2 {
		return // Clause too short to have watches
	}

	clauseIdx := -learnedIdx - 1

	// Use stored watched literal indices (not from potentially corrupted literals)
	idx0 := s.learnedWatchIdx0[learnedIdx]
	idx1 := s.learnedWatchIdx1[learnedIdx]

	// Skip if watch indices are invalid (already removed or corrupted)
	if idx0 < 0 || idx1 < 0 || idx0 >= len(s.watchLists) || idx1 >= len(s.watchLists) {
		return
	}

	// Remove watch from lit0's watch list
	watchList0 := s.watchLists[idx0]
	for i := range watchList0 {
		if watchList0[i].ClauseIdx == clauseIdx {
			// Remove this watch by swapping with last
			lastIdx := len(watchList0) - 1
			if i != lastIdx {
				watchList0[i] = watchList0[lastIdx]
				// Update symmetric watch Blit
				movedWatch := watchList0[i]
				for symI := range s.watchLists[movedWatch.Blit] {
					if s.watchLists[movedWatch.Blit][symI].ClauseIdx == movedWatch.ClauseIdx {
						s.watchLists[movedWatch.Blit][symI].Blit = uint32(idx0)
						break
					}
				}
			}
			watchList0 = watchList0[:lastIdx]
			break
		}
	}
	s.watchLists[idx0] = watchList0

	// Remove watch from lit1's watch list
	watchList1 := s.watchLists[idx1]
	for i := range watchList1 {
		if watchList1[i].ClauseIdx == clauseIdx {
			// Remove this watch by swapping with last
			lastIdx := len(watchList1) - 1
			if i != lastIdx {
				watchList1[i] = watchList1[lastIdx]
				// Update symmetric watch Blit
				movedWatch := watchList1[i]
				for symI := range s.watchLists[movedWatch.Blit] {
					if s.watchLists[movedWatch.Blit][symI].ClauseIdx == movedWatch.ClauseIdx {
						s.watchLists[movedWatch.Blit][symI].Blit = uint32(idx1)
						break
					}
				}
			}
			watchList1 = watchList1[:lastIdx]
			break
		}
	}
	s.watchLists[idx1] = watchList1
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
							if resolvent != nil && clauseSubsumes(resolvent, &s.cnf.Clauses[j]) {
								s.cnf.Clauses[j] = *resolvent
								changed = true
																s.Log("c [verbose] Self-subsumption: strengthened clause\n")
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
		// CRITICAL: Only consider ORIGINAL clauses for hyper-binary resolution
		// Learned clauses are context-dependent and cannot be used for permanent simplification
		if clause.Learned {
			continue
		}
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
		// CRITICAL: Only simplify ORIGINAL clauses
		if clause.Learned {
			continue
		}
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
			s.Log("c [verbose] Hyper-binary: empty clause\n")
			return
		}

		s.cnf.Clauses[i] = cnf.Clause{Literals: newLiterals, Learned: false}
	satisfied:
	}

	// FIX: Do NOT remove satisfied clauses during preprocessing.
	// The CNF must remain unchanged for sound model verification.
	// Satisfied clauses will be handled efficiently by the watch system during search.
	// Removing clauses breaks verification because we check against the modified CNF,
	// not the original, leading to unsound SAT verdicts.
}

func (s *CDCLSolver) inprocessBlockedClauseElimination() {
	if s.cnf.NumClauses > 2000 {
		return // Skip on large instances
	}

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
	}

	if removed > 0 {
		s.cnf.Clauses = keptClauses
		s.cnf.NumClauses = len(s.cnf.Clauses)
			s.Log("c [inprocess] Blocked clause elimination: removed %d clauses\n", removed)
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
		// CRITICAL: Only consider ORIGINAL clauses as units
		// Learned unit clauses are context-dependent and cannot be used for simplification
		if clause.Learned {
			continue
		}
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
	seenVars := make(map[uint32]bool) // Track added literals to prevent duplicates

	for _, lit := range c1.Literals {
		if lit.Var() == varIdx {
			if lit.IsNegated() {
				foundNeg = true
			} else {
				foundPos = true
			}
		} else {
			// Only add if not already present (prevent duplicates)
			if !seenVars[lit.Var()] {
				literals = append(literals, lit)
				seenVars[lit.Var()] = true
			}
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
			// Only add if not already present (prevent duplicates)
			if !seenVars[lit.Var()] {
				literals = append(literals, lit)
				seenVars[lit.Var()] = true
			}
		}
	}

	if !(foundNeg && foundPos) {
		return nil
	}

	return &cnf.Clause{Literals: literals, Learned: false}
}

// clauseSubsumes checks if c1 subsumes c2 (c1 is subset of c2)
// Used by self-subsumption and variable elimination
// c1 subsumes c2 if all literals in c1 are also in c2 (same var, same polarity)
func clauseSubsumes(c1, c2 *cnf.Clause) bool {
	if len(c1.Literals) > len(c2.Literals) {
		return false
	}
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
			s.Log("c [restart] Glucose: LBD %.1f > avg %.1f × %.2f\n",
				recentLBD, avgLBD, s.restartGlucoseRatio)
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

func (s *CDCLSolver) restart() bool {
	s.Log("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)


	// CRITICAL: Reset VSIDS activity on restart to escape local minima
	// Random instances need aggressive diversification - activity converges too quickly
	s.vsids.ResetActivityPartial(0.3) // Keep 30% of activity, add noise

	// Use stored LBD values (calculated at learning time) instead of recalculating
	// Recalculating during restart gives wrong values since assignments change
	glueCount := 0
	
	// Ensure tmpIsGlue buffer is large enough
	if cap(s.tmpIsGlue) < len(s.learnedOffsets) {
		s.tmpIsGlue = make([]bool, len(s.learnedOffsets))
	}
	isGlue := s.tmpIsGlue[:len(s.learnedOffsets)]
	
	// Clear buffer
	for i := range isGlue {
		isGlue[i] = false
	}

	for i := 0; i < len(s.learnedOffsets); i++ {
		lbd := s.learnedMetadata[i].LBD

		// Keep glue clauses (configurable via restartKeepGlueLBD, default 3)
		// LBD ≤ 2: core glue (most valuable)
		// LBD = 3: near-glue (very valuable)
		// LBD > restartKeepGlueLBD: delete (will be re-learned if needed)
		if lbd <= s.restartKeepGlueLBD {
			glueCount++
			isGlue[i] = true
		}
	}

		s.Log("c [verbose] Restart: %d glue clauses (LBD≤%d), %d total active\n", glueCount, s.restartKeepGlueLBD, s.learnedActiveCount)

	// NOTE: We don't delete clauses on restart - let deleteLearnedClauses handle memory management
	// Restart is for escaping local minima, not for clause deletion
	// Deleting clauses on restart throws away potentially useful learned information

	// Clear trail and assignments
	// FIX: Preserve ONLY preprocessing assignments (Level <= 1 AND implication <= -2)
	// Learned clause propagations have Level > 1 and implication < -1, must be cleared
	
	// CRITICAL FIX: Rebuild trail from preserved preprocessing assignments
	// After inprocessing rebuilds watches, preserved assignments must be in the trail
	// so propagateWatched() processes them through the new watch lists.
	// Without this, preserved assignments are invisible to the watch system.
	s.trail = s.trail[:0]
	for i := range s.assignments {
		// CRITICAL FIX: Only preserve preprocessing assignments
		// Must check BOTH implication AND level:
		// - Preprocessing: Level == 0 AND (implication >= 0 or -2 or -3)
		// - Search: Level >= 1 (even if implication >= 0 from original clause propagation)
		if s.assignments[i].Level == 0 && (s.implication[i] >= 0 || s.implication[i] == -2 || s.implication[i] == -3) {
			s.trail = append(s.trail, int(i))
		}
	}
	s.trailHead = append(s.trailHead[:0], 0) // Reset to [0] for level 0
	// CRITICAL FIX: Set qhead to trail length - preprocessing assignments already processed
	// Setting qhead=0 causes massive slowdown: 364 assignments × 100+ restarts = 36K redundant propagations
	s.qhead = len(s.trail)
	s.level = 0 // Start at level 0, first decision will increment to 1
	
	for i := range s.implication {
		// CRITICAL FIX: Only preserve preprocessing assignments
		// Clear ALL search assignments (learned clause propagations and decisions)
		if s.assignments[i].Level == 0 && (s.implication[i] >= 0 || s.implication[i] == -2 || s.implication[i] == -3) {
			continue
		}
		s.implication[i] = -1
	}
	for i := range s.assignments {
		// CRITICAL FIX: Only preserve preprocessing assignments
		if s.assignments[i].Level == 0 && (s.implication[i] >= 0 || s.implication[i] == -2 || s.implication[i] == -3) {
			continue
		}
		s.assignments[i] = Assignment{Level: -1}
		s.varLevel[i] = -1
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
	s.level = 0  // FIX: Start at level 0, decisions will be at level 1+
	s.trailHead = s.trailHead[:0]
	s.trailHead = append(s.trailHead, 0)
	for i := 0; i < s.cnf.NumClauses; i++ {
		clause := &s.cnf.Clauses[i]
		if len(clause.Literals) == 1 {
			lit := clause.Literals[0]
			varIdx := lit.Var()
			if s.assignments[varIdx].Level < 0 {
				value := !lit.IsNegated()
				s.assignments[varIdx] = Assignment{
					Value: value,
					Level: 0,  // Unit propagations from original clauses are permanent (level 0)
				}
				s.varLevel[varIdx] = 0
				s.trail = append(s.trail, int(varIdx))
				s.implication[varIdx] = i // Original clause index (positive)
			}
		}
	}
	// FIX: Don't add spurious trailHead entry for unit propagations
	// CRITICAL: Update qhead to skip the unit propagations we just added
	// Otherwise propagateWatched() will re-process them, causing massive slowdown
	s.qhead = len(s.trail)

	// Reclaim literal storage when too many tombstones have accumulated.
	// This is the safe moment: we are at level 0 and no learned clause is in
	// use as a reason (all search implications were cleared above), so the
	// implication remapping inside compactLearnedClauses is a no-op and the
	// watch rebuild uses the non-false literal selection. Reprocess the
	// trail against the freshly rebuilt watches for safety.
	if s.compactPending {
		s.compactLearnedClauses()
		s.compactPending = false
		s.qhead = 0
	}

	return false // No UNSAT detected
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
			s.Log("c [verbose] Blocked clause elimination: skipped (%d clauses, limit 15000)\n", s.cnf.NumClauses)
		return UNKNOWN
	}

	s.Log("c [verbose] Blocked clause elimination: checking %d clauses\n", s.cnf.NumClauses)


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

	s.Log("c [verbose] Blocked clause elimination: removed %d clauses\n", removedCount)


	return UNKNOWN
}

func (s *CDCLSolver) inprocessing() bool {
	s.Log("c [inprocess] Inprocessing at conflict %d: %d clauses\n", s.conflicts, s.cnf.NumClauses)


	initialClauses := s.cnf.NumClauses
	startTime := time.Now()
	timeLimit := 500 * time.Millisecond // Limit inprocessing time

	// 1. Unit propagation (cheap, can find new units from learned clauses)
	s.inprocessUnitPropagation()
	if s.emptyClauseFound {
		return true // UNSAT detected
	}
	if time.Since(startTime) > timeLimit {
		return false
	}

	// 2. Variable elimination DISABLED - soundness bug with variable tracking
	// Preprocessing VE eliminates most vars, search handles the rest
	if time.Since(startTime) > timeLimit {
		return false
	}

	// 3. Blocked clause elimination DISABLED - soundness bug removing non-redundant clauses
	// Run every 200 conflicts (more expensive than subsumption)
	// if s.conflicts%200 == 0 && s.cnf.NumClauses < 2000 {
	// 	s.inprocessBlockedClauseElimination()
	// }
	if time.Since(startTime) > timeLimit {
		return false
	}

	// 5. Self-subsumption (every 1000 conflicts, more expensive)
	// Further reduce clause database after other simplifications
	if s.conflicts%1000 == 0 && s.cnf.NumClauses < 5000 {
		s.selfSubsumption()
	}
	if time.Since(startTime) > timeLimit {
		return false
	}

	removed := initialClauses - s.cnf.NumClauses
	if s.verbose && removed != 0 {
		elapsed := time.Since(startTime)
		s.Log("c [inprocess] Inprocessing complete: removed %d clauses in %.1fms\n", removed, float64(elapsed.Nanoseconds())/1e6)
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
	
	return false // No UNSAT detected
}

// inprocessVariableElimination performs lightweight variable elimination during search
// Lighter version of variableElimination() with shorter time limit and fewer iterations
// Only eliminates variables with positive deficiency (net clause reduction)
// Does NOT track eliminated variables for model reconstruction (too complex during search)
func (s *CDCLSolver) inprocessPureLiteralElimination() {
	s.Log("c [inprocess] Pure literal elimination during search: %d vars, %d clauses\n",
			s.cnf.NumVars, s.cnf.NumClauses)


	startTime := time.Now()
	timeLimit := 50 * time.Millisecond // Short time limit for inprocessing
	assignedCount := 0

	changed := true
	for changed {
		if time.Since(startTime) > timeLimit {
			break
		}

		changed = false

		// Scan for pure literals (use pre-allocated buffers)
		hasPositive := s.tmpHasPositive[:s.cnf.NumVars]
		hasNegative := s.tmpHasNegative[:s.cnf.NumVars]
		
		// Clear buffers
		for i := range hasPositive {
			hasPositive[i] = false
			hasNegative[i] = false
		}

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

								s.Log("c [inprocess] Pure literal: assigned var %d = %v, now %d clauses\n",
					varIdx, pureValue, s.cnf.NumClauses)
			}
		}
	}

	if s.verbose && assignedCount > 0 {
		elapsed := time.Since(startTime)
		s.Log("c [inprocess] Pure literal eliminated %d variables in %.1fms, now %d clauses\n",
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
	// FIX: Do NOT clear existing assignments (from pure literal elimination, etc.)
	// Only reset trail and propagate NEW unit clauses from current state
	// Initialize trail for preprocessing (reuse capacity)
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	s.level = 0  // Unit propagations at level 0, decisions start at level 1

	// TIME BUDGET: Limit unit propagation to 500ms to avoid spending too long in preprocessing
	// This allows running until fixpoint on small instances while preventing timeout on large ones
	const timeBudget = 500 * time.Millisecond
	startTime := time.Now()

	// Count unit clauses for debugging
	unitCount := 0
	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) == 1 {
			unitCount++
		}
	}
	s.Log("c [unit prop] Starting with %d unit clauses, %d existing assignments\n", unitCount, s.countAssignedVariables())

	changed := true
	pass := 0
	for changed {
		// Check time budget
		if time.Since(startTime) > timeBudget {
			s.Log("c [unit prop] Time budget exceeded (%.1fms), stopping after %d passes, trail has %d units\n",
				float64(time.Since(startTime).Nanoseconds())/1e6, pass, len(s.trail))
			break
		}
		
		changed = false
		pass++

		clauseCount := len(s.cnf.Clauses)
		for clauseIdx := 0; clauseIdx < clauseCount; clauseIdx++ {
			clause := s.cnf.Clauses[clauseIdx]

			satisfied := false
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal

			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level >= 0 {
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
				s.Log("c [verbose] Preprocessing: conflict in unit propagation\n")
				return UNSAT
			}

			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				varIdx := unassignedLit.Var()
				// FIX: Check if variable is already assigned (may have been assigned by pure literal elimination)
				if s.assignments[varIdx].Level >= 0 {
					// Already assigned - check if consistent
					expectedValue := !unassignedLit.IsNegated()
					if s.assignments[varIdx].Value != expectedValue {
						// Conflict: variable already assigned opposite value
						s.Log("c [verbose] Preprocessing: conflict - var %d already assigned opposite value\n", varIdx)
						return UNSAT
					}
					// Already assigned correctly - skip
					continue
				}
				value := !unassignedLit.IsNegated()
				s.assignments[varIdx] = Assignment{
					Value: value,
					Level: 0,  // Unit propagations at level 0
				}
				s.varLevel[varIdx] = 0
				s.trail = append(s.trail, int(varIdx))
				// FIX: Set implication to prevent re-propagation during search
				// Use -2 to indicate "assigned by preprocessing unit propagation"
				s.implication[varIdx] = -2
				changed = true
				if s.verbose && len(clause.Literals) == 1 {
					s.Log("c [unit prop] Propagated unit clause: var %d = %v, implication=%d\n", varIdx, value, s.implication[varIdx])
				}
				// Don't modify clauses - just track assignments in trail
			}
		}
	}

	s.Log("c [unit prop] Finished after %d passes, trail has %d units\n", pass, len(s.trail))

	// Set up trail for search - reset to initial state, units are at level 0
	s.trailHead = []int{0}
	s.level = 0

	return UNKNOWN
}

// countAssignedVariables counts variables with Level >= 0
func (s *CDCLSolver) countAssignedVariables() int {
	count := 0
	for _, assign := range s.assignments {
		if assign.Level >= 0 {
			count++
		}
	}
	return count
}

// inprocessUnitPropagation performs unit propagation during search
// Unlike unitPropagationPreprocess, this runs on the current trail state
// and doesn't reset assignments. It's safe to call during search.
func (s *CDCLSolver) inprocessUnitPropagation() {
	// SOUND INPROCESSING: Level-0 unit propagation
	// Only process original clauses (learned clauses change too frequently)
	// Assignments are made at level 0 (permanent, never backtracked)
	// This is sound: equivalent to preprocessing unit propagation

	// Collect all unit clauses first (avoid modifying during iteration)
	type unitClause struct {
		varIdx uint32
		value  bool
	}
	units := make([]unitClause, 0, 16)

	// Phase 1: Scan for unit clauses
	// CRITICAL: Only consider ORIGINAL clauses as permanent units
	// Learned unit clauses are context-dependent and cannot be propagated at level 0
	for i := 0; i < s.cnf.NumClauses && i < len(s.cnf.Clauses); i++ {
		clause := &s.cnf.Clauses[i]
		if clause.Learned {
			continue
		}
		if len(clause.Literals) != 1 {
			continue
		}

		lit := clause.Literals[0]
		varIdx := lit.Var()

		// Check if already assigned (Level >= 0 means assigned)
		if s.assignments[varIdx].Level >= 0 {
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
		s.trail = append(s.trail, int(unit.varIdx))
		s.implication[unit.varIdx] = -2 // Mark as unit propagation (not decision)

			s.Log("c [inprocess] Unit propagation: var %d = %v (level 0)\n", unit.varIdx, unit.value)
	}
	
	// CRITICAL: Propagate the level-0 assignments through watch lists
	// Without this, clauses that should be satisfied by these assignments are not processed
	if len(units) > 0 {
		s.qhead = 0
		s.level = 0
		s.trailHead = []int{0}  // FIX: Unit propagations are not decisions, don't add spurious trailHead entry
		// Run propagation to process the level-0 assignments
		if conflict, _ := s.propagate(); conflict {
			s.Log("c [inprocess] Conflict during level-0 propagation - UNSAT\n")
			// Conflict at level 0 means UNSAT - but we can't return here
			// Just mark the solver for UNSAT detection
			s.emptyClauseFound = true
		}
	}
}

func (s *CDCLSolver) simplifyAfterAssignment(varIdx uint32, value bool) bool {
	// DISABLED: Do NOT modify clauses during preprocessing.
	// Clause modification causes soundness bugs when combined with pure literal elimination.
	// The watch system will handle clause satisfaction during search.
	// Just check for conflicts.
	for i := range s.cnf.Clauses {
		clause := &s.cnf.Clauses[i]
		allFalse := true
		for _, lit := range clause.Literals {
			if lit.Var() == varIdx {
				litValue := !lit.IsNegated()
				if litValue == value {
					// Clause is satisfied
					allFalse = false
					break
				}
			} else if s.assignments[lit.Var()].Level >= 0 {
				// Check if other literals are already satisfied
				assignValue := s.assignments[lit.Var()].Value
				litIsTrue := (!lit.IsNegated() && assignValue) || (lit.IsNegated() && !assignValue)
				if litIsTrue {
					allFalse = false
					break
				}
			} else {
				// Unassigned literal - clause not yet decided
				allFalse = false
			}
		}
		if allFalse {
			return true // Empty clause - UNSAT
		}
	}
	return false
}

func (s *CDCLSolver) equivalenceDetection() SolveResult {
		s.Log("c [verbose] EquivalenceDetection() called with %d clauses\n", s.cnf.NumClauses)

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
		s.Log("c [verbose] Equivalence detection: found %d implications from %d binary clauses\n",
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
			s.Log("c [verbose] Equivalence detection: no bidirectional implications found\n")
		}
		return UNKNOWN
	}

	// Step 5: Substitute throughout formula
	newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))

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
			s.Log("c [verbose] Equivalence detection: empty clause created (UNSAT)\n")
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

		s.Log("c [verbose] Equivalence detection: substituted %d variables, resulting in %d clauses\n",
		len(substMap), s.cnf.NumClauses)

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
				if s.assignments[varIdx].Level >= 0 {
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
			if s.assignments[varIdx].Level >= 0 {
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
					Level: 0,  // CRITICAL FIX: Level 0 for preprocessing assignments
				}
				s.varLevel[varIdx] = 0
				// FIX: Set implication to prevent re-propagation during search
				// Use -3 to indicate "assigned by pure literal elimination"
				s.implication[varIdx] = -3
				// CRITICAL FIX: Add to trail so watch propagation processes these assignments
				s.trail = append(s.trail, int(varIdx))
				changed = true

				conflict := s.simplifyAfterAssignment(varIdx, pureValue)
				if conflict {
										s.Log("c [verbose] Pure literal elimination: empty clause created\n")
					return UNSAT
				}

								s.Log("c [verbose] Pure literal elimination: assigned var %d = %v\n", varIdx, pureValue)
			}
		}
	}

	// FIX: Check if all clauses are satisfied (not if clause list is empty)
	// Since we no longer remove clauses during preprocessing, we need to check
	// if all clauses are satisfied by the current assignments.
	allSatisfied := true
	for _, clause := range s.cnf.Clauses {
		satisfied := false
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			if s.assignments[varIdx].Level >= 0 {
				litIsTrue := (s.assignments[varIdx].Value != lit.IsNegated())
				if litIsTrue {
					satisfied = true
					break
				}
			}
		}
		if !satisfied {
			allSatisfied = false
			break
		}
	}

	if allSatisfied {
			s.Log("c [verbose] Pure literal elimination: all clauses satisfied\n")
		// Assign all remaining unassigned variables arbitrarily
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level < 0 {
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
		// CRITICAL: Verify model before returning SAT
		if !s.verifyModel() {
			return UNKNOWN
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
//
// CRITICAL: Processes one variable at a time and re-computes elimination candidates after each elimination

// variableEliminationSinglePass eliminates variables with pos=1 from the ORIGINAL formula only
// This is sound because definitions don't have transitive dependencies
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

// initVSIDSOccurrenceBonus initializes VSIDS activity with a clause-length
// weighted base (via InitializeFromClauses) plus an occurrence-based bonus.
// Variables appearing in more clauses get higher activity since they are more
// constrained. Built in O(clauses x literals) rather than O(vars x clauses).
func (s *CDCLSolver) initVSIDSOccurrenceBonus() {
	// Initialize VSIDS with clause-length weighted activity BEFORE search.
	// Variables in shorter clauses get higher activity (more constrained).
	// Binary clauses get 100x base weight to bias initial variable selection.
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	// Occurrence-based activity bonus: variables in more remaining clauses are
	// more constrained and should be selected earlier.
	occurrences := make([]int, s.cnf.NumVars)
	for _, clause := range s.cnf.Clauses {
		for _, lit := range clause.Literals {
			occurrences[lit.Var()]++
		}
	}
	for i := range s.assignments {
		if s.assignments[i].Level < 0 {
			s.vsids.activity[i] = 1.0 + float64(occurrences[i])*0.5
		}
	}
	s.vsids.heapValid = false // Force heap rebuild
}

// cdclLoop runs the main CDCL search loop until SAT, UNSAT, or UNKNOWN is
// determined. It assumes preprocessing, watch initialization, and VSIDS
// initialization are already complete.
func (s *CDCLSolver) cdclLoop() SolveResult {
	for {
		s.iterations++
		if s.iterations%IterationReportInterval == 0 && s.verbose {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			s.Log("c [progress] Iter %d, Conflicts %d, Level %d, Learned %d, Alloc=%dMB\n",
				s.iterations, s.conflicts, s.level, s.learnedActiveCount, mem.Alloc/1024/1024)
		}
		if s.maxIter > 0 && s.iterations > s.maxIter {
			s.Log("c [verbose] Iteration limit reached (%d)\n", s.maxIter)
			s.printStats()
			return UNKNOWN
		}

		// Propagate all clauses (unit learned clauses handled in propagate())
		conflict, conflictClause := s.propagate()
		if conflict {
			s.handleConflict(conflictClause)
			if s.conflicts%50 == 0 && s.verbose {
				propsPerDec := 0.0
				if s.decisions > 0 {
					propsPerDec = float64(s.propagations) / float64(s.decisions)
				}
				s.Log("c [verbose] Conflict %d, level %d, learned %d, decisions %d, propagations %d, props/dec %.1f\n",
					s.conflicts, s.level, s.learnedActiveCount, s.decisions, s.propagations, propsPerDec)
			}
			if !s.backtrack() {
				s.printStats()
				return UNSAT
			}
			s.backjumpLevel = 0

			if s.shouldRestart() {
				if s.restart() {
					// UNSAT detected during restart/inprocessing
					s.printStats()
					return UNSAT
				}
			}

			continue
		}

		if s.allAssigned() {
			// CRITICAL: Verify model before declaring SAT
			// All variables assigned doesn't guarantee all clauses satisfied
			s.Log("c [SOLVE] All assigned, verifying model...\n")
			if !s.verifyModel() {
				s.Log("c [SOLVE] Model verification FAILED - returning UNKNOWN\n")
				s.printStats()
				// Model invalid - this indicates a bug, return UNKNOWN
				return UNKNOWN
			}
			s.Log("c [SOLVE] Model verification PASSED\n")
			s.printStats()
			return SAT
		}

		if !s.decide() {
			s.printStats()
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
// This is the primary solving method. It performs adaptive preprocessing
// based on instance structure analysis, then runs the main CDCL search loop
// with watched-literals propagation, 1-UIP conflict analysis, clause learning
// and deletion, and adaptive restarts.
//
// For a debug escape hatch that skips all preprocessing, use
// SolveWithoutPreprocessing().
//
// The solver maintains internal state; create a new solver for each formula.
func (s *CDCLSolver) SolveWithResult() SolveResult {
	// Adaptive preprocessing: structure analysis selects techniques and
	// tunes VSIDS/restart parameters. See preprocessAggressive.
	preprocResult := s.preprocessAggressive()
	if preprocResult != UNKNOWN {
		s.printStats()
		return preprocResult
	}

	s.initVSIDSOccurrenceBonus()

	return s.cdclLoop()
}

// SolveWithoutPreprocessing skips all preprocessing (no unit propagation, no
// structure analysis, no adaptive tuning) and goes straight to the CDCL search
// loop after minimal setup. Intended as a debug escape hatch (the -no-preprocess
// CLI flag); production code should use SolveWithResult.
func (s *CDCLSolver) SolveWithoutPreprocessing() SolveResult {
	if s.hasEmptyClause() {
		s.printStats()
		return UNSAT
	}
	s.cnf.RebuildLiteralPool()
	s.initWatches()
	s.initVSIDSOccurrenceBonus()
	return s.cdclLoop()
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
//	        s.Log("%d ", val)
//	    }
//	}
//	fmt.Println("0")
func (s *CDCLSolver) GetAssignments() []Assignment {
	return s.assignments
}

func (s *CDCLSolver) allAssigned() bool {
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level < 0 {
			return false
		}
	}
	return true
}

// verifyModel checks if the current assignment satisfies all clauses
// Returns true if model is valid, false otherwise
func (s *CDCLSolver) verifyModel() bool {
		s.Log("c [VERIFY] Checking %d clauses...\n", len(s.cnf.Clauses))
	for ci, clause := range s.cnf.Clauses {
		clauseSat := false
		for _, lit := range clause.Literals {
			assign := s.assignments[lit.Var()]
			// FIX: Check if variable is actually assigned (level >= 0)
			// Unassigned variables (level < 0) cannot satisfy clauses
			if assign.Level < 0 {
				continue // Unassigned - clause not satisfied by this literal
			}
			litTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
			if litTrue {
				clauseSat = true
				break
			}
		}
		if !clauseSat {
			s.Log("c [VERIFY] Clause %d NOT satisfied: ", ci)
			for _, lit := range clause.Literals {
				if lit.IsNegated() {
			s.Log("-%d ", lit.Var()+1)
				} else {
			s.Log("%d ", lit.Var()+1)
				}
			s.Log("(assignments: ")
				for _, lit := range clause.Literals {
					v := lit.Var()
			s.Log("var%d={V=%v,L=%d,I=%d} ", v+1, s.assignments[v].Value, s.assignments[v].Level, s.implication[v])
				}
			s.Log(")\n")
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

func (s *CDCLSolver) propagateWatched() (bool, *cnf.Clause) {
	if !s.watchInitialized {
		return s.propagate()
	}

	propagationCount := 0

	if s.verbose && s.conflicts <= 10 {
		s.Log("c [PROPAGATE] qhead=%d, trail len=%d, level=%d\n", s.qhead, len(s.trail), s.level)
	}

	// OPTIMIZATION #1: Use unitLearnedList for O(1) unit propagation
	// Previously scanned ALL learned clauses (O(n)), now only scans unit clauses
	// This must run even if trail is empty (after backtrack to level 0)
	// Learned clauses >= 2 literals are watched, but unit learned clauses
	// (size=1) are not watched and must be scanned explicitly.
	unitCount := 0
	for _, learnedIdx := range s.unitLearnedList {
		// Skip deleted/tombstone entries (can happen after swap-remove)
		if learnedIdx >= s.learnedCapacity || s.learnedSizes[learnedIdx] != 1 {
			continue
		}
		literals := s.getLearnedClauseLiterals(learnedIdx)
		if len(literals) != 1 {
			continue
		}
		unitCount++
		lit := literals[0]
		varIdx := lit.Var()
		litValue := !lit.IsNegated()
		if s.verbose {
			s.Log("c [UNIT SCAN] idx=%d, var=%d, level=%d\n", learnedIdx, varIdx+1, s.assignments[varIdx].Level)
		}
		if s.assignments[varIdx].Level < 0 {
			// CRITICAL FIX: Propagate at max(s.level, 1) to maintain trail invariant
			// All trail elements must be at levels <= s.level (or level 1 if s.level=0)
			propLevel := s.level
			if propLevel == 0 {
				propLevel = 1
			}
			// FIX: Do NOT update trailHead during unit propagation when s.level=0.
			// trailHead should only track decisions, not propagated variables.
			// If we append trailHead here, backtracking will incorrectly clear unit-propagated variables.
		if s.verbose {
			s.Log("c [UNIT PROP] var=%d, value=%v, level=%d (s.level=%d)\n", varIdx+1, litValue, propLevel, s.level)
		}
			s.assignments[varIdx] = Assignment{Value: litValue, Level: propLevel}
			s.varLevel[varIdx] = propLevel
			s.trail = append(s.trail, int(varIdx))
			// Store learned clause index as negative: -learnedIdx-5
			// Offset by 4 so clause 0 maps to -5, freeing -1/-2/-3/-4 as sentinels
			// (-1 decision, -2 unit-prop preprocess, -3 pure-literal preprocess, -4 reserved)
			s.implication[varIdx] = -learnedIdx - 5
			s.propagations++
		} else if s.assignments[varIdx].Value != litValue {
			// Conflict: unit learned clause conflicts with existing assignment
			existingIdx := s.implication[varIdx]
			existingLevel := s.assignments[varIdx].Level
			existingValue := s.assignments[varIdx].Value
		if s.verbose {
			s.Log("c [UNIT CONFLICT] var=%d, new=%v (idx=%d), existing=%v (level=%d, idx=%d)\n",
				varIdx+1, litValue, learnedIdx, existingValue, existingLevel, existingIdx)
		}
		if existingIdx != -1 {
			// Existing assignment is from propagation (not decision) - UNSAT!
			if existingIdx <= -5 {
				existingLearnedIdx := -existingIdx - 5
					existingLits := s.getLearnedClauseLiterals(existingLearnedIdx)
				if s.verbose {
					s.Log("c   Existing from learned clause %d: ", existingLearnedIdx)
					for _, l := range existingLits {
						s.Log("%d%c ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()])
						s.Log("\n")
					}
					s.Log("c   New unit clause %d: ", learnedIdx)
					for _, l := range literals {
						s.Log("%d%c ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()])
					}
					s.Log("\n")
				}
				}
			}
			s.emptyClauseFound = true
			s.conflictClauseBuf.Literals = literals
			s.conflictClauseBuf.Learned = true
			return true, &s.conflictClauseBuf
		}
	}

	// CRITICAL FIX: After unit propagation, reset qhead to process newly added trail elements
	// Without this, trail elements from unit clauses are never processed through watch lists
	if s.level < len(s.trailHead) {
		s.qhead = s.trailHead[s.level]
	} else {
		s.qhead = 0
	}

	// Use persistent qhead pointer (MiniSat-style) to avoid re-processing trail elements
	if s.qhead >= len(s.trail) {
		return false, nil
	}

	for trailIndex := s.qhead; trailIndex < len(s.trail); trailIndex++ {
		lit := s.trail[trailIndex]

		varIdx := uint32(lit)
		// CRITICAL FIX: Skip unassigned variables (level < 0)
		// Unassigned variables have Value=false by default, which incorrectly triggers watches
		if s.assignments[varIdx].Level < 0 {
			continue // Unassigned - skip watch processing
		}
		value := s.assignments[varIdx].Value

		// OPTIMIZATION: Inline LitToIndex - avoids function call overhead
		// lit index = varIdx * 2 + (1 if negated else 0)
		watchIdx := int(varIdx) << 1
		if value {
			watchIdx |= 1 // negated literal watches false when var is true
		}

		// Process watches for this literal using swap-with-last deletion
		watchList := s.watchLists[watchIdx]

		for readIdx := 0; readIdx < len(watchList); readIdx++ {
			watch := watchList[readIdx]

			// Get current clause literals - ZERO ALLOCATION (use slice views directly)
			// OPTIMIZATION: Eliminate make()/copy() for learned clauses - saves 570ms (5.6% of total)
			var clause *cnf.Clause
			var clauseLits []cnf.Literal
			if watch.ClauseIdx >= 0 {
				// Bounds check for safety
				if watch.ClauseIdx >= len(s.cnf.Clauses) {
					continue
				}
				clause = &s.cnf.Clauses[watch.ClauseIdx]
				clauseLits = clause.Literals
			} else {
				learnedIdx := -watch.ClauseIdx - 1
				// SAFETY CHECK: Skip deleted clauses (shouldn't happen if watch removal works correctly)
				if learnedIdx >= len(s.learnedSizes) || s.learnedSizes[learnedIdx] == 0 {
					continue
				}
				// OPTIMIZATION: Inline getLearnedClauseLiterals to eliminate function call overhead
				offset := s.learnedOffsets[learnedIdx]
				size := s.learnedSizes[learnedIdx]
				clauseLits = s.learnedLiterals[offset : offset+size]
			}
			blitIdx := watch.Blit
			// OPTIMIZATION: Removed verbose logging from hot path - reduces branch overhead

			// Inline IndexToLit
			blitVarIdx := blitIdx >> 1
			blitNegated := (blitIdx & 1) != 0

			// OPTIMIZATION 1B: Use varLevel cache instead of assignments[].Level
			blitLevel := s.varLevel[blitVarIdx]

			if blitLevel >= 0 {
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

				if litTrue || litLevel < 0 {
					// Found replacement - move watch from falseLit to clauseLit
					// OPTIMIZATION: Inline LitToIndex
					newWatchIdx := int(clauseLitVar) << 1
					if litNegated {
						newWatchIdx |= 1
					}

					// Add new watch to clauseLit's watch list
					// OPTIMIZATION: No Clause pointer - use ClauseIdx for all accesses
					s.watchLists[newWatchIdx] = append(s.watchLists[newWatchIdx], cnf.Watch{
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

			if blitLevel < 0 {
				// Unassigned blit - propagate it
				blitLit := cnf.IndexToLit(int(blitIdx))
				// CRITICAL FIX: Propagate at level 1 if s.level=0 (after restart)
				// This prevents search propagations from being confused with preprocessing assignments
				propLevel := s.level
				if propLevel == 0 {
					propLevel = 1
				}
				// Translate watch encoding (-learnedIdx-1) to implication encoding (-learnedIdx-5).
			// The two encodings diverge by the sentinel offset; original clauses pass through unchanged.
			reasonIdx := watch.ClauseIdx
			if reasonIdx < 0 {
				learnedIdx := -reasonIdx - 1
				reasonIdx = -learnedIdx - 5
			}
			s.assignLiteralByClause(blitLit, propLevel, reasonIdx)
				propagationCount++
				s.propagations++
				continue
			}

			// FIX: blitLevel == 0 means assigned by unit propagation - do NOT re-propagate
			// Previously this case propagated again, causing unit assignments to be overwritten

			// Re-check blit value - may have been assigned TRUE during replacement search
			blitValue := s.assignments[blitVarIdx].Value
			blitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)

			if !blitTrue {
				// Build conflict clause only when needed (avoid allocation in common case)
				var conflictClause *cnf.Clause
				if watch.ClauseIdx >= 0 {
					conflictClause = &s.cnf.Clauses[watch.ClauseIdx]
				} else {
					learnedIdx := -watch.ClauseIdx - 1
					literals := s.getLearnedClauseLiterals(learnedIdx)
					
					// VERIFY: Check that watched literals are actually in the clause
					watchLit := cnf.IndexToLit(int(watchIdx))
					blitLit := cnf.IndexToLit(int(blitIdx))
					hasWatch := false
					hasBlit := false
					for _, l := range literals {
						if l == watchLit {
							hasWatch = true
						}
						if l == blitLit {
							hasBlit = true
						}
					}
				if !hasWatch || !hasBlit {
					if s.verbose {
						s.Log("c [WATCH CORRUPTION] learnedIdx=%d: watch=%d%c blit=%d%c, clause=[",
							learnedIdx, watchLit.Var()+1, map[bool]byte{true: '-', false: '+'}[watchLit.IsNegated()],
							blitLit.Var()+1, map[bool]byte{true: '-', false: '+'}[blitLit.IsNegated()])
						for _, l := range literals {
							s.Log("%d%c ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()])
						}
						s.Log("]\n")
					}
				}
					
					// Reuse pre-allocated buffer (conflict is consumed before next propagation)
					s.conflictLitsBuf = s.conflictLitsBuf[:0]
					s.conflictLitsBuf = append(s.conflictLitsBuf, literals...)
					s.conflictClauseBuf.Literals = s.conflictLitsBuf
					s.conflictClauseBuf.Learned = true
					conflictClause = &s.conflictClauseBuf
				}

				// Conflict at level 0 = UNSAT (no decisions to backtrack)
				if s.level == 0 {
					s.emptyClauseFound = true
					s.watchLists[watchIdx] = watchList
					return true, conflictClause
				}
				if logEnabled {
					s.Log("c [PROP CONFLICT] Watch idx=%d, clauseIdx=%d, learnedIdx=%d, blit=%d, level=%d\n",
						watchIdx, watch.ClauseIdx, -watch.ClauseIdx-1, watch.Blit, s.level)
					if watch.ClauseIdx < 0 {
						learnedIdx := -watch.ClauseIdx - 1
						if learnedIdx < len(s.learnedSizes) {
							s.Log("c   Clause size=%d, LBD=%d\n", s.learnedSizes[learnedIdx], s.learnedMetadata[learnedIdx].LBD)
						}
						// Print conflict clause literals with levels
						s.Log("c   Conflict clause (s.level=%d): ", s.level)
						for _, cl := range conflictClause.Literals {
							s.Log("%d%c(L%d) ", cl.Var()+1, map[bool]byte{true: '-', false: '+'}[cl.IsNegated()], s.assignments[cl.Var()].Level)
						}
						s.Log("\n")
						// Count literals at current level
						atCurrentLevel := 0
						for _, cl := range conflictClause.Literals {
							if s.assignments[cl.Var()].Level == s.level {
								atCurrentLevel++
							}
						}
						s.Log("c   Literals at current level %d: %d (should be >= 1)\n", s.level, atCurrentLevel)
					}
				}
				s.watchLists[watchIdx] = watchList
				return true, conflictClause
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
				if litLevel < 0 {
					// Unassigned
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
				// CRITICAL FIX: Propagate at level 1 if s.level=0 (after restart)
				// This prevents search propagations from being confused with preprocessing assignments
				propLevel := s.level
				if propLevel == 0 {
					propLevel = 1
				}
				s.assignLiteralByClause(unassignedLit, propLevel, clauseIdx)
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
				if litLevel < 0 {
					// Unassigned
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
			// Store learned clause index as negative: -learnedIdx-5 (offset by 4, see implication comment)
			s.assignLiteralByClause(unassignedLit, s.level, -learnedIdx-5)
				unitPropagated = true
				// Track propagation count for this learned clause
				if learnedIdx < len(s.learnedMetadata) {
					s.learnedMetadata[learnedIdx].PropCount++
					s.learnedMetadata[learnedIdx].ScoreDirty = true
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

func (s *CDCLSolver) selectRandomUnassigned() uint32 {
	// Reuse persistent buffer - no allocation!
	s.tmpUnassignedVars = s.tmpUnassignedVars[:0]
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level < 0 {
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

		if s.verbose && s.conflicts%1000 == 0 {
			s.Log("c [verbose] Random decision at conflict %d, level %d (rate=%.2f)\n", s.conflicts, s.level, s.randomDecisionRate)
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
					if s.assignments[i].Level < 0 && !s.decidedVarSet[i] {
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

	// SAFETY CHECK: Ensure variable is unassigned before deciding.
	// The selection functions should already return unassigned variables,
	// but a stale heap entry can slip through. Fall back to a linear scan
	// for the first unassigned variable. NOTE: s.level has NOT been
	// incremented yet at this point, so it must not be modified here.
	if int(varIdx) < len(s.assignments) && s.assignments[varIdx].Level >= 0 {
		s.Log("c [DECIDE BUG] var %d already assigned at level %d, falling back to linear scan\n", varIdx+1, s.assignments[varIdx].Level)
		found := false
		for i := uint32(0); i < s.cnf.NumVars; i++ {
			if s.assignments[i].Level < 0 {
				varIdx = i
				if int(varIdx) < len(s.savedPhase) {
					phase = s.savedPhase[varIdx]
				} else {
					phase = true
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, phase), s.level, -1) // -1 = decision
	s.savedPhase[varIdx] = phase                                // Save phase for decisions only
	s.decisions++
	// SYMMETRY BREAKING: Track this decision to apply recency penalty
	s.vsids.TrackDecision(varIdx, s.conflicts)
	if s.verbose && s.conflicts <= 10 {
		// phase is the negated flag: true=negative lit (-var), false=positive lit (+var)
		// Variable value is !phase: if lit is -var, var=FALSE; if lit is +var, var=TRUE
		s.Log("c [DECIDE] Level %d (was %d): var %d = %v (decision, lit=%d%c), trailHead len=%d, trailHead=%v\n",
			s.level, s.level-1, varIdx+1, !phase, varIdx+1, map[bool]byte{true: '-', false: '+'}[phase], len(s.trailHead), s.trailHead)
	}
	return true
}

func (s *CDCLSolver) assignLiteral(lit cnf.Literal, level int, clauseIdx int) {
	varIdx := lit.Var()

	if s.assignments[varIdx].Level >= 0 {
		if s.verbose && level > 0 {
			reasonStr := "propagation"
			if clauseIdx == -1 {
				reasonStr = "decision"
			}
			s.Log("c [ALERT] assignLiteral: var %d already assigned at level %d, trying to assign at level %d (%s)\n",
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
	s.implication[varIdx] = clauseIdx

	if s.verbose && level > 0 {
		reasonStr := "propagation"
		if clauseIdx == -1 {
			reasonStr = "decision"
		}
		s.Log("c [ASSIGN] Level %d: var %d = %v (%s)", level, varIdx+1, value, reasonStr)
		if clauseIdx != -1 {
			if clauseIdx < 0 {
			s.Log(" from learned clause")
			} else {
			s.Log(" from original clause")
			}
		}
		s.Log("\n")
	}
}

// assignLiteralByClause assigns a literal with a clause as reason
// clauseIdx: >=0 for original clause, <=-5 for learned (-learnedIdx-5); -1 decision; -2/-3 preprocessing sentinels
func (s *CDCLSolver) assignLiteralByClause(lit cnf.Literal, level int, clauseIdx int) {
	varIdx := lit.Var()

	// CRITICAL FIX: Check implication instead of Level != 0
	// Level 0 can mean both "unassigned" AND "assigned at level 0" after backtracking
	// implication[varIdx] != -1 properly indicates the variable is already assigned
	if s.implication[varIdx] != -1 {
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value: value,
		Level: level,
	}
	s.varLevel[varIdx] = level
	s.trail = append(s.trail, int(varIdx))

	// Store clause index
	s.implication[varIdx] = clauseIdx
}

func (s *CDCLSolver) literalIsTrue(lit cnf.Literal) bool {
	assign := s.assignments[lit.Var()]
	if assign.Level < 0 {
		return false // Unassigned literals are not true
	}
	if lit.IsNegated() {
		return !assign.Value
	}
	return assign.Value
}

func (s *CDCLSolver) handleConflict(conflictClause *cnf.Clause) {
	s.conflicts++

	// If empty clause was already detected (e.g., during propagation), skip learning
	if s.emptyClauseFound {
		return
	}

	// Check if conflict clause is a unit that conflicts with an existing assignment
	// This indicates UNSAT (two conflicting unit clauses)
	if len(conflictClause.Literals) == 1 {
		lit := conflictClause.Literals[0]
		varIdx := lit.Var()
		litValue := !lit.IsNegated()
		if s.assignments[varIdx].Level != 0 || s.varLevel[varIdx] != 0 {
			if s.assignments[varIdx].Value != litValue {
				// Variable assigned with opposite value
				// Check if existing assignment was from a unit propagation (not a decision)
				impIdx := s.implication[varIdx]
				if impIdx != -1 {
					// Has an implication clause
					isUnit := false
				if impIdx <= -5 {
					// Learned clause
					learnedIdx := -impIdx - 5
						if learnedIdx < s.learnedCapacity && s.learnedSizes[learnedIdx] == 1 {
							isUnit = true
						}
				} else {
					// Original clause (guard against preprocessing sentinels -2/-3/-4)
					if impIdx >= 0 && impIdx < len(s.cnf.Clauses) && len(s.cnf.Clauses[impIdx].Literals) == 1 {
							isUnit = true
						}
					}
					if isUnit {
						// Both are unit propagations - conflicting units, UNSAT
											if s.verbose {
						s.Log("c [handleConflict] Conflicting units on var %d: existing=%v (from unit), new=%v - UNSAT\n", varIdx+1, s.assignments[varIdx].Value, litValue)
					}
						s.emptyClauseFound = true
						return
					}
				}
				// Existing assignment was a decision - not UNSAT, just a conflict
				// Continue to conflict analysis below (don't return here!)
			} else {
				// Variable already assigned with same value - redundant unit, skip
							if s.verbose {
				s.Log("c [handleConflict] Redundant unit on var %d - skipping\n", varIdx+1)
			}
				return
			}
		}
	}

	// Clear empty clause flag for new conflict analysis
	s.emptyClauseFound = false

	// Get the conflicting clause literals directly
	conflictLits := conflictClause.Literals

	// Bump activity for learned clause involved in conflict
	if conflictClause.Learned {
		// Find matching clause by comparing literals (expensive, so skip for now)
	}

	s.vsids.bumpClause(conflictLits, s.assignments)

	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	// Delete learned clauses when database exceeds dynamic limit
	// MiniSat-style: limit grows with conflicts to allow more learning on hard instances
	// Formula: base + conflicts/50 (MiniSat uses similar growth rate)
	// Trigger deletion at 150% of limit (MiniSat-style)
	dynamicLimit := s.maxLearned + s.conflicts/50
	if s.learnedActiveCount > dynamicLimit+dynamicLimit/2 {
		s.deleteLearnedClauses()
	}

	// Decay VSIDS activity every conflict (standard)
	s.vsids.decay(s.assignments)
	s.vsids.decayLBD()

	// OPTIMIZATION 2A: Lazy clause activity decay
	// Decay clause activity every 100 conflicts instead of every conflict
	// This reduces GC pressure and CPU overhead while maintaining search quality
	// Standard solvers (MiniSat, Glucose) use lazy decay for both variables and clauses
	if s.conflicts%100 == 0 {
		for i := range s.learnedMetadata {
			s.learnedMetadata[i].Activity *= ClauseActivityDecay
			s.learnedMetadata[i].ScoreDirty = true
		}
	}

	// Inprocessing runs at restart after inprocessingMinConflicts conflicts (default 1000)
	// See inprocessing() method and restart() integration
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
	if s.verbose && s.conflicts <= DebugConflictLimit {
		s.Log("c [conflict] Conflict %d, iter %d, level %d, learned %d, trail %d\n",
			s.conflicts, s.iterations, s.level, s.learnedActiveCount, len(s.trail))
	}

	// Fast cleanup from previous conflict
	for _, varIdx := range s.tmpTouchedVars {
		s.tmpLiteralInClause[varIdx] = false
		s.tmpLiteralIsNegated[varIdx] = false
	}
	for _, varIdx := range s.tmpResolvedVars {
		s.tmpResolved[varIdx] = false
	}
	s.tmpResolvedVars = s.tmpResolvedVars[:0]
	for _, lvl := range s.tmpLevelSet {
		s.tmpLevelCount[lvl] = 0
		s.tmpLevelSetUsed[lvl] = false
		s.tmpLevelCountUsed[lvl] = false
	}
	s.tmpTouchedVars = s.tmpTouchedVars[:0]
	s.tmpCandidates = s.tmpCandidates[:0]
	s.tmpLevelSet = s.tmpLevelSet[:0]

	// Add conflict clause literals
	if s.verbose {
		s.Log("c [1-UIP] ===== Conflict %d: %d literals at level %d =====\n", s.conflicts, len(conflictLits), s.level)
		for i, lit := range conflictLits {
			s.Log("c   INIT[%d]: var=%d, neg=%v, level=%d\n", i, lit.Var()+1, lit.IsNegated(), s.assignments[lit.Var()].Level)
		}
	}
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = true
			s.tmpLiteralIsNegated[varIdx] = lit.IsNegated()
			s.tmpTouchedVars = append(s.tmpTouchedVars, varIdx)
			lvl := s.assignments[varIdx].Level
			if lvl >= 0 && lvl <= s.level {
				// Ensure arrays are large enough for this level
				if lvl >= len(s.tmpLevelCountUsed) {
					newSize := lvl + 1
					newCountUsed := make([]bool, newSize)
					newLevelCount := make([]int, newSize)
					copy(newCountUsed, s.tmpLevelCountUsed)
					copy(newLevelCount, s.tmpLevelCount)
					s.tmpLevelCountUsed = newCountUsed
					s.tmpLevelCount = newLevelCount
				}
				if !s.tmpLevelCountUsed[lvl] {
					s.tmpLevelCountUsed[lvl] = true
					s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				}
				s.tmpLevelCount[lvl]++
			}
		}
	}

	// 1-UIP: Resolve until exactly 1 literal at current level
	currentCount := s.tmpLevelCount[s.level]

	// Build candidate list from trail (most recent first)
	s.tmpCandidates = s.tmpCandidates[:0]
	for i := len(s.trail) - 1; i >= 0; i-- {
		varIdx := uint32(s.trail[i])
		if s.varLevel[varIdx] == s.level && s.tmpLiteralInClause[varIdx] {
			s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx: varIdx, trailPos: i})
		}
	}

	// Resolve on candidates until 1 UIP remains
	candidateIdx := 0
	resolveStep := 0
	resolvedCount := 0
	for currentCount > 1 && candidateIdx < len(s.tmpCandidates) {
		candidate := s.tmpCandidates[candidateIdx]
		candidateIdx++
		varIdx := candidate.varIdx

		if s.tmpResolved[varIdx] {
			continue
		}

		reasonClauseIdx := s.implication[varIdx]
		if reasonClauseIdx == -1 {
			// Decision literal encountered - skip it (can't resolve on decisions)
			// The decision will remain in the clause as the UIP
			continue
		}
		// CRITICAL FIX: Don't resolve on level-0 literals (preprocessing assignments)
		// These are permanent assignments from unit propagation/pure literals
		// They have no meaningful "reason clause" for conflict analysis
		if s.assignments[varIdx].Level == 0 {
			if s.verbose && s.conflicts < 100 {
			s.Log("c [1-UIP] SKIP var %d: level-0 preprocessing assignment\n", varIdx+1)
			}
			continue
		}

		// Get reason clause literals
		var reasonLits []cnf.Literal
		if reasonClauseIdx <= -5 {
			// Learned clause
			learnedIdx := -reasonClauseIdx - 5
			if learnedIdx < s.learnedCapacity && s.learnedSizes[learnedIdx] > 0 {
				reasonLits = s.getLearnedClauseLiterals(learnedIdx)
			}
		} else {
			// Original clause
			if reasonClauseIdx < len(s.cnf.Clauses) {
				reasonLits = s.cnf.Clauses[reasonClauseIdx].Literals
			}
		}
		if reasonLits == nil {
			continue // Invalid clause
		}

		// DEBUG: Trace resolution step
		if s.verbose && s.conflicts >= 4990 {
			s.Log("c [1-UIP TRACE] Resolving on var %d (level %d)\n", varIdx+1, s.assignments[varIdx].Level)
			s.Log("c   Current clause: ")
			for v := range s.tmpLiteralInClause {
				if s.tmpLiteralInClause[v] {
			s.Log("%d%c ", v+1, map[bool]byte{true: '-', false: '+'}[s.tmpLiteralIsNegated[v]])
				}
			}
			s.Log("0\n")
			s.Log("c   Reason clause (idx=%d): ", reasonClauseIdx)
			for _, rl := range reasonLits {
			s.Log("%d%c ", rl.Var()+1, map[bool]byte{true: '-', false: '+'}[rl.IsNegated()])
			}
			s.Log("0\n")
		}

		// FIX: Skip reason clauses with unassigned literals
		// Such clauses are not valid conflict clauses
		hasUnassigned := false
		reasonAtCurrentLevel := 0
		for _, lit := range reasonLits {
			if s.assignments[lit.Var()].Level < 0 {
				hasUnassigned = true
				break
			}
			if s.assignments[lit.Var()].Level == s.level {
				reasonAtCurrentLevel++
			}
		}
		if hasUnassigned {
			if s.verbose && s.conflicts < 100 {
			s.Log("c [1-UIP] SKIP var %d: reason has unassigned literal\n", varIdx+1)
			}
			continue
		}
		// CRITICAL FIX: Skip learned clauses with unassigned literals (soundness bug)
		// Original clauses can have multiple lits at current level - always valid to resolve
		// Learned clauses with unassigned lits indicate corruption - skip them
		hasUnassignedInReason := false
		for _, rl := range reasonLits {
			if s.assignments[rl.Var()].Level < 0 {
				hasUnassignedInReason = true
				break
			}
		}
	if reasonClauseIdx < 0 && hasUnassignedInReason {
		if s.verbose {
			s.Log("c [1-UIP] SKIP var %d: learned clause %d has unassigned literal: ",
				varIdx+1, -reasonClauseIdx-1)
			for _, rl := range reasonLits {
				s.Log("%d%c(L%d) ", rl.Var()+1, map[bool]byte{true: '-', false: '+'}[rl.IsNegated()],
					s.assignments[rl.Var()].Level)
				s.Log("\n")
			}
		}
		continue
	}

		// Resolve: remove varIdx, add reason literals
		s.tmpLiteralInClause[varIdx] = false
		s.tmpResolved[varIdx] = true
		s.tmpResolvedVars = append(s.tmpResolvedVars, varIdx)
		s.tmpLevelCount[s.level]--
		currentCount--
		resolvedCount++

		for _, lit := range reasonLits {
			v := lit.Var()
			litNegated := lit.IsNegated()

			// Skip the resolved variable - its negation in the reason cancels with the original
			if v == varIdx {
				if s.verbose {
					s.Log("c [1-UIP]   Skip resolved var %d\n", v+1)
				}
				continue
			}
			if s.tmpLiteralInClause[v] {
				if s.tmpLiteralIsNegated[v] != litNegated {
					// Cancel: remove from clause
					if s.verbose {
						s.Log("c [1-UIP]   CANCEL var %d (neg=%v vs %v)\n", v+1, s.tmpLiteralIsNegated[v], litNegated)
					}
					s.tmpLiteralInClause[v] = false
					s.tmpLevelCount[s.varLevel[v]]--
					if s.varLevel[v] == s.level {
						currentCount--
					}
				} else {
					if s.verbose {
						s.Log("c [1-UIP]   Keep var %d (same polarity)\n", v+1)
					}
				}
			} else {
				// FIX: Only add assigned literals (level >= 0)
				// Unassigned literals (level < 0) cannot be part of a conflict clause
				assignLevel := s.assignments[v].Level
				if assignLevel < 0 {
					// Skip unassigned literal - it's not part of the conflict
					continue
				}
				
				// Add to clause
				if s.verbose {
					s.Log("c [1-UIP]   ADD var %d, neg=%v, level=%d\n", v+1, litNegated, assignLevel)
				}
				s.tmpLiteralInClause[v] = true
				s.tmpLiteralIsNegated[v] = litNegated
				s.tmpTouchedVars = append(s.tmpTouchedVars, v)
				lvl := assignLevel // Use assignment level, not varLevel cache
				if lvl <= s.level {
					if !s.tmpLevelCountUsed[lvl] {
						s.tmpLevelCountUsed[lvl] = true
						s.tmpLevelSet = append(s.tmpLevelSet, lvl)
					}
					s.tmpLevelCount[lvl]++
					if lvl == s.level {
						currentCount++
						// Find trail position
						trailPos := -1
						for i := len(s.trail) - 1; i >= 0; i-- {
							if uint32(s.trail[i]) == v {
								trailPos = i
								break
							}
						}
						s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx: v, trailPos: trailPos})
					}
				}
			}
		}
		resolveStep++
	}

	// FIX: If 1-UIP didn't converge (currentCount > 1) after exhausting all candidates,
	// check if remaining literals are all decisions or if some are propagations with buggy reasons
	decisionsAtCurrentLevel := 0
	propagationsAtCurrentLevel := 0
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] && s.assignments[varIdx].Level == s.level {
			if s.implication[varIdx] == -1 {
				decisionsAtCurrentLevel++
			} else {
				propagationsAtCurrentLevel++
			}
		}
	}

	if currentCount > 1 {
		// 1-UIP did not converge: there is more than one literal at the current
		// decision level remaining after resolution. In a correct CDCL this never
		// happens (each level has exactly one decision, so resolving non-decision
		// literals always reduces to that single decision = the UIP). Non-convergence
		// indicates inconsistent reason clauses (e.g. a propagated literal whose
		// reason contains an unassigned literal, or more than one decision at one
		// level), which were skipped above.
		//
		// SOUNDNESS FIX: do NOT drop literals to force a 1-UIP. Dropping literals
		// produces a clause that is NOT entailed by resolution, which has caused
		// incorrect UNSAT (wrong unit clauses like "var=x" learned on SAT instances).
		// The current clause is a valid resolvent of the conflict clause with the
		// reason clauses we were able to resolve, so keeping every literal is sound.
		// The clause may be non-asserting (several literals at the current level),
		// in which case the normal backjump to the second-highest level simply leaves
		// it non-unit; that is correct (just less effective) and never wrong.
		if s.verbose {
			s.Log("c [1-UIP] NON-CONVERGE: %d decisions + %d propagations remain at level %d; keeping all literals (sound, non-asserting)\n",
				decisionsAtCurrentLevel, propagationsAtCurrentLevel, s.level)
		}

		// Keep all literals. Do not modify currentCount beyond logging; the LBD and
		// backjump computations below handle a multi-literal current-level clause.
		// Reset level tracking so LBD is recomputed from scratch.
		for i := range s.tmpLevelSetUsed {
			s.tmpLevelSetUsed[i] = false
		}
	}

	// CRITICAL FIX: If 1-UIP resolved away ALL literals (currentCount == 0), we derived empty clause = UNSAT
	if currentCount == 0 {
		s.emptyClauseFound = true
		return 0
	}

	// Calculate LBD and backjump level
	// CRITICAL FIX: Level 0 is preprocessing - not a decision level for LBD
	lbd := 0
	maxLevel := 0
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			lvl := s.assignments[varIdx].Level
			if lvl > 0 {
				// Ensure arrays are large enough for this level
				if lvl >= len(s.tmpLevelSetUsed) {
					newSize := lvl + 1
					newSetUsed := make([]bool, newSize)
					copy(newSetUsed, s.tmpLevelSetUsed)
					s.tmpLevelSetUsed = newSetUsed
				}
				if !s.tmpLevelSetUsed[lvl] {
					s.tmpLevelSetUsed[lvl] = true
					s.tmpLevelSet = append(s.tmpLevelSet, lvl)
					lbd++
				}
			}
			if lvl > maxLevel && lvl < s.level {
				maxLevel = lvl
			}
		}
	}

	// Build learned clause
	s.tmpLearnedLits = s.tmpLearnedLits[:0]
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = false
			s.tmpLearnedLits = append(s.tmpLearnedLits, cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx]))
		}
	}

	// NOTE: resolvedCount == 0 is normal when all conflict literals are decisions.
	// The learned clause IS useful - it prevents this exact combination of decisions.
	// Do NOT skip learning in this case - that would cripple the solver.

	// CRITICAL: Verify 1-UIP property to catch soundness bugs
	// If verification fails, the learned clause is invalid - skip learning (safer than wrong clause)
	// NON-CONVERGE clauses (currentCount > 1) are sound but may have >1 literal at current level,
	// so check 3 is skipped for them.
	if !s.verifyLearnedClause(s.tmpLearnedLits, currentCount > 1) {
		if s.verbose {
			s.Log("c [learnClause] Skipping buggy learned clause due to 1-UIP violation\n")
		}
		return maxLevel
	}

	// Empty clause = UNSAT
	if len(s.tmpLearnedLits) == 0 {
		if s.verbose {
			s.Log("c [learnClause] Empty clause learned - UNSAT\n")
		}
		s.emptyClauseFound = true
		return 0
	}

	// CRITICAL FIX: Apply clause minimization via self-subsumption
	// This reduces learned clause size and LBD, enabling propagation
	// Disabled for clauses ≤2 literals (already minimal)
	originalSize := len(s.tmpLearnedLits)
	originalLBD := lbd
	if len(s.tmpLearnedLits) > 2 {
		s.tmpLearnedLits = s.minimizeLearnedClause(s.tmpLearnedLits)
		
		// Recalculate LBD after minimization (CRITICAL - LBD may have decreased)
		// Must reset tmpLevelSetUsed since it was used for original LBD calculation
		lbd = 0
		maxLevel = 0
		for i := range s.tmpLevelSetUsed {
			s.tmpLevelSetUsed[i] = false
		}
		for _, lit := range s.tmpLearnedLits {
			varIdx := lit.Var()
			lvl := s.assignments[varIdx].Level
			if lvl >= 0 {
				if lvl >= len(s.tmpLevelSetUsed) {
					newSize := lvl + 1
					newSetUsed := make([]bool, newSize)
					s.tmpLevelSetUsed = newSetUsed
				}
				if !s.tmpLevelSetUsed[lvl] {
					s.tmpLevelSetUsed[lvl] = true
					lbd++
				}
				if lvl > maxLevel && lvl < s.level {
					maxLevel = lvl
				}
			}
		}
		
		if s.verbose && originalSize > len(s.tmpLearnedLits) {
			s.Log("c [minimize] Reduced: %d→%d literals, LBD %d→%d\n",
				originalSize, len(s.tmpLearnedLits), originalLBD, lbd)
		}
	}

	// Backjump level = second-highest in learned clause (= maxLevel)
	// SPECIAL CASE: If learned clause is unit (1 literal) at current level, backjump to level 0
	// to flip the decision. The unit literal represents a constraint that must be satisfied.
	backjumpLevel := maxLevel

	if s.verbose && s.conflicts <= 10 {
		s.Log("c [LEARNED CLAUSE] ")
		for _, lit := range s.tmpLearnedLits {
			s.Log("%d%c ", lit.Var()+1, map[bool]byte{true: '-', false: '+'}[lit.IsNegated()])
		}
		s.Log("0 (LBD=%d, backjump=%d)\n", lbd, backjumpLevel)
	}
	if len(s.tmpLearnedLits) == 1 {
		// Unit clause: check if the literal's variable is at current level
		lit := s.tmpLearnedLits[0]
		if s.assignments[lit.Var()].Level == s.level {
			// Unit literal at current level: backjump to 0 to flip the decision
			backjumpLevel = 0
		} else if backjumpLevel == 0 {
			backjumpLevel = 1
		}
	} else if backjumpLevel == 0 {
		backjumpLevel = 1
	}

	if s.verbose {
		s.Log("c   FINAL: %d literals, LBD=%d, backjump=%d\n", len(s.tmpLearnedLits), lbd, backjumpLevel)
	}

	// Check for tautologies (both polarities of same variable)
	// This can happen due to bugs in conflict analysis
	for _, lit := range s.tmpLearnedLits {
		varIdx := lit.Var()
		if s.tmpSeenVar[varIdx] {
			// Tautology detected - skip learning this clause
			if s.verbose {
				s.Log("c [learnClause] TAUTOLOGY detected in learned clause - skipping\n")
			}
			// Reset before returning
			for _, l := range s.tmpLearnedLits {
				s.tmpSeenVar[l.Var()] = false
			}
			return backjumpLevel
		}
		s.tmpSeenVar[varIdx] = true
	}
	// Reset
	for _, lit := range s.tmpLearnedLits {
		s.tmpSeenVar[lit.Var()] = false
	}

	// Check for conflicting unit clauses
	if len(s.tmpLearnedLits) == 1 {
		lit := s.tmpLearnedLits[0]
		varIdx := lit.Var()
		litValue := !lit.IsNegated()
		if s.verbose {
			s.Log("c [learnClause] Learning unit: var=%d, value=%v\n", varIdx+1, litValue)
		}
		// Check if opposite unit already exists
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedSizes[i] != 1 {
				continue
			}
			existingLits := s.getLearnedClauseLiterals(i)
			if len(existingLits) != 1 {
				continue
			}
			existingLit := existingLits[0]
			if existingLit.Var() == varIdx {
				existingValue := !existingLit.IsNegated()
				if existingValue != litValue {
					// Conflicting unit found - UNSAT
										if s.verbose {
						s.Log("c [learnClause] Conflicting unit on var %d: existing=%v, new=%v - UNSAT\n", varIdx+1, existingValue, litValue)
					}
					s.emptyClauseFound = true
					return 0
				}
			}
		}
	}

	// Store learned clause in database
	if len(s.tmpLearnedLits) > 0 && lbd <= 8 {
		// Check for duplicate literals (SOUNDNESS CHECK)
		hasDup := false
		for _, lit := range s.tmpLearnedLits {
			if s.tmpSeenVar[lit.Var()] {
				hasDup = true
				break
			}
			s.tmpSeenVar[lit.Var()] = true
		}
		// Reset
		for _, lit := range s.tmpLearnedLits {
			s.tmpSeenVar[lit.Var()] = false
		}
		if hasDup {
			if s.verbose {
				s.Log("c [SOUNDNESS BUG] Learned clause has duplicate literals: conflict=%d, clause: ", s.conflicts)
				for _, lit := range s.tmpLearnedLits {
					s.Log("%d%c ", lit.Var()+1, map[bool]byte{true: '-', false: '+'}[lit.IsNegated()])
					s.Log("\n")
				}
			}
			// Skip storing this buggy clause
			return 0
		}

		// Store literals in contiguous pool
		// NOTE: Slot reuse disabled - swap-remove moves clauses but literals stay in place,
		// causing corruption when freed slots are reused
		offset := len(s.learnedLiterals)
		s.learnedLiterals = append(s.learnedLiterals, make([]cnf.Literal, len(s.tmpLearnedLits))...)

		copy(s.learnedLiterals[offset:offset+len(s.tmpLearnedLits)], s.tmpLearnedLits)

		// Append metadata (packed struct for cache efficiency)
		s.learnedOffsets = append(s.learnedOffsets, offset)
		s.learnedSizes = append(s.learnedSizes, len(s.tmpLearnedLits))
		s.learnedMetadata = append(s.learnedMetadata, cnf.ClauseMetadata{
			Age:        s.currentAge,
			Size:       len(s.tmpLearnedLits),
			LBD:        lbd,
			Activity:   0.0,
			UseCount:   0,
			PropCount:  0,
			Score:      0.0,
			ScoreDirty: true,
		})
		if lbd > 3 {
			s.normalClauseCount++
		}
		s.currentAge++
		s.learnedActiveCount++
		s.learnedCapacity++

		// Add to watches
		learnedIdx := s.learnedActiveCount - 1
		literals := s.getLearnedClauseLiterals(learnedIdx)
		
		// Store watch indices for all clauses to maintain array consistency
		if len(literals) >= 2 {
			idx0 := cnf.LitToIndex(literals[0])
			idx1 := cnf.LitToIndex(literals[1])
			s.learnedWatchIdx0 = append(s.learnedWatchIdx0, idx0)
			s.learnedWatchIdx1 = append(s.learnedWatchIdx1, idx1)
			
			if s.watchInitialized {
				tmpClause := &cnf.Clause{Literals: literals, Learned: true}
				s.addLearnedClauseToWatches(learnedIdx, tmpClause, literals)
			}
		} else {
			// Unit clause: use sentinel values
			s.learnedWatchIdx0 = append(s.learnedWatchIdx0, -1)
			s.learnedWatchIdx1 = append(s.learnedWatchIdx1, -1)
		}

		// Track unit clauses for O(1) propagation (OPTIMIZATION #1)
		if len(literals) == 1 {
			s.unitLearnedList = append(s.unitLearnedList, learnedIdx)
		}

		// VSIDS bump
		s.vsids.bumpLBD(s.tmpLearnedLits, lbd)
	}

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
// When learned clause count exceeds maxLearned (default 200), delete down to
// minLearned (default 100) - aggressive 50% reduction (MiniSat-style).

// Scoring Formula:
// score = age*10 + LBD*50 + size*5 - activity*20 + bonuses/penalties
// Higher score = more likely to delete

// minimizeLearnedClause reduces the size of a learned clause via self-subsumption
// Aggressive minimization: always attempt for clauses > 2 literals
func (s *CDCLSolver) minimizeLearnedClause(learnedLits []cnf.Literal) []cnf.Literal {
	// EARLY TERMINATION: Clauses ≤2 literals are already minimal
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

	// Single-pass minimization: try to remove each literal via self-subsumption
	reductionAchieved := 0
	attemptedRemovals := 0
	for _, lit := range learnedLits {
		varIdx := lit.Var()

		// Skip if already removed
		if !s.tmpLiteralInClause[varIdx] {
			continue
		}

		canRemove := false
		reasonClauseIdx := s.implication[varIdx]
		var reasonLits []cnf.Literal
		if reasonClauseIdx >= 0 {
			// Original clause
			if reasonClauseIdx < len(s.cnf.Clauses) {
				reasonLits = s.cnf.Clauses[reasonClauseIdx].Literals
			}
		} else if reasonClauseIdx <= -5 {
			// Learned clause
			learnedIdx := -reasonClauseIdx - 5
			if learnedIdx < s.learnedCapacity && s.learnedSizes[learnedIdx] > 0 {
				reasonLits = s.getLearnedClauseLiterals(learnedIdx)
			}
		}
		// else: -2/-3/-4 preprocessing sentinels — no resolvable reason clause

		if reasonLits != nil {
			attemptedRemovals++
			// Check if all reason literals (except varIdx) are in the learned clause
			// This is self-subsumption: if (A ∨ B ∨ C) and reason for A is (A ∨ D),
			// and D is in learned clause, we can remove A
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

		if canRemove {
			s.tmpLiteralInClause[varIdx] = false
			reductionAchieved++
		}
	}

	// Compact in-place: keep only literals whose tmpLiteralInClause is still true
	writeIdx := 0
	for _, lit := range learnedLits {
		if s.tmpLiteralInClause[lit.Var()] {
			learnedLits[writeIdx] = lit
			writeIdx++
		}
	}

	if s.verbose && reductionAchieved > 0 {
		s.Log("c [minimize] Reduced: %d→%d literals (removed %d)\n",
			len(learnedLits), writeIdx, reductionAchieved)
	}

	return learnedLits[:writeIdx]
}

// computeClauseScore calculates the deletion score for a clause
// Higher score = more likely to delete
// Used for incremental scoring optimization
func (s *CDCLSolver) computeClauseScore(idx int) float64 {
	lbd := s.learnedMetadata[idx].LBD
	size := s.learnedSizes[idx]
	age := s.currentAge - s.learnedMetadata[idx].Age
	activity := s.learnedMetadata[idx].Activity
	useCount := s.learnedMetadata[idx].UseCount
	propCount := s.learnedMetadata[idx].PropCount

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

	// Protection for glue clauses and ALL unit clauses
	if size == 1 {
		// NEVER delete unit clauses - they are global constraints
		score = -100000.0
	} else if lbd <= s.coreGlueLBDThreshold {
		score = -10000.0
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

	return score
}

// markClauseDirty marks a clause's score as needing recomputation
// Called when clause activity, useCount, or propCount changes
func (s *CDCLSolver) markClauseDirty(idx int) {
	if idx >= 0 && idx < len(s.learnedMetadata) {
		s.learnedMetadata[idx].ScoreDirty = true
	}
}

// updateScoresIncrementally recomputes scores only for dirty clauses
// This is O(dirty clauses) instead of O(all clauses)
func (s *CDCLSolver) updateScoresIncrementally() {
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedSizes[i] == 0 {
			continue // Skip tombstones
		}
		if s.learnedMetadata[i].ScoreDirty {
			s.learnedMetadata[i].Score = s.computeClauseScore(i)
			s.learnedMetadata[i].ScoreDirty = false
		}
	}
}

func (s *CDCLSolver) deleteLearnedClauses() {
	// LAZY LBD-BASED DELETION (Glucose-style)
	// Key insight: LBD is the best predictor of clause usefulness
	// - Keep all "glue" clauses (LBD ≤ 3) permanently
	// - Delete clauses with high LBD when database grows too large
	
	// Count current active clauses
	dynamicLimit := s.maxLearned + s.conflicts/50
	targetCount := dynamicLimit
	
	currentActive := 0
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedSizes[i] > 0 {
			currentActive++
		}
	}
	
	toDelete := currentActive - targetCount
	if toDelete <= 0 {
		return // Nothing to delete
	}
	
	// Mark protected clauses (used as implications) using bitmap
	// This avoids O(n*m) scanning
	if cap(s.tmpClauseUsedAsReason) < s.learnedCapacity {
		s.tmpClauseUsedAsReason = make([]bool, s.learnedCapacity)
	}
	protected := s.tmpClauseUsedAsReason[:s.learnedCapacity]
	for i := range protected {
		protected[i] = false
	}
	
	for _, impIdx := range s.implication {
		if impIdx <= -5 {
			learnedIdx := -impIdx - 5
			if learnedIdx < s.learnedCapacity {
				protected[learnedIdx] = true
			}
		}
	}
	
	// Use tmp buffer for deletion marks
	if cap(s.tmpDeleted) < s.learnedCapacity {
		s.tmpDeleted = make([]bool, s.learnedCapacity)
	}
	deleted := s.tmpDeleted[:s.learnedCapacity]
	for i := range deleted {
		deleted[i] = false
	}
	
	deletedCount := 0
	
	// Delete clauses with LBD > 5 first (aggressive but safe)
	for i := 0; i < s.learnedCapacity && deletedCount < toDelete; i++ {
		if s.learnedSizes[i] == 0 || protected[i] {
			continue
		}
		
		if s.learnedMetadata[i].LBD > 5 {
			deleted[i] = true
			deletedCount++
		}
	}
	
	// If still need to delete more, lower threshold to LBD > 3
	if deletedCount < toDelete {
		for i := 0; i < s.learnedCapacity && deletedCount < toDelete; i++ {
			if s.learnedSizes[i] == 0 || protected[i] || deleted[i] {
				continue
			}
			
			if s.learnedMetadata[i].LBD > 3 {
				deleted[i] = true
				deletedCount++
			}
		}
	}
	
	// Apply tombstones: mark size=0 and remove watches
	tombstoneCount := 0
	for i := 0; i < s.learnedCapacity; i++ {
		if deleted[i] && s.learnedSizes[i] > 0 {
			// Remove watches BEFORE marking as tombstone
			s.removeLearnedClauseWatches(i)
			// Mark as tombstone
			s.learnedSizes[i] = 0
			s.learnedWatchIdx0[i] = -1
			s.learnedWatchIdx1[i] = -1
			tombstoneCount++
		}
	}

	// Update active count (excludes tombstones)
	activeCount := 0
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedSizes[i] > 0 {
			activeCount++
		}
	}
	s.learnedActiveCount = activeCount

	// Rebuild unit clause list from scratch
	s.unitLearnedList = s.unitLearnedList[:0]
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedSizes[i] == 1 {
			s.unitLearnedList = append(s.unitLearnedList, i)
		}
	}

	// Mark LBD order as dirty
	s.lbdOrderDirty = true

	// WATCH LIST COMPACTION: Remove watches for deleted clauses (tombstones)
	// This reduces propagation overhead by shrinking watch lists
	s.compactWatchLists()
	
	// Track tombstone ratio for logging
	tombstoneRatio := float64(tombstoneCount) / float64(s.learnedCapacity)

	// Request compaction at the next restart (level 0, where no learned
	// clause is in use as a reason) when too many tombstones have accumulated.
	// compactLearnedClauses reclaims the literal storage that tombstones leave
	// behind, bounding learnedLiterals growth on long-running instances.
	// Use ACCUMULATED tombstones (capacity - active), not just this call's
	// deletions: capacity grows monotonically while tombstones pile up, so the
	// per-call count understates the real waste.
	accumulated := s.learnedCapacity - s.learnedActiveCount
	if s.learnedCapacity > 0 && accumulated*3 >= s.learnedCapacity*2 { // >= ~33% tombstones
		s.compactPending = true
	}

		s.Log("c [verbose] Deleted %d learned clauses via LBD, kept %d active (tombstones=%d, ratio=%.1f%%)\n",
		deletedCount, activeCount, tombstoneCount, tombstoneRatio*100)
}

// compactLearnedClauses rebuilds all learned clause arrays to remove tombstones
// This is called periodically when tombstone ratio exceeds threshold
func (s *CDCLSolver) compactLearnedClauses() {
	s.Log("c [compact] Compacting learned clauses: capacity=%d, active=%d\n",
			s.learnedCapacity, s.learnedActiveCount)


	// Build clause index mapping (old -> new compacted index)
	if cap(s.tmpClauseIndexMap) < s.learnedCapacity {
		s.tmpClauseIndexMap = make([]int, s.learnedCapacity)
	}
	clauseIndexMap := s.tmpClauseIndexMap[:s.learnedCapacity]
	for i := range clauseIndexMap {
		clauseIndexMap[i] = -1
	}

	// Compact clauses: move active clauses to contiguous positions
	writeIdx := 0
	nextOffset := 0
	
	for readIdx := 0; readIdx < s.learnedCapacity; readIdx++ {
		if s.learnedSizes[readIdx] == 0 {
			continue // Skip tombstones
		}

		// Record mapping
		clauseIndexMap[readIdx] = writeIdx

		// Move clause metadata
		oldStart := s.learnedOffsets[readIdx]
		oldSize := s.learnedSizes[readIdx]
		newStart := nextOffset
		
		s.learnedOffsets[writeIdx] = newStart
		s.learnedSizes[writeIdx] = oldSize
		s.learnedMetadata[writeIdx] = s.learnedMetadata[readIdx]
		s.learnedWatchIdx0[writeIdx] = s.learnedWatchIdx0[readIdx]
		s.learnedWatchIdx1[writeIdx] = s.learnedWatchIdx1[readIdx]

		// Copy literals
		copy(s.learnedLiterals[newStart:newStart+oldSize], s.learnedLiterals[oldStart:oldStart+oldSize])
		
		writeIdx++
		nextOffset += oldSize
	}

	// Update implication array using the mapping
	for varIdx := range s.implication {
		if s.implication[varIdx] <= -5 {
			learnedIdx := -s.implication[varIdx] - 5
			if learnedIdx < len(clauseIndexMap) && clauseIndexMap[learnedIdx] >= 0 {
				s.implication[varIdx] = -clauseIndexMap[learnedIdx] - 5
			} else if learnedIdx < len(clauseIndexMap) {
				// Clause was deleted - reset to decision
				s.implication[varIdx] = -1
			}
		}
	}

	// REBUILD ALL WATCH LISTS FROM SCRATCH (correct and simple)
	// CRITICAL: use chooseWatchPositions (via addOriginalClauseToWatches /
	// addLearnedClauseToWatches) so watched literals are chosen to NOT both
	// be false under the current assignment. Watching literals[0]/[1]
	// blindly violates the watched-literal invariant and causes missed
	// propagations/conflicts (soundness bug).
	s.watchLists = make([][]cnf.Watch, len(s.watchLists))

	// First, add original clauses
	for i := 0; i < len(s.cnf.Clauses); i++ {
		clause := &s.cnf.Clauses[i]
		if len(clause.Literals) < 2 {
			continue
		}
		s.addOriginalClauseToWatches(i, clause, clause.Literals)
	}

	// Then, add learned clauses (and store the chosen watch literal indices)
	for i := 0; i < writeIdx; i++ {
		if s.learnedSizes[i] < 2 {
			continue
		}
		literals := s.getLearnedClauseLiterals(i)
		if len(literals) < 2 {
			continue
		}
		watch0, watch1 := s.chooseWatchPositions(literals)
		if watch0 < 0 || watch1 < 0 {
			s.learnedWatchIdx0[i] = -1
			s.learnedWatchIdx1[i] = -1
			continue
		}
		idx0 := cnf.LitToIndex(literals[watch0])
		idx1 := cnf.LitToIndex(literals[watch1])
		clauseIdx := -i - 1

		s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
			ClauseIdx: clauseIdx,
			Blit:      uint32(idx1),
		})
		s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
			ClauseIdx: clauseIdx,
			Blit:      uint32(idx0),
		})

		// Update stored watch indices to the chosen literals
		s.learnedWatchIdx0[i] = idx0
		s.learnedWatchIdx1[i] = idx1
	}
	
	// Mark watches as initialized
	s.watchInitialized = true

	// Truncate arrays to new capacity
	s.learnedLiterals = s.learnedLiterals[:nextOffset]
	s.learnedOffsets = s.learnedOffsets[:writeIdx]
	s.learnedSizes = s.learnedSizes[:writeIdx]
	s.learnedMetadata = s.learnedMetadata[:writeIdx]
	s.learnedWatchIdx0 = s.learnedWatchIdx0[:writeIdx]
	s.learnedWatchIdx1 = s.learnedWatchIdx1[:writeIdx]
	s.learnedCapacity = writeIdx
	s.learnedActiveCount = writeIdx

	// Rebuild unit list
	s.unitLearnedList = s.unitLearnedList[:0]
	for i := 0; i < writeIdx; i++ {
		if s.learnedSizes[i] == 1 {
			s.unitLearnedList = append(s.unitLearnedList, i)
		}
	}

	s.Log("c [compact] Compaction complete: new capacity=%d, literals=%d\n",
			writeIdx, nextOffset)

	// Debug-build only: validate that all clause references (implications,
	// watches, unit list) are consistent after the rebuild. No-op in release.
	verifyClauseIndices(s)
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

// compactWatchLists removes watches for deleted learned clauses (tombstones)
// This is called after deleteLearnedClauses to clean up watch lists
// 
// Why needed: When learned clauses are deleted (marked as tombstones), their watches
// remain in watch lists, causing unnecessary iteration during propagation.
// This removes watches for clauses with size=0 (permanently deleted).
func (s *CDCLSolver) compactWatchLists() {
	s.Log("c [compact] Compacting watch lists: removing deleted clause watches\n")

	
	removedCount := 0
	
	// Scan each watch list and remove watches for deleted learned clauses
	for litIdx := range s.watchLists {
		watchList := s.watchLists[litIdx]
		if len(watchList) == 0 {
			continue
		}
		
		// Compact in-place: move active watches forward
		writeIdx := 0
		for readIdx := range watchList {
			watch := watchList[readIdx]
			
			// Check if learned clause is deleted (tombstone)
			shouldRemove := false
			if watch.ClauseIdx < 0 {
				learnedIdx := -watch.ClauseIdx - 1
				// Check if this learned clause is a tombstone (deleted)
				if learnedIdx < s.learnedCapacity && s.learnedSizes[learnedIdx] == 0 {
					shouldRemove = true
				}
			}
			// Note: We never remove original clauses from watch lists
			
			// Keep watch if clause is still active
			if !shouldRemove {
				if writeIdx != readIdx {
					watchList[writeIdx] = watch
				}
				writeIdx++
			} else {
				removedCount++
			}
		}
		
		// Truncate watch list
		if writeIdx < len(watchList) {
			s.watchLists[litIdx] = watchList[:writeIdx]
		}
	}
	
	s.Log("c [compact] Watch list compaction: removed %d watches for deleted clauses\n", removedCount)

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
			s.Log("c [BACKTRACK] Empty clause found - returning UNSAT\n")
		return false
	}

	if len(s.trailHead) <= 1 {
			s.Log("c [BACKTRACK] Returning false: trailHead len=%d\n", len(s.trailHead))
		return false
	}

	// Use backjump level if available, otherwise backtrack one level
	bjLevel := s.backjumpLevel
	if bjLevel < 0 {
		bjLevel = s.level - 1
	}
	// CRITICAL FIX: Allow bjLevel == s.level for unit clause flips
	// When bjLevel == s.level, we stay at current level and flip the decision
	// This is correct for unit learned clauses where the UIP is the decision literal
	// Only adjust if bjLevel > s.level (which would be a bug)
	if bjLevel > s.level {
		bjLevel = s.level - 1
	}
	// Allow bjLevel=0 to backtrack to root level (needed for unit clause conflicts with decisions)
	if bjLevel < 0 {
			s.Log("c [BACKTRACK] bjLevel=%d invalid at level %d - returning UNSAT\n", bjLevel, s.level)
		return false
	}

	// Find the decision point at the backjump level
	// SPECIAL CASE: If bjLevel=0, we're backtracking to root, so clear all decisions (start from trailHead[1])
	var decisionPoint int
	if bjLevel == 0 {
		if len(s.trailHead) > 1 {
			decisionPoint = s.trailHead[1]
		} else {
			decisionPoint = 0
		}
	} else {
		decisionPoint = s.trailHead[bjLevel]
	}
	if decisionPoint >= len(s.trail) {
			s.Log("c [BACKTRACK] FAIL: decision point %d >= trail len %d at conflict %d\n", decisionPoint, len(s.trail), s.conflicts)
		return false
	}

	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value

	if s.verbose && s.conflicts <= 10 {
		s.Log("c [BACKTRACK] Flipping var %d (decision at trail pos %d, level %d) from %v to %v\n",
			decisionVar+1, decisionPoint, s.assignments[decisionVar].Level, decisionValue, !decisionValue)
	}

	// Clear all assignments from decisionPoint onwards
	// FIX: Skip ONLY preprocessing assignments (Level == 0 AND implication <= -2)
	// Search propagations have Level >= 1 even though implication < -1, and MUST be cleared
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		// Skip preprocessing: Level == 0 (unit prop or pure literal)
		if s.assignments[varIdx].Level == 0 && s.implication[varIdx] <= -2 {
			continue
		}
		s.assignments[varIdx] = Assignment{Level: -1}
		s.varLevel[varIdx] = -1
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
	// FIX: Reset qhead to 0 to re-scan entire trail after backjump
	// This ensures watches on newly learned clauses are checked against all assigned variables
	s.qhead = 0
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel

	// SAFETY: Clear any variables with Level > bjLevel that aren't in the trail
	// This catches bugs where trail was truncated without clearing assignments
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level > bjLevel {
			inTrail := false
			for _, t := range s.trail {
				if uint32(t) == i {
					inTrail = true
					break
				}
			}
			if !inTrail {
				s.assignments[i] = Assignment{Level: -1}
				s.varLevel[i] = -1
				s.implication[i] = -1
			}
		}
	}

	// Reset conflicts at levels > bjLevel since we're backtracking
	for i := bjLevel + 1; i < len(s.conflictsAtLevel); i++ {
		s.conflictsAtLevel[i] = 0
	}

	// SPECIAL CASE: If bjLevel=0, we've backtracked to root. Don't flip a decision -
	// the unit clause will be propagated in the next iteration.
	if bjLevel == 0 {
		return true
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

	if s.verbose && s.conflicts <= 10 {
		s.Log("c [BACKTRACK POST] trail len=%d, trailHead=%v, s.level=%d\n", len(s.trail), s.trailHead, s.level)
		for i, t := range s.trail {
			v := uint32(t)
			s.Log("c   trail[%d] = var %d (level %d)\n", i, v+1, s.assignments[v].Level)
		}
	}

	return true
}

// getReasonLBD returns the LBD of a variable's reason clause
// For original clauses: returns size (approximation, original clauses don't have LBD tracking)
// For learned clauses: returns stored LBD
// Used during 1-UIP analysis to prefer resolving with low-LBD reason clauses
func (s *CDCLSolver) getReasonLBD(varIdx uint32) int {
	reasonClauseIdx := s.implication[varIdx]
	if reasonClauseIdx == -1 {
		return 0 // Decision, no reason
	}

	if reasonClauseIdx <= -5 {
		// Learned clause - get LBD directly
		learnedIdx := -reasonClauseIdx - 5
		if learnedIdx < len(s.learnedMetadata) {
			return s.learnedMetadata[learnedIdx].LBD
		}
		return 999
	}

	// Original clause - use size as approximation
	if reasonClauseIdx >= 0 && reasonClauseIdx < len(s.cnf.Clauses) {
		return len(s.cnf.Clauses[reasonClauseIdx].Literals)
	}
	return 999
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
		return s.learnedMetadata[s.learnedClauseOrder[i]].LBD < s.learnedMetadata[s.learnedClauseOrder[j]].LBD
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
		s.Log("c Using plain DPLL algorithm (no clause learning)\n")

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

// verifyLearnedClause checks that a learned clause is sound.
// Checks 1-2 (all assigned, all false) always run. Check 3 (exactly 1 literal
// at current level) is skipped when allowMultipleAtCurrentLevel is true, which
// is the case for NON-CONVERGE clauses that are sound but non-asserting.
func (s *CDCLSolver) verifyLearnedClause(learnedLits []cnf.Literal, allowMultipleAtCurrentLevel bool) bool {
	if len(learnedLits) == 0 {
		return true // Empty clause is valid (means UNSAT)
	}

	// Check 1: All literals must be assigned (no unassigned literals)
	for _, lit := range learnedLits {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level < 0 {
			s.Log("c [SOUNDNESS BUG] Learned clause has unassigned literal: conflict=%d, var=%d\n",
				s.conflicts, varIdx+1)
			return false
		}
	}

	// Check 2: Learned clause must be falsified by current assignment
	for _, lit := range learnedLits {
		varIdx := lit.Var()
		litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
		if litTrue {
			s.Log("c [SOUNDNESS BUG] Learned clause has TRUE literal: conflict=%d, var=%d\n",
				s.conflicts, varIdx+1)
			return false
		}
	}

	// Check 3: 1-UIP property - EXACTLY 1 literal at current level.
	// Skipped for NON-CONVERGE clauses (sound but non-asserting, may have >1).
	if !allowMultipleAtCurrentLevel {
		literalsAtCurrentLevel := 0
		for _, lit := range learnedLits {
			if s.assignments[lit.Var()].Level == s.level {
				literalsAtCurrentLevel++
			}
		}
		if literalsAtCurrentLevel != 1 {
			s.Log("c [SOUNDNESS BUG] 1-UIP violation: conflict=%d, level=%d, literals_at_level=%d (expected exactly 1)\n",
				s.conflicts, s.level, literalsAtCurrentLevel)
			return false
		}
	}

	return true
}
