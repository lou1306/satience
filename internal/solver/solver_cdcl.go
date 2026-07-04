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
//   - Preprocessing
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
	cnf             *cnf.CNF
	assignments     []Assignment
	trail           []int
	preprocessTrail []int // Permanent preprocessing assignments (Level 0, never cleared/backtracked)
	varLevel        []int // Cache of variable levels (avoids random assignments[].Level access)
	trailHead       []int
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
	learnedActiveCount   int                   // Number of active clauses (excludes tombstones)
	learnedCapacity      int                   // Total capacity including tombstones
	currentAge           int
	verbose              bool
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
	randomDecisionPeriod int             // Period for forced random decisions (default 0=disabled, causes O(n) overhead)
	randomSeed           uint64          // Seed for deterministic random selection
	lastDecisionVar      uint32          // Last variable chosen for decision
	consecutiveFlips     int             // Count of consecutive decisions on same variable
	unitLearnedList      []int           // List of learned clause indices that are unit clauses (for O(1) propagation)
	// Exploration diversity tracking (IMPROVEMENT #3)
	decidedVars               []uint32 // Variables decided during current search phase
	decidedVarSet             []bool   // Fast lookup for decided variables
	restartDecisionCount      int      // Decisions since last restart (for diversity reset)
	minimizeMaxDepth   int      // Max recursion depth for recursive clause minimization (default 100)
	vivifyPeriod       int      // Run vivification every Nth restart (0=disabled, default 50)
	vivifyEnabled      bool     // Whether vivification is enabled (adaptive: structured instances only)
	inVivification     bool     // True during vivification trial propagation (suppresses false UNSAT from unit scan)
	// Reusable buffers for conflict analysis (avoid per-conflict allocation)
	tmpLiteralInClause   []bool
	tmpSeenVar           []bool // Pre-allocated bitset for duplicate/tautology checks (replaces per-conflict maps)
	tmpLiteralIsNegated  []bool
	tmpLevelCount        []int
	tmpLevelCountUsed    []bool // Track which levels have non-zero tmpLevelCount
	tmpCandidates        []resolveCandidate
	tmpLevelSet          []int         // For LBD calculation (replaces map)
	tmpLevelSetUsed      []bool        // Track which levels are in tmpLevelSet
	tmpResolved          []bool        // Track resolved variables in 1-UIP to prevent re-resolution cycles
	tmpResolvedVars      []uint32      // Track which variables were resolved (for fast reset)
	tmpFlippedVars       []bool        // Track flipped variables at level 1 to prevent infinite loops
	tmpTouchedVars       []uint32      // Track which variables were modified (for fast reset)
	tmpUnassignedVars    []uint32      // Reusable buffer for random variable selection (avoids allocation)
	tmpLearnedLits       []cnf.Literal // Reusable buffer for learned clause literals
	tmpSortedLits        []cnf.Literal // Temporary buffer for canonical clause sorting
	tmpMinimizedLits     []cnf.Literal // Reusable buffer for clause minimization (avoids allocation)
	tmpMinSeenVars       []uint32      // Vars marked in tmpSeenVar during minimization (for fast cleanup)
	conflictClauseBuf   cnf.Clause    // Pre-allocated conflict clause (avoids per-conflict heap alloc)
	conflictLitsBuf     []cnf.Literal // Pre-allocated buffer for conflict clause literal copies
	tmpIsGlue            []bool        // Bitmap for glue clause selection during restart (avoids allocation)

	// Reusable buffers for clause deletion (avoid per-deletion allocation)
	tmpDeleted            []bool               // Bitmap for deleted clauses
	tmpClauseUsedAsReason []bool               // Track clauses used as implications
	tmpClauseIndexMap     []int                // Pre-allocated buffer for old->new clause index mapping

	// Watched literals infrastructure
	watchLists          [][]cnf.Watch // watchLists[lit] = clauses watching lit
	watchInitialized    bool          // True if watches have been initialized
	learnedClauseBase   int           // Base ID for learned clause watches (fixed at initialization)
	originalUnitClauses []int         // Precomputed indices of original unit clauses (for restart re-propagation)

	// LBD-based learned clause ordering for propagation prioritization

	// Watched literals infrastructure
	emptyClauseFound    bool                     // Set when empty learned clause derived (UNSAT)
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
	reasonSize int // Size of reason clause (for optional sorting heuristics)
}

// precomputeOriginalUnitClauses returns clause indices of original unit clauses (1 literal).
// Used by restart() to re-propagate units without scanning all clauses.
func precomputeOriginalUnitClauses(formula *cnf.CNF) []int {
	var units []int
	for i := range formula.Clauses {
		if len(formula.Clauses[i].Literals) == 1 {
			units = append(units, i)
		}
	}
	return units
}

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
		preprocessTrail:      make([]int, 0, formula.NumVars),
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
		tmpMinSeenVars:       make([]uint32, 0, 256),
		conflictLitsBuf:     make([]cnf.Literal, 0, 256),
		tmpIsGlue:            make([]bool, maxLearned),
		// Clause deletion buffers - pre-allocate to maxLearned to avoid reallocation
		tmpDeleted:            make([]bool, maxLearned),
		tmpClauseUsedAsReason: make([]bool, maxLearned),
		tmpClauseIndexMap:     make([]int, maxLearned),
		learnedClauseBase:     int(formula.NumClauses),
		originalUnitClauses:   precomputeOriginalUnitClauses(formula),
		// Recursive minimization: max depth of reason-chain exploration (safety cap)
		minimizeMaxDepth: 100,
		// Vivification: run every 50 restarts (configurable via CLI)
		vivifyPeriod:     50,
		vivifyEnabled:    true,
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
	// Also initialize implication to -1 (no reason) — make([]int, N) zero-fills to 0,
	// which looks like "original clause 0" and causes assignLiteralByClause to skip assignment.
	for i := range solver.assignments {
		solver.assignments[i] = Assignment{Level: -1}
		solver.varLevel[i] = -1
		solver.implication[i] = -1
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

// SetMinimizeMaxDepth sets the maximum recursion depth for recursive clause
// minimization (safety/cost cap). The natural DAG bound of the implication
// graph already terminates recursion; this is a defensive limit.
func (s *CDCLSolver) SetMinimizeMaxDepth(d int) {
	if d < 0 {
		d = 0
	}
	s.minimizeMaxDepth = d
}

// SetVivifyPeriod sets how often vivification runs (every Nth restart).
// 0 disables vivification entirely.
func (s *CDCLSolver) SetVivifyPeriod(p int) {
	s.vivifyPeriod = p
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

// getReasonLitsForVar returns the reason clause literals for a variable whose
// truth was derived by propagation. Returns nil if the variable is a decision,
// has a preprocessing sentinel reason, or the reason clause is gone (tombstone
// or out-of-bounds). Decodes the implication array encoding:
//   >=0  → original clause at that index
//   <=-5 → learned clause (index = -impl-5)
//   -1   → decision (no reason)
//   -2/-3/-4 → preprocessing sentinels (no resolvable reason clause)
func (s *CDCLSolver) getReasonLitsForVar(v uint32) []cnf.Literal {
	reasonClauseIdx := s.implication[v]
	if reasonClauseIdx >= 0 {
		if int(reasonClauseIdx) < len(s.cnf.Clauses) {
			return s.cnf.Clauses[reasonClauseIdx].Literals
		}
		return nil
	}
	if reasonClauseIdx <= -5 {
		learnedIdx := -reasonClauseIdx - 5
		if int(learnedIdx) < s.learnedCapacity && s.learnedSizes[learnedIdx] > 0 {
			return s.getLearnedClauseLiterals(int(learnedIdx))
		}
	}
	return nil
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
type PreprocessingConfig struct {
	EnableUnitProp bool
	MaxPasses      int
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
			EnableUnitProp: false,
			MaxPasses:      0,
		}
	}

	// Highly structured (score >= 0.7): unit propagation only
		s.Log("c [preprocessing] Highly structured instance (score=%.2f) - enabling unit propagation only\n", structure.StructuredScore)
	return PreprocessingConfig{
		EnableUnitProp: true,
		MaxPasses:      1,
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
		// Still need to propagate original unit clauses + activate watches
		if s.propagateOriginalUnitsAndActivateWatches() {
			s.printStats()
			return UNSAT
		}
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

	// CRITICAL FIX: Rebuild preprocessTrail from all preprocessing assignments.
	// Preprocessing vars live on preprocessTrail (separate from search trail)
	// so they are never cleared by backtrack/restart.
	s.preprocessTrail = s.preprocessTrail[:0]
	for i := range s.assignments {
		if s.assignments[i].Level == 0 {
			s.preprocessTrail = append(s.preprocessTrail, int(i))
		}
	}
	s.trail = s.trail[:0]
	s.trailHead = []int{0}
	s.qhead = 0

	// Rebuild literal pool after preprocessing (even if no clauses removed)
	s.cnf.RebuildLiteralPool()

	// Initialize watches after unit propagation
	// CRITICAL: Reset watchInitialized flag so watches are re-initialized
	s.watchInitialized = false
	s.initWatches()

	// CRITICAL: Propagate original unit clauses + activate watches for preprocessing vars.
	if unsat := s.propagateOriginalUnitsAndActivateWatches(); unsat {
		return UNSAT
	}

	return UNKNOWN
}

// propagateOriginalUnitsAndActivateWatches propagates original unit clauses (length 1,
// not watched), stores them on preprocessTrail, then runs one propagation pass to
// activate watches on preprocessing variables. Must be called after initWatches.
// Returns true if UNSAT is detected (conflicting unit clauses).
func (s *CDCLSolver) propagateOriginalUnitsAndActivateWatches() bool {
	// Propagate original unit clauses (not watched by watched literals)
	for _, clauseIdx := range s.originalUnitClauses {
		clause := &s.cnf.Clauses[clauseIdx]
		lit := clause.Literals[0]
		varIdx := lit.Var()
		if s.assignments[varIdx].Level >= 0 {
			litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
			if !litTrue {
				return true // Conflict with unit clause - UNSAT
			}
			continue
		}
		value := !lit.IsNegated()
		s.assignments[varIdx] = Assignment{Value: value, Level: 0}
		s.varLevel[varIdx] = 0
		s.preprocessTrail = append(s.preprocessTrail, int(varIdx))
		s.implication[varIdx] = -2
	}

	// Activate watches for preprocessing variables.
	// Preprocessing vars live on preprocessTrail (not s.trail), so propagateWatched
	// never processes them during search. Run one explicit activation pass: temporarily
	// put preprocessTrail on s.trail, call propagateWatched to process watches, then
	// move everything (including new propagations) back to preprocessTrail.
	s.trail = append(s.trail[:0], s.preprocessTrail...)
	s.qhead = 0
	s.level = 0
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	conflict, _ := s.propagateWatched()
	if conflict {
		return true
	}
	// Move all trail elements (original + any propagations) to preprocessTrail.
	// Reset propagated vars from Level 1 (propLevel hack) to Level 0 (preprocessing).
	for _, v := range s.trail {
		if s.assignments[v].Level != 0 {
			s.assignments[v] = Assignment{Value: s.assignments[v].Value, Level: 0}
			s.varLevel[v] = 0
		}
	}
	s.preprocessTrail = append(s.preprocessTrail[:0], s.trail...)
	s.trail = s.trail[:0]
	s.trailHead = []int{0}
	s.qhead = 0
	return false
}

// initWatches initializes watched literals for all clauses
// Called after preprocessing completes (preprocessing modifies clauses)
func (s *CDCLSolver) initWatches() {
	if s.watchInitialized {
		return
	}

	numLits := int(s.cnf.NumVars) * 2
	if numLits == 0 {
		s.watchInitialized = true
		return
	}
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
		return
	}

	// Swap watched literals into positions 0 and 1 (MiniSat-style).
	// This lets us find the blocking literal at position 1-WatchPos without storing Blit.
	lit0 := literals[watch0]
	lit1 := literals[watch1]
	literals[0], literals[watch0] = lit0, literals[0]
	if watch1 == 0 {
		literals[1], literals[watch0] = lit1, literals[1]
	} else {
		literals[1], literals[watch1] = lit1, literals[1]
	}

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: int32(clauseIdx),
		Blit:      uint32(lit1),
		WatchPos:  0,
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: int32(clauseIdx),
		Blit:      uint32(lit0),
		WatchPos:  1,
	})
}

// addLearnedClauseToWatches adds a learned clause to the watch lists.
// Watches the first two literals that are not both false (shared helper).
// Returns the literal indices of the two watched literals.
func (s *CDCLSolver) addLearnedClauseToWatches(learnedIdx int, clause *cnf.Clause, literals []cnf.Literal) (int, int) {
	if len(literals) < 2 {
		return -1, -1
	}

	watch0, watch1 := s.chooseWatchPositions(literals)
	if watch0 < 0 || watch1 < 0 {
		return -1, -1
	}

	// Swap watched literals into positions 0 and 1 (MiniSat-style).
	lit0 := literals[watch0]
	lit1 := literals[watch1]
	literals[0], literals[watch0] = lit0, literals[0]
	if watch1 == 0 {
		literals[1], literals[watch0] = lit1, literals[1]
	} else {
		literals[1], literals[watch1] = lit1, literals[1]
	}

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	clauseIdx := int32(-learnedIdx - 1)

	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: clauseIdx,
		Blit:      uint32(lit1),
		WatchPos:  0,
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: clauseIdx,
		Blit:      uint32(lit0),
		WatchPos:  1,
	})

	return idx0, idx1
}

// vivifyClause attempts to shorten a learned clause by detecting redundant
// literals via trial propagation. For each literal li in the clause, we assume
// ¬li and propagate. If a prior literal is forced true by the assumptions → it's
// redundant, remove it. If ¬li causes conflict → the prefix is implied, shrink
// to it.
//
// Returns true if the clause was modified (caller must update watches and
// possibly compact). Returns false if no change. The solver state (trail,
// assignments, level) is always restored to level 0 on return.
//
// Soundness: a literal li is removed only if (¬l1 ∧ ... ∧ ¬l(k-1)) → li,
// meaning (l1 ∨ ... ∨ l(k-1) ∨ li) ≡ (l1 ∨ ... ∨ l(k-1)). If the negation
// of a prefix causes conflict, that prefix is implied by the empty set, so the
// full clause is a tautology — shrinking to the prefix is sound.
func (s *CDCLSolver) vivifyClause(learnedIdx int) bool {
	offset := s.learnedOffsets[learnedIdx]
	size := s.learnedSizes[learnedIdx]
	literals := s.learnedLiterals[offset : offset+size]

	if size <= 2 {
		return false
	}

	// Copy literals to a local buffer — propagation may swap literals in the
	// clause's storage (watch replacement), which would corrupt our iteration.
	// We reuse tmpLearnedLits (it's not in use during restart).
	newLits := s.tmpLearnedLits[:0]
	s.level = 0
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	s.qhead = 0

	modified := false

	for i := 0; i < len(literals); i++ {
		lit := literals[i]
		varIdx := lit.Var()

		// If already assigned (permanent or trial), check if clause is satisfied.
		if s.assignments[varIdx].Level >= 0 {
			litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
			if litTrue {
				// Clause is satisfied — no vivification possible.
				s.cancelUntil(0)
				return false
			}
			// Literal is false under existing assignments — keep it (can't assume ¬lit
			// since it's already true). These include permanent assignments (unit
			// clauses, preprocessing) and trial propagations from prior assumptions.
			newLits = append(newLits, lit)
			continue
		}

		// Unassigned: keep in new clause, assume ¬lit.
		newLits = append(newLits, lit)

		// Assume ¬lit: assign it false at a new level and propagate.
		s.level++
		s.trailHead = append(s.trailHead, len(s.trail))
		// Assign ¬lit (the negation of the literal in the clause)
		negLit := cnf.NewLiteral(varIdx, !lit.IsNegated())
		s.assignLiteralByClause(negLit, s.level, -1)

		conflict, _ := s.propagateWatched()
		if conflict {
			// ¬l1 ∧ ... ∧ ¬li causes conflict → (l1 ∨ ... ∨ li) is implied.
			// Shrink to the prefix (newLits so far, including li).
			s.cancelUntil(0)
			if len(newLits) < size {
				modified = true
			} else {
				modified = false
			}
			goto done
		}
	}

	s.cancelUntil(0)

	done:
	if !modified || len(newLits) == size || len(newLits) == 0 {
		return false
	}

	// Don't shrink to 1 literal during vivification — unit clauses propagate
	// immediately and can cause false conflicts with other clauses being
	// vivified in the same round.
	if len(newLits) == 1 {
		return false
	}
	// Update s.tmpLearnedLits so the caller can read the correct length and data.
	// Without this, len(s.tmpLearnedLits) would be stale (from the last
	// learnClause call), causing runVivification to copy wrong number of literals.
	s.tmpLearnedLits = newLits
	s.Log("c [vivify] clause %d: %d→%d literals (deferred)\n", -learnedIdx-1, size, len(newLits))
	return true
}

// runVivification runs one round of clause vivification on the learned clause
// database. Processes clauses with LBD > 2 and size > 2 that are not currently
// used as reasons. Time-bounded to avoid excessive overhead.
//
// Returns true if UNSAT was detected (empty clause derived).
func (s *CDCLSolver) runVivification() bool {
	if !s.vivifyEnabled || s.vivifyPeriod <= 0 {
		return false
	}
	if s.learnedActiveCount == 0 {
		return false
	}

	s.Log("c [vivify] Starting vivification round: %d active clauses\n", s.learnedActiveCount)

	s.inVivification = true
	defer func() { s.inVivification = false }()

	const timeBudget = 500 * time.Millisecond
	startTime := time.Now()

	// Mark clauses used as reasons (cannot vivify these — they're in use).
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
			if int(learnedIdx) < s.learnedCapacity {
				protected[learnedIdx] = true
			}
		}
	}

	modifiedCount := 0
	checkedCount := 0
	type vivifyResult struct {
		idx   int
		newLits []cnf.Literal
	}
	results := make([]vivifyResult, 0, 64)

	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedSizes[i] <= 2 {
			continue
		}
		if protected[i] {
			continue
		}
		if s.learnedMetadata[i].LBD <= 2 {
			continue
		}

		// Time budget check
		if time.Since(startTime) > timeBudget {
			break
		}

		checkedCount++
		oldSize := s.learnedSizes[i]
		if s.vivifyClause(i) {
			// vivifyClause left the new literals in tmpLearnedLits
			newSize := len(s.tmpLearnedLits)
			if newSize > 0 && newSize < oldSize {
				newLits := make([]cnf.Literal, newSize)
				copy(newLits, s.tmpLearnedLits)
				results = append(results, vivifyResult{idx: i, newLits: newLits})
				modifiedCount++
			}
		}

		// Check for UNSAT
		if s.emptyClauseFound {
			s.Log("c [vivify] UNSAT detected (conflicting unit clauses)\n")
			return true
		}
	}

	// Restore solver state (vivification may have left trail in a weird state).
	s.cancelUntil(0)
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	s.qhead = 0
	s.level = 0

	// Apply all modifications NOW (after the round — trial propagation is done).
	for _, r := range results {
		offset := s.learnedOffsets[r.idx]
		oldSize := s.learnedSizes[r.idx]
		// Remove old watches (while size is still oldSize >= 2)
		s.removeLearnedClauseWatches(r.idx)
		// Rewrite literals
		copy(s.learnedLiterals[offset:offset+oldSize], r.newLits)
		s.learnedSizes[r.idx] = len(r.newLits)
		// Rebuild watches
		if len(r.newLits) >= 2 {
			lits := s.learnedLiterals[offset : offset+len(r.newLits)]
			tmpClause := &cnf.Clause{Literals: lits, Learned: true}
			idx0, idx1 := s.addLearnedClauseToWatches(r.idx, tmpClause, lits)
			if r.idx < len(s.learnedWatchIdx0) {
				s.learnedWatchIdx0[r.idx] = idx0
				s.learnedWatchIdx1[r.idx] = idx1
			}
		} else if len(r.newLits) == 1 {
			if r.idx < len(s.learnedWatchIdx0) {
				s.learnedWatchIdx0[r.idx] = -1
				s.learnedWatchIdx1[r.idx] = -1
			}
			s.unitLearnedList = append(s.unitLearnedList, r.idx)
		}
	}

	if modifiedCount > 0 {
		s.compactPending = true
		s.Log("c [vivify] Vivified %d/%d clauses\n", modifiedCount, checkedCount)
	}

	// CRITICAL: Clear all Level > 0 assignments and re-propagate from scratch.
	// Vivification trial propagation may have moved watches on clauses. After
	// cancelUntil(0), the trial assignments are cleared, but the watch moves
	// are NOT undone. The base assignments (unit clause propagations at Level 1)
	// are still in s.assignments but NOT in the trail. Without re-propagation,
	// the watch system may miss propagations for these base-assigned variables
	// (because their watches were moved during trial), leading to incomplete
	// propagation and potential false UNSAT.
	s.inVivification = false
	for i := range s.assignments {
		if s.assignments[i].Level > 0 {
			s.assignments[i] = Assignment{Level: -1}
			s.varLevel[i] = -1
			s.implication[i] = -1
		}
	}
	s.level = 0
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	s.qhead = 0
	s.propagateWatched()

	return false
}

// removeLearnedClauseWatches removes all watches for a deleted learned clause.
// With MiniSat-style watched literals (positions 0/1), no Blit updates needed.
func (s *CDCLSolver) removeLearnedClauseWatches(learnedIdx int) {
	if learnedIdx < 0 || learnedIdx >= s.learnedCapacity {
		return
	}

	if s.learnedSizes[learnedIdx] < 2 {
		return
	}

	clauseIdx := int32(-learnedIdx - 1)

	idx0 := s.learnedWatchIdx0[learnedIdx]
	idx1 := s.learnedWatchIdx1[learnedIdx]

	if idx0 < 0 || idx1 < 0 || idx0 >= len(s.watchLists) || idx1 >= len(s.watchLists) {
		return
	}

	// Remove watch from lit0's watch list (swap-remove, no Blit update needed)
	wl0 := s.watchLists[idx0]
	for i := range wl0 {
		if wl0[i].ClauseIdx == clauseIdx {
			lastIdx := len(wl0) - 1
			if i != lastIdx {
				wl0[i] = wl0[lastIdx]
			}
			wl0 = wl0[:lastIdx]
			break
		}
	}
	s.watchLists[idx0] = wl0

	// Remove watch from lit1's watch list
	wl1 := s.watchLists[idx1]
	for i := range wl1 {
		if wl1[i].ClauseIdx == clauseIdx {
			lastIdx := len(wl1) - 1
			if i != lastIdx {
				wl1[i] = wl1[lastIdx]
			}
			wl1 = wl1[:lastIdx]
			break
		}
	}
	s.watchLists[idx1] = wl1
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

// cancelUntil backtracks to the given decision level, unassigning all variables
// above it. Used by vivification to roll back trial assignments. Unlike
// backtrack(), this does not flip decisions or perform conflict analysis — it
// is a pure state rollback.
func (s *CDCLSolver) cancelUntil(level int) {
	if level >= s.level {
		return
	}
	var decisionPoint int
	if level+1 < len(s.trailHead) {
		decisionPoint = s.trailHead[level+1]
	} else {
		decisionPoint = len(s.trail)
	}
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{Level: -1}
		s.varLevel[varIdx] = -1
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:level+1]
	s.level = level
	s.qhead = len(s.trail)
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

	// Clear search trail and assignments.
	// Preprocessing vars live on preprocessTrail (separate, permanent) — no preservation check needed.
	// Search assignments are always at Level >= 1 (propLevel hack ensures root-level props get Level 1).
	s.trail = s.trail[:0]
	s.trailHead = append(s.trailHead[:0], 0)
	s.qhead = 0
	s.level = 0
	for i := range s.assignments {
		if s.assignments[i].Level > 0 {
			s.assignments[i] = Assignment{Level: -1}
			s.varLevel[i] = -1
			s.implication[i] = -1
		}
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

	// Run vivification every Nth restart (after compaction, at level 0
	// with no learned clause in use as a reason — same safety conditions
	// as compaction).
	if s.vivifyEnabled && s.vivifyPeriod > 0 && s.lubyIndex > 0 && s.lubyIndex%s.vivifyPeriod == 0 {
		if s.runVivification() {
			return true // UNSAT detected
		}
	}

	return false // No UNSAT detected
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
	// Propagate original unit clauses + activate watches even without preprocessing
	if s.propagateOriginalUnitsAndActivateWatches() {
		s.printStats()
		return UNSAT
	}
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
		// Skip unit scan during vivification trial propagation (level > 0).
		// The unit scan re-propagates permanent unit clauses during trial,
		// which can conflict with trial assumptions and falsely declare UNSAT.
		// During initial propagation (level == 0), the unit scan runs normally.
		if s.inVivification && s.level > 0 {
			break
		}
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

			// FAST PATH: Check the cached blocking literal (Blit) before
			// accessing clause data. If Blit is true, the clause is satisfied
			// and we can skip it entirely — no need to dereference clauseLits
			// (which may cause a cache miss to random memory). Blit may be
			// stale (if the other watch was moved), but a stale-true Blit is
			// sound: the literal it references is still assigned and true, so
			// the clause is still satisfied.
			blitLit := cnf.Literal(watch.Blit)
			blitVarIdx := blitLit.Var()
			blitNegated := blitLit.IsNegated()
			blitLevel := s.varLevel[blitVarIdx]
			if blitLevel >= 0 {
				blitValue := s.assignments[blitVarIdx].Value
				blitLitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)
				if blitLitTrue {
					continue
				}
			}

			// SLOW PATH: Blocking literal is not true (or unassigned).
			// Access clause data for replacement search / conflict detection.
			var clauseLits []cnf.Literal
			if watch.ClauseIdx >= 0 {
				if int(watch.ClauseIdx) >= len(s.cnf.Clauses) {
					continue
				}
				clauseLits = s.cnf.Clauses[watch.ClauseIdx].Literals
			} else {
				learnedIdx := -watch.ClauseIdx - 1
				if int(learnedIdx) >= len(s.learnedSizes) || s.learnedSizes[learnedIdx] == 0 {
					continue
				}
				offset := s.learnedOffsets[learnedIdx]
				size := s.learnedSizes[learnedIdx]
				clauseLits = s.learnedLiterals[offset : offset+size]
			}

			// MiniSat-style: watched literals are at positions 0 and 1.
			myPos := int(watch.WatchPos)
			blitPos := 1 - myPos

			// Re-read the actual blocking literal from clause data (Blit may be stale).
			// The fast-path Blit check already filtered out the true case; here we
			// need the actual literal for the replacement guard, propagation, and
			// conflict detection.
			blitLit = clauseLits[blitPos]
			blitVarIdx = blitLit.Var()
			blitNegated = blitLit.IsNegated()

			// Look for replacement watch
			foundReplacement := false
			watchLitVar := uint32(watchIdx >> 1)
			for j := 2; j < len(clauseLits); j++ {
				clauseLit := clauseLits[j]
				clauseLitVar := clauseLit.Var()

				if clauseLitVar == watchLitVar || clauseLitVar == blitVarIdx {
					continue
				}

				litLevel := s.varLevel[clauseLitVar]
				litNegated := clauseLit.IsNegated()
				if litLevel >= 0 {
					litValue := s.assignments[clauseLitVar].Value
					litTrue := (!litNegated && litValue) || (litNegated && !litValue)
					if !litTrue {
						continue
					}
				}
				// Found replacement - swap into myPos and add new watch
				newWatchIdx := int(clauseLitVar) << 1
				if litNegated {
					newWatchIdx |= 1
				}

			// Swap replacement literal into position myPos
			clauseLits[myPos], clauseLits[j] = clauseLit, clauseLits[myPos]

			// The blocking literal (at 1-myPos) hasn't changed — cache it in Blit
			// so future propagations can skip clause data access when it's true.
			newBlit := uint32(clauseLits[1-myPos])

			s.watchLists[newWatchIdx] = append(s.watchLists[newWatchIdx], cnf.Watch{
				ClauseIdx: watch.ClauseIdx,
				Blit:      newBlit,
				WatchPos:  watch.WatchPos,
			})
			// Update learnedWatchIdx to track the new watch list (fixes stale
			// index bug where removeLearnedClauseWatches would search the wrong
			// list after a watch move, leaving orphaned watches for tombstoned
			// clauses).
			if watch.ClauseIdx < 0 {
				li := int(-watch.ClauseIdx - 1)
				if li < len(s.learnedWatchIdx0) {
					if watch.WatchPos == 0 {
						s.learnedWatchIdx0[li] = newWatchIdx
					} else {
						s.learnedWatchIdx1[li] = newWatchIdx
					}
				}
			}

				foundReplacement = true
				break
			}

		if foundReplacement {
				// Watch moved - remove old watch using swap-with-last
				lastIdx := len(watchList) - 1
				if readIdx != lastIdx {
					watchList[readIdx] = watchList[lastIdx]
					readIdx--
				}
				watchList = watchList[:lastIdx]
				s.watchLists[watchIdx] = watchList
				continue
			}

			// No replacement found - check if we can propagate or have conflict
			blitLevel = s.varLevel[blitVarIdx]

			if blitLevel < 0 {
				// Unassigned blit - propagate it
				propLevel := s.level
				if propLevel == 0 {
					propLevel = 1
				}
				reasonIdx := int(watch.ClauseIdx)
				if reasonIdx < 0 {
					li := -reasonIdx - 1
					reasonIdx = -li - 5
				}
				s.assignLiteralByClause(blitLit, propLevel, reasonIdx)
				propagationCount++
				s.propagations++
				continue
			}

			// Re-check blit value
			blitValue := s.assignments[blitVarIdx].Value
			blitTrue := (!blitNegated && blitValue) || (blitNegated && !blitValue)

			if !blitTrue {
				// Build conflict clause
				var conflictClause *cnf.Clause
				if watch.ClauseIdx >= 0 {
					conflictClause = &s.cnf.Clauses[watch.ClauseIdx]
				} else {
					li := -watch.ClauseIdx - 1
					literals := s.getLearnedClauseLiterals(int(li))
					s.conflictLitsBuf = s.conflictLitsBuf[:0]
					s.conflictLitsBuf = append(s.conflictLitsBuf, literals...)
					s.conflictClauseBuf.Literals = s.conflictLitsBuf
					s.conflictClauseBuf.Learned = true
					conflictClause = &s.conflictClauseBuf
				}

			if s.level == 0 {
				s.emptyClauseFound = true
					s.watchLists[watchIdx] = watchList
					return true, conflictClause
				}
				if s.verbose {
					s.Log("c [PROP CONFLICT] Watch idx=%d, clauseIdx=%d, level=%d\n",
						watchIdx, watch.ClauseIdx, s.level)
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
			s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx: varIdx})
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

		// Skip resolved variables (prevents re-resolution cycles when the
		// trail-order invariant is violated by inconsistent reason clauses).
		if s.tmpResolved[varIdx] {
			continue
		}
		// Only resolve variables currently in the clause (a var may have been
		// cancelled by a prior resolution and is no longer present — resolving it
		// would produce an unsound super-clause and corrupt currentCount).
		if !s.tmpLiteralInClause[varIdx] {
			continue
		}

		reasonClauseIdx := s.implication[varIdx]
		if reasonClauseIdx == -1 {
			continue
		}
		if s.assignments[varIdx].Level == 0 {
			continue
		}

		// Get reason clause literals via shared helper
		reasonLits := s.getReasonLitsForVar(varIdx)
		if reasonLits == nil {
			continue
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

		// Skip reason clauses with unassigned literals (invalid conflict clauses)
		hasUnassigned := false
		for _, lit := range reasonLits {
			if s.assignments[lit.Var()].Level < 0 {
				hasUnassigned = true
				break
			}
		}
		if hasUnassigned {
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
			// Only add assigned literals (level >= 0)
			assignLevel := s.assignments[v].Level
			if assignLevel < 0 {
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
						s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx: v})
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

	// NOTE: currentCount == 0 means resolution canceled all literals at the
	// current decision level. The learned clause would be non-asserting (no
	// literal at the current level to propagate after backjump). Skip learning
	// it — the clause is sound but not useful, and backjump to s.level-1.
	if currentCount == 0 {
		// Compute maxLevel from remaining literals for a better backjump target
		bjLevel := 0
		for _, varIdx := range s.tmpTouchedVars {
			if s.tmpLiteralInClause[varIdx] {
				lvl := s.assignments[varIdx].Level
				if lvl > 0 && lvl < s.level && lvl > bjLevel {
					bjLevel = lvl
				}
			}
		}
		if bjLevel == 0 {
			bjLevel = s.level - 1
			if bjLevel < 0 {
				bjLevel = 0
			}
		}
		return bjLevel
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
		s.learnedLiterals = append(s.learnedLiterals, s.tmpLearnedLits...)

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
		}
		s.currentAge++
		s.learnedActiveCount++
		s.learnedCapacity++

		// Add to watches
		// CRITICAL: Use learnedCapacity - 1 (the index of the just-appended data),
		// NOT learnedActiveCount - 1. After deleteLearnedClauses decrements
		// learnedActiveCount, using learnedActiveCount - 1 would point to a
		// tombstoned slot instead of the newly appended clause data, causing
		// duplicate watches and stale watch corruption.
		learnedIdx := s.learnedCapacity - 1
		literals := s.getLearnedClauseLiterals(learnedIdx)
		
		// Store watch indices for all clauses to maintain array consistency
		if len(literals) >= 2 {
			var idx0, idx1 int
			if s.watchInitialized {
				tmpClause := &cnf.Clause{Literals: literals, Learned: true}
				idx0, idx1 = s.addLearnedClauseToWatches(learnedIdx, tmpClause, literals)
			} else {
				// Watches not yet initialized (preprocessing); store positions 0,1
				// as placeholder — initWatches will choose correct positions later
				idx0 = cnf.LitToIndex(literals[0])
				idx1 = cnf.LitToIndex(literals[1])
			}
			s.learnedWatchIdx0 = append(s.learnedWatchIdx0, idx0)
			s.learnedWatchIdx1 = append(s.learnedWatchIdx1, idx1)
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

// minimizeLearnedClause reduces the size of a learned clause via recursive
// self-subsumption (MiniSat/Bier-style). A literal is removable if every other
// literal in its reason clause is itself either already in the learned clause
// or recursively removable. The UIP (single current-level literal) and level-0
// literals are never removed — the clause must stay asserting and level-0
// literals are global.
//
// Uses two mark arrays:
//   - tmpLiteralInClause[v]: literal is currently in the clause (drives rebuild)
//   - tmpSeenVar[v]: literal is "covered" (in clause OR proven-removable OR
//     being-explored). Set during exploration; cleared on failed branches via
//     snapshot rollback; cleared in full at the end via tmpMinSeenVars.
//
// Soundness rests on the CDCL resolution invariant: a literal removable via its
// reason clause yields a valid resolution consequence, so dropping it produces a
// strictly stronger (or equal) clause. Termination is guaranteed by the DAG
// property of the implication graph within a decision level (reason clauses only
// reference earlier trail literals). minimizeMaxDepth is a defensive cap.
func (s *CDCLSolver) minimizeLearnedClause(learnedLits []cnf.Literal) []cnf.Literal {
	if len(learnedLits) <= 2 {
		return learnedLits
	}

	// Phase A: mark clause literals in both arrays and record for cleanup.
	s.tmpMinSeenVars = s.tmpMinSeenVars[:0]
	for _, lit := range learnedLits {
		v := lit.Var()
		s.tmpLiteralInClause[v] = true
		if !s.tmpSeenVar[v] {
			s.tmpSeenVar[v] = true
			s.tmpMinSeenVars = append(s.tmpMinSeenVars, v)
		}
	}

	// Phase B: try to remove each non-protected literal.
	reductionAchieved := 0
	for _, lit := range learnedLits {
		v := lit.Var()
		if !s.tmpLiteralInClause[v] {
			continue // already removed in this pass
		}
		// Protect the UIP / any current-level literal: removing it would make
		// the clause non-asserting.
		if s.assignments[v].Level == s.level {
			continue
		}
		// Protect level-0 literals: they are global and never removable.
		if s.assignments[v].Level == 0 {
			continue
		}
		// Skip decisions (no reason clause to resolve against).
		if s.implication[v] == -1 {
			continue
		}
		if s.recursiveTryRemove(v) {
			s.tmpLiteralInClause[v] = false
			reductionAchieved++
			// tmpSeenVar[v] stays true — v is now "covered" for downstream
			// checks in the same pass (it's removable given prior removals).
		}
	}

	// Phase C: compact in-place, keeping literals still marked.
	writeIdx := 0
	for _, lit := range learnedLits {
		if s.tmpLiteralInClause[lit.Var()] {
			learnedLits[writeIdx] = lit
			writeIdx++
		}
	}

	// Phase D: clear tmpSeenVar marks recorded in tmpMinSeenVars. Also clear
	// tmpLiteralInClause (defensive — the next-conflict cleanup at learnClause
	// would do it, but keeping the buffer clean here is safer and matches the
	// pre-minimization invariant).
	for _, v := range s.tmpMinSeenVars {
		s.tmpSeenVar[v] = false
	}
	for _, lit := range learnedLits {
		s.tmpLiteralInClause[lit.Var()] = false
	}
	s.tmpMinSeenVars = s.tmpMinSeenVars[:0]

	if s.verbose && reductionAchieved > 0 {
		s.Log("c [minimize] Reduced: %d→%d literals (removed %d)\n",
			len(learnedLits), writeIdx, reductionAchieved)
	}

	return learnedLits[:writeIdx]
}

// recursiveTryRemove attempts to prove that literal v (currently in the learned
// clause) is redundant: every other literal in v's reason clause is either
// already covered (in the clause or proven removable) or recursively removable.
// On success returns true (v may be dropped). On failure returns false and
// rolls back ALL marks pushed during this call via a snapshot of tmpMinSeenVars,
// so failed explorations leave no stale "covered" marks (this correctly handles
// the diamond case where a prior successful sub-exploration is invalidated by a
// later sibling failure).
func (s *CDCLSolver) recursiveTryRemove(v uint32) bool {
	snapshot := len(s.tmpMinSeenVars)
	result := s.exploreRemovable(v, 0)
	if !result {
		// Roll back: unmark everything pushed during this call.
		for i := snapshot; i < len(s.tmpMinSeenVars); i++ {
			s.tmpSeenVar[s.tmpMinSeenVars[i]] = false
		}
		s.tmpMinSeenVars = s.tmpMinSeenVars[:snapshot]
	}
	return result
}

// exploreRemovable is the recursive core. It does NOT roll back on failure —
// that is the caller's job (recursiveTryRemove) via the snapshot. This avoids
// double-unmarking in the diamond case: a nested success pushes marks that
// remain valid only if the whole top-level call succeeds; if a sibling later
// fails, the top-level rollback clears them all in one pass.
//
// Returns true iff every non-resolved literal in v's reason clause is covered
// (tmpSeenVar) or recursively removable within minimizeMaxDepth.
func (s *CDCLSolver) exploreRemovable(v uint32, depth int) bool {
	if depth > s.minimizeMaxDepth {
		return false
	}
	reasonLits := s.getReasonLitsForVar(v)
	if reasonLits == nil {
		return false // decision, preprocessing sentinel, or stale clause
	}
	if len(reasonLits) <= 1 {
		// Unit reason: the asserting literal alone — nothing to resolve against,
		// so v is not removable via this reason.
		return false
	}
	for _, rl := range reasonLits {
		rv := rl.Var()
		if rv == v {
			continue // the asserting (resolved) literal of the reason
		}
		if s.tmpSeenVar[rv] {
			continue // already covered
		}
		// Level-0 literals are global: a reason depending on one cannot be
		// resolved away without losing global information.
		if s.assignments[rv].Level == 0 {
			return false
		}
		// Decisions cannot be resolved away.
		if s.implication[rv] == -1 {
			return false
		}
		// Optimistically mark rv as "being explored" to prevent cycles within
		// this call, then recurse.
		s.tmpSeenVar[rv] = true
		s.tmpMinSeenVars = append(s.tmpMinSeenVars, rv)
		if !s.exploreRemovable(rv, depth+1) {
			return false
		}
	}
	return true
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

// deleteLearnedClauses removes low-quality learned clauses to control memory usage
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
		// Swap watched literals into positions 0 and 1 (MiniSat-style)
		lit0 := literals[watch0]
		lit1 := literals[watch1]
		literals[0], literals[watch0] = lit0, literals[0]
		if watch1 == 0 {
			literals[1], literals[watch0] = lit1, literals[1]
		} else {
			literals[1], literals[watch1] = lit1, literals[1]
		}
		clauseIdx := int32(-i - 1)

		s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
			ClauseIdx: clauseIdx,
			Blit:      uint32(lit1),
			WatchPos:  0,
		})
		s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
			ClauseIdx: clauseIdx,
			Blit:      uint32(lit0),
			WatchPos:  1,
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
				if int(learnedIdx) < s.learnedCapacity && s.learnedSizes[learnedIdx] == 0 {
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

	// Clear all assignments from decisionPoint onwards.
	// No preprocessing check needed — preprocessing vars are on preprocessTrail (not s.trail).
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
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


