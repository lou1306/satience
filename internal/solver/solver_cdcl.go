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
	"fmt"
	"os"
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
	DefaultRestartBase       = 200   // Base for Luby restart sequence (MiniSat-style)
	IterationReportInterval = 10000 // Report progress every N iterations

	// Debugging thresholds.
	DebugConflictLimit = 100 // Verbose debug output for first N conflicts

	// Watch.ClauseIdx bit encoding:
	//   Bit 31: 0 = original clause, 1 = learned clause (sign bit, so ClauseIdx < 0 = learned)
	//   Bit 30: myPos — which watch position (0 or 1) this watch occupies in the clause
	//   Bits 0-29: clause index (supports up to 1B clauses)
	// Packing myPos into ClauseIdx eliminates the clauseLits[0] != falseLit cache miss
	// that was the #1 hotspot in propagation (13-14% of CPU).
	watchLearnedBit  uint32 = 0x80000000
	watchMyPosBit    uint32 = 0x40000000
	watchIdxMask     uint32 = 0x3FFFFFFF
	watchMyPosMask   uint32 = 0xBFFFFFFF // bits 0-29 + bit 31 (clears myPos for identity comparison)
)

// litToBlit converts a Literal to a litTrue index for storage in Watch.Blit.
// litTrue index = varIdx*2 + negated, matching the litValue array layout.
// Storing the index directly eliminates bit manipulation in the fast path.
func litToBlit(lit cnf.Literal) uint32 {
	l := uint32(lit)
	return (l&0x7FFFFFFF)<<1 | (l >> 31)
}

// calculateMaxLearned scales the clause database limit with instance size.
// MiniSat-style: base limit proportional to variables, grows with conflicts
func calculateMaxLearned(numVars uint32, numClauses int) int {
	// Learned clause limit: max(numClauses/3, min(numVars*10, 5000)).
	// - numClauses/3: MiniSat-style base, keeps database proportional to instance.
	// - min(numVars*10, 5000): Floor for small instances (need room to learn),
	//   capped at 5000 to prevent excessive database size on large instances
	//   (e.g. 7807v → 5000 instead of 78070, which caused 100K+ clause databases
	//   and 6x slower propagation).
	baseLimit := numClauses / 3

	varFloor := int(numVars) * 10
	if varFloor > 5000 {
		varFloor = 5000
	}
	if varFloor < 300 {
		varFloor = 300
	}
	if baseLimit < varFloor {
		baseLimit = varFloor
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
	trail           []uint32
	preprocessTrail []uint32 // Permanent preprocessing assignments (Level 0, never cleared/backtracked)
	trailHead       []int
	level        int
	vsids        *VSIDS
	conflicts    int
	implication  []int32 // Reason clause: >=0 original; <=-5 learned (-learnedIdx-5); -1 decision; -2 unit-prop preprocess; -3 pure-literal preprocess; -4 reserved
	iterations   int
	propagations int // Total propagations (assignments by unit propagation)
	maxIter      int
	// Memory pool for learned clauses - contiguous literal storage to eliminate per-clause allocations
	learnedLiterals      []cnf.Literal         // All learned clause literals in one contiguous slice
	learnedLoc           []LearnedClauseLoc    // Packed (Offset, Size) per learned clause; Size=0 means tombstone
	learnedAlive         []byte                 // 1 = clause alive, 0 = tombstone (cache-friendly bitmap for tombstone check)
	learnedMetadata      []cnf.ClauseMetadata  // Per-clause metadata (LBD, SearchHint)
	learnedWatchIdx0     []int                 // First watched literal index (for fast watch removal)
	learnedWatchIdx1     []int                 // Second watched literal index (for fast watch removal)
	learnedActiveCount   int                   // Number of active clauses (excludes tombstones)
	learnedCapacity      int                   // Total capacity including tombstones
	verbose              bool
	statsInterval        int // Print stats every N conflicts (0=disabled, bypasses verbose gate)
	solveStartNs         int64 // Wall-clock start (UnixNano) of the public Solve entry; for elapsed in stats
	decisions            int
	backjumpLevel        int
	maxLearned           int
	savedPhase           []bool
	restartBase          int
	restartCount         int
	lubyIndex            int
	lubyThresholdCap     int // Max Luby threshold before resetting lubyIndex to 0 (prevents Luby exhaustion)
	lbdSum               int
	lbdCount             int
	emaLBD               float64 // Exponential moving average of LBD (smooth restart signal)
	// B2: Cumulative LBD accumulator (NOT reset on restart, unlike lbdSum/lbdCount).
	// Used to detect consistently high-LBD instances and shrink the clause DB.
	totalLbdSum          uint64
	totalLbdCount        uint64
	maxLearnedShrunk     bool // True if maxLearned has been shrunk (one-way, no grow-back)
	restartPropsDecLimit  int     // Props/dec threshold for restart (0=disabled, default 100)
	adaptivePhaseFlipRate float64 // Phase flip rate when props/dec is high (0=disabled, default 0.1)
	propsDecRestartGap    int     // Min conflicts between props/dec-bounded restarts (default 100)
	// B1: Level-capped restart for long-clause instances. Long-clause instances
	// produce high-LBD clauses consistently, so the Glucose EMA criterion never
	// fires (EMA ≈ avg). Deep search produces high-LBD clauses with no propagation
	// guidance, creating a deep-search → high-LBD → no-guidance → deep-search cycle.
	// This breaks the cycle by restarting when conflict level exceeds the cap.
	// Gated on LongClauseRatio > 0.8 (binary-heavy instances have the props/dec restart).
	lastConflictLevel    int     // Level at which the last conflict occurred (captured in handleConflict)
	longClauseRatio      float64 // Cached LongClauseRatio from classifier (for B1 gate)
	restartLevelCap      int     // Force restart when conflict level exceeds this on long-clause instances (0=disabled)
	levelRestartGap      int     // Min conflicts between level-capped restarts (anti-thrashing)
	randomSeed           uint64          // Seed for deterministic random selection
	rndInitNoise         float64 // Magnitude of random noise added to initial VSIDS activity (0=disabled)
	unitLearnedList      []int           // List of learned clause indices that are unit clauses (for O(1) propagation)
	unitsDirty           bool            // True when unit scan needs to run (new unit learned or backtrack occurred)
	numUnassigned            int      // Count of unassigned variables (O(1) allAssigned/hasUnassigned)
	minimizeMaxDepth   int      // Max recursion depth for recursive clause minimization (default 0=unlimited)
	unitPropBudget     int      // Max literal visits for unit propagation preprocess (0=unlimited)
	veBudget           int      // Max resolvents for variable elimination (0=unlimited)
	eliminatedVars     []eliminatedVar // Variables eliminated by BVE (for model reconstruction)
	vivifyPeriod       int      // Run vivification every Nth restart (0=disabled, default 50)
	vivifyMinConflictGap int    // Min conflicts between vivify rounds (default 5000)
	conflictsAtLastVivify int   // conflict count at last vivify round (for gap gate)
	randomPhaseRate       float64 // Probability of flipping the saved phase per decision (0=disabled)
	restartPhaseFlipRate  float64 // Probability of flipping each saved phase on restart (0=disabled)
	lbdScaleOverride      bool    // True if user explicitly set LBD scale via CLI (skip adaptive)
	vivifyEnabled      bool     // Whether vivification is enabled (adaptive: structured instances only)
	inVivification     bool     // True during vivification trial propagation (suppresses false UNSAT from unit scan)
	// Equivalence detection (SCC-based): stores mapping for model reconstruction.
	equivRep           []uint32 // Representative variable for each variable (identity if not merged)
	equivNeg           []bool   // Whether variable is equivalent to negation of its representative
	hasEquivalences    bool     // True if detectEquivalences found and merged any equivalences
	// Cached classifier output (set in getAdaptivePreprocessingConfig). Used by
	// initVSIDSOccurrenceBonus to gate the polarity-based initial phase: the
	// occurrence-based phase is trajectory-sensitive and helps some instances
	// while hurting others with near-identical structure, so the classifier
	// score alone cannot predict benefit. The PolarityImbalance metric (mean
	// per-variable |pos-neg|/(pos+neg)) is stored as a secondary signal.
	structureScore      float64 // Cached StructuredScore from analyzeInstanceStructure
	polarityImbalance   float64 // Mean per-variable polarity imbalance (0=balanced, 1=pure)
	// skipPolarityPhase is set by the classifier for dense binary instances
	// (binaryRatio > 0.9 AND density > 35) where the default phase propagates
	// to a solution quickly via the highly-connected BIG, and the occurrence-
	// based override fights the implication structure.
	skipPolarityPhase   bool
	// skipBVE is set by the classifier for dense binary instances
	// (binaryRatio > 0.95 AND density > 10) where BVE hits the resolvent budget
	// eliminating only 7-14% of variables while spending 4-15s on resolvent
	// generation + post-BVE rebuild. These instances' highly-connected BIGs are
	// navigated in <2s by watch-based propagation alone, so VE is pure overhead.
	skipBVE             bool
	// skipSubsumption is set by the classifier for very dense instances
	// (density > 60) where the O(clauses × occurrences × clause-length) subsumption
	// pass is pure overhead. On ramlb_6_6 (density 149, 44856 clauses) subsumption
	// strengthens 47636 clauses in ~5s while the instance solves in 0.03s without
	// it. On ramlb_5_5 (density 74, 11180 clauses) it strengthens 11244 clauses in
	// ~0.4s while the instance solves in 0.01s without it. The threshold of 60 is
	// above the highest-density MiniSat Fast Suite instance (32baec6a, density 49)
	// but catches the divergent cnfgen instances (ramlb density 74-279,
	// kcliquebin density 298).
	skipSubsumption     bool
	// Binary implication graph (BIG): bigAdj[litIdx] lists forward successors m
	// such that binary clause (¬litIdx ∨ m) exists (i.e., litIdx → m in the
	// implication graph). Built once from original binary clauses in buildBIG.
	// Used for transitive BIG-based clause minimization (see bigReachableInClause).
	bigAdj [][]int
	// BIG BFS state for transitive clause minimization. Per-literal epoch stamps
	// avoid re-zeroing the visited array on each minimization call (epoch just
	// increments). The queue is reused across calls (sliced to [:0]).
	bigBfsVisited []uint32
	bigBfsEpoch   uint32
	bigBfsQueue   []int
	// Diagnostic counters for learned-clause minimization (always-on; reported in
	// printStats and a periodic solve-loop log). Pure instrumentation — no behavior.
	minimizeCalls       uint64 // recursive self-subsumption invocations (clauses >2 lits)
	minimizeLiteralsIn  uint64 // literals seen by minimizer across all calls
	minimizeLiteralsOut uint64 // literals remaining after minimization across all calls
	bigMinimizeCalls    uint64 // BIG minimization attempts
	bigMinimizeHits     uint64 // BIG minimization successes
	vivifyRoundsRun     uint64 // vivification rounds actually executed
	vivifyClausesChecked uint64 // clauses passed to vivifyClause
	vivifyClausesModified uint64 // clauses shortened by vivify
	vivifyLiteralsRemoved  uint64 // literals removed by vivify
	// Learned-clause subsumption (forward subsumption + self-subsumption
	// strengthening). Runs at level-0 restart boundaries like vivification,
	// using binary learned clauses as the subsumers. Self-gating: the occ
	// build early-returns when no binary learned clauses exist (random
	// instances produce none), so it is effectively free there.
	subsumptionPeriod         int    // Run subsumption every Nth restart (0=disabled, default 50)
	subsumptionMinConflictGap int    // Min conflicts between subsumption rounds (0=restart-based only)
	conflictsAtLastSubsumption int   // conflict count at last subsumption round (for gap gate)
	subsumptionRoundsRun       uint64 // subsumption rounds actually executed
	subsumptionClausesChecked uint64 // non-binary learned clauses scanned
	subsumptionClausesSubsumed  uint64 // clauses deleted by forward subsumption
	subsumptionClausesStrengthened uint64 // literals removed by self-subsumption
	// Histogram of final learned-clause sizes (post-minimization, at learn time).
	// Buckets: [<=2, 3-5, 6-10, 11-20, 21-50, >50].
	learnedLenHist     [6]uint64
	maxLearnedClauseSize int
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
	tmpTouchedVars       []uint32      // Track which variables were modified (for fast reset)
	tmpLearnedLits       []cnf.Literal // Reusable buffer for learned clause literals
	tmpMinSeenVars       []uint32      // Vars marked in tmpSeenVar during minimization (for fast cleanup)
	conflictClauseBuf   cnf.Clause    // Pre-allocated conflict clause (avoids per-conflict heap alloc)
	conflictLitsBuf     []cnf.Literal // Pre-allocated buffer for conflict clause literal copies

	// Reusable buffer for vivification results (avoid per-round allocation)
	tmpVivifyResults []vivifyResult

	// Reusable buffers for clause deletion (avoid per-deletion allocation)
	tmpDeleted            []bool               // Bitmap for deleted clauses
	tmpClauseUsedAsReason []bool               // Track clauses used as implications
	tmpClauseIndexMap     []int                // Pre-allocated buffer for old->new clause index mapping
	tmpDeletionCandidates []int                 // Pre-allocated buffer for deletion candidate sorting

	// Watched literals infrastructure
	watchLists          [][]cnf.Watch // watchLists[lit] = clauses watching lit
	watchInitialized    bool          // True if watches have been initialized
	originalUnitClauses []int         // Precomputed indices of original unit clauses (for restart re-propagation)
	originalSearchHint  []int32        // Per-original-clause search hint for replacement scan (0=no hint)

	// LBD-based learned clause ordering for propagation prioritization

	// Watched literals infrastructure
	emptyClauseFound    bool                     // Set when empty learned clause derived (UNSAT)
	compactPending      bool                     // Set when learned-clause tombstone ratio is high; compaction runs at the next restart (level 0)

	qhead                int // Watched literals: next trail index to process
	lastLearnedClauseIdx int // Index of most recently learned clause (-1 = none); for asserting literal propagation

	// Configurable parameters (exposed for tuning)
	preprocessingMaxVars     int     // Skip preprocessing if > N vars (default 50000)
	preprocessingMaxClauses  int     // Skip preprocessing if > N clauses (default 500000)
	// Restart policy parameters
	restartGlucoseRatio        float64 // Glucose-style restart when LBD > ratio × avg (default 1.5)
	restartGlucoseMinConflicts int     // Min conflicts before Glucose restarts kick in (default 50)
	glucoseGap                 int     // Min conflicts between Glucose restarts (anti-thrashing, default 100)

	// litTrue caches whether each literal is assigned-and-true, indexed by
	// LitToIndex(lit) = varIdx*2 + negated. Updated on assign/unassign. The blit
	// fast path reads this instead of decoding the literal + loading assignments[],
	// eliminating 5 ops per watch. On unassign, both polarities are set to false
	// (unassigned ≠ false-assigned). Trajectory-neutral: same truth values, just cached.
	litTrue []bool
}

// resolveCandidate is used in learnClause for tracking resolution candidates
type resolveCandidate struct {
	varIdx     uint32
	reasonSize int // Size of reason clause (for optional sorting heuristics)
}

// vivifyResult holds a vivified clause's new literals pending application
type vivifyResult struct {
	idx     int
	newLits []cnf.Literal
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
	restartBase := DefaultRestartBase

	// Ensure literal pool is built for efficient propagation
	formula.RebuildLiteralPool()

	solver := &CDCLSolver{
		cnf:                  formula,
		assignments:          make([]Assignment, formula.NumVars),
		trail:                make([]uint32, 0, formula.NumVars),
		preprocessTrail:      make([]uint32, 0, formula.NumVars),
		trailHead:            make([]int, 1),
		qhead:                0,
		lastLearnedClauseIdx: -1,
		level:                0,
		vsids:                NewVSIDS(formula.NumVars),
		conflicts:            0,
		implication:          make([]int32, formula.NumVars), // -1 = decision (no clause)
		iterations:           0,
		maxIter:              0,
		// P0: Pre-allocate learned clause arrays with generous capacity to avoid growth
		// learnedLiterals: 8 literals per clause average (covers most learned clauses)
		learnedLiterals:      make([]cnf.Literal, 0, maxLearned*8),
		learnedLoc:           make([]LearnedClauseLoc, 0, maxLearned),
		learnedMetadata:      make([]cnf.ClauseMetadata, 0, maxLearned), // Packed metadata
		learnedWatchIdx0:     make([]int, 0, maxLearned), // Watched literal indices
		learnedWatchIdx1:     make([]int, 0, maxLearned),
		learnedAlive:         make([]byte, 0, maxLearned),
		learnedActiveCount:   0,
		learnedCapacity:      0,
		unitLearnedList:      make([]int, 0, 64), // Pre-allocate for unit clause tracking
		verbose:              false,
		decisions:            0,
		backjumpLevel:        0,
		maxLearned:           maxLearned,
		savedPhase:           make([]bool, formula.NumVars),
		restartBase:          restartBase,
		restartCount:         0,
		lubyIndex:            0,
		lubyThresholdCap:     0, // Disabled by default; enabled for structured instances in classifyInstance
		lbdSum:               0,
		lbdCount:             0,
		randomSeed:           0,
		// P1: Pre-allocate reusable buffers with generous capacity to avoid reallocation
		tmpLiteralInClause:   make([]bool, formula.NumVars),
		tmpSeenVar:           make([]bool, formula.NumVars),
		tmpLiteralIsNegated:  make([]bool, formula.NumVars),
		bigBfsVisited:        make([]uint32, int(formula.NumVars)*2),
		bigBfsQueue:          make([]int, 0, 256),
		tmpLevelCount:        make([]int, formula.NumVars+1),
		tmpLevelCountUsed:    make([]bool, formula.NumVars+1),
		tmpCandidates:        make([]resolveCandidate, 0, 200),  // Increased from 100
		tmpLevelSet:          make([]int, 0, formula.NumVars),
		tmpLevelSetUsed:      make([]bool, formula.NumVars+1),
		tmpResolved:          make([]bool, formula.NumVars),
		tmpResolvedVars:      make([]uint32, 0, formula.NumVars),
		tmpTouchedVars:       make([]uint32, 0, formula.NumVars),
		// P1: Increased buffer capacity from 64 to 256 to handle larger learned clauses
		tmpLearnedLits:       make([]cnf.Literal, 0, 256),
		tmpMinSeenVars:       make([]uint32, 0, 256),
		conflictLitsBuf:     make([]cnf.Literal, 0, 256),
		tmpVivifyResults:    make([]vivifyResult, 0, 64),
		// Clause deletion buffers - pre-allocate to maxLearned to avoid reallocation
		tmpDeleted:            make([]bool, maxLearned),
		tmpClauseUsedAsReason: make([]bool, maxLearned),
		tmpClauseIndexMap:     make([]int, maxLearned),
		tmpDeletionCandidates: make([]int, 0, maxLearned),
		originalUnitClauses:   precomputeOriginalUnitClauses(formula),
		// Recursive minimization: max depth of reason-chain exploration (safety cap)
		// 0 = unlimited (rely on DAG property for termination). MiniSat uses no cap.
		minimizeMaxDepth: 0,
		// Unit propagation budget: 0 = unlimited (small instances use fixpoint cap).
		// Large instances set this to bound preprocessing time.
		unitPropBudget: 0,
		// Variable elimination budget: 0 = unlimited (small instances).
		// Large instances set this to bound resolvent generation.
		veBudget: 5000000, // 5M resolvents default; large instances override to 2M
		// Vivification: run every 50 restarts (configurable via CLI)
		vivifyPeriod:     50,
		vivifyEnabled:    true,
		// Min conflicts between vivify rounds. Without this gate, small/fast-restart
		// instances fire vivify every ~50 restarts = every few hundred conflicts,
		// thrashing the solver with low-yield rounds. 20000 ensures vivify only
		// fires on genuinely hard instances (those exceeding ~20K conflicts); easy
		// instances solve before vivify ever triggers.
		vivifyMinConflictGap: 20000,
		// Learned-clause subsumption: same gating cadence as vivification.
		// Self-gating via early-return on no binary learned clauses means it
		// is effectively free on random instances, so it is always enabled.
		subsumptionPeriod:         50,
		subsumptionMinConflictGap: 20000,
		randomPhaseRate:       0,
		restartPhaseFlipRate:  0,
		// Configurable parameters with defaults
		preprocessingMaxVars:     50000,
		preprocessingMaxClauses:  500000,
		// Restart policy defaults (aggressive Glucose-style for better performance)
		restartGlucoseRatio:        1.5, // Standard Glucose value (aggressive restarts)
		restartGlucoseMinConflicts: 50,  // Start Glucose restarts early
		glucoseGap:                 100, // Min 100 conflicts between Glucose restarts
		restartPropsDecLimit:       100, // Restart when props/dec > 100 (deep search pathology)
		adaptivePhaseFlipRate:      0.1, // Flip 10% of phases when props/dec is high
		propsDecRestartGap:         100, // Min 100 conflicts between props/dec restarts
		restartLevelCap:            40,  // Force restart when conflict level > 40 on long-clause instances
		levelRestartGap:            100, // Min 100 conflicts between level-capped restarts
		litTrue:                     make([]bool, int(formula.NumVars)*2),
	}

	// FIX: Initialize all assignments as unassigned (Level=-1)
	// Also initialize implication to -1 (no reason) — make([]int, N) zero-fills to 0,
	// which looks like "original clause 0" and causes assignLiteralByClause to skip assignment.
	for i := range solver.assignments {
		solver.assignments[i] = Assignment{Level: -1}
		solver.implication[i] = -1
	}

	// Initialize savedPhase to true (default positive phase)
	for i := range solver.savedPhase {
		solver.savedPhase[i] = true
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

// SetStatsInterval sets the conflict interval for periodic stats printing.
// 0 disables. When >0, stats are printed every N conflicts directly to stderr,
// bypassing the verbose gate (no arg-boxing overhead on hot paths).
func (s *CDCLSolver) SetStatsInterval(n int) {
	s.statsInterval = n
}

// SetRandomSeed sets the seed for deterministic random selection
// Default seed is 0
func (s *CDCLSolver) SetRandomSeed(seed uint64) {
	s.randomSeed = seed
	s.vsids.SetRandomSeed(seed)
}

// SetRndInitNoise sets the magnitude of random noise added to initial VSIDS
// activity. 0 = disabled (deterministic init). When >0, each variable's
// initial activity is perturbed by (rand[0,1) - 0.5) * noise, breaking exact
// ties at decision 1 without per-conflict overhead. Deterministic given -seed.
func (s *CDCLSolver) SetRndInitNoise(noise float64) {
	s.rndInitNoise = noise
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

// SetLBDBonusScale sets the LBD bonus scale for VSIDS.
// A non-zero value marks the scale as user-overridden (skips adaptive scaling).
func (s *CDCLSolver) SetLBDBonusScale(scale float64) {
	s.vsids.SetLBDBonusScale(scale)
	if scale > 0 {
		s.lbdScaleOverride = true
	}
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



// SetRestartParameters configures restart policy parameters
// base: Luby sequence base multiplier (default 20, range 1-1000)
// glucoseRatio: Glucose restart when LBD > ratio × avg (default 1.5, range 1.0-5.0)
// minConflicts: min conflicts before Glucose restarts activate (default 50)
func (s *CDCLSolver) SetRestartParameters(base int, glucoseRatio float64, minConflicts int) {
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
	if glucoseRatio > 100.0 {
		glucoseRatio = 100.0
	}
	s.restartGlucoseRatio = glucoseRatio

	if minConflicts < 0 {
		minConflicts = 0
	}
	s.restartGlucoseMinConflicts = minConflicts
}

func (s *CDCLSolver) SetRestartPropsDecLimit(limit int) {
	s.restartPropsDecLimit = limit
}

func (s *CDCLSolver) SetAdaptivePhaseFlipRate(rate float64) {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	s.adaptivePhaseFlipRate = rate
}

// SetRestartLevelCap sets the conflict level threshold for level-capped restarts
// on long-clause instances (0=disabled).
func (s *CDCLSolver) SetRestartLevelCap(n int) {
	s.restartLevelCap = n
}

func (s *CDCLSolver) SetLevelRestartGap(n int) {
	s.levelRestartGap = n
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

// SetVivifyMinConflictGap sets the minimum number of conflicts that must occur
// between two vivification rounds. 0 disables the gap gate (restart-based only).
func (s *CDCLSolver) SetVivifyMinConflictGap(g int) {
	s.vivifyMinConflictGap = g
}

// SetSubsumptionPeriod sets how often learned-clause subsumption runs (every
// Nth restart). 0 disables subsumption entirely.
func (s *CDCLSolver) SetSubsumptionPeriod(p int) {
	s.subsumptionPeriod = p
}

// SetSubsumptionMinConflictGap sets the minimum number of conflicts that must
// occur between two subsumption rounds. 0 disables the gap gate (restart-based
// only).
func (s *CDCLSolver) SetSubsumptionMinConflictGap(g int) {
	s.subsumptionMinConflictGap = g
}

// SetRandomPhaseRate sets the probability of flipping the saved phase per decision.
// 0 disables phase jitter. Default 0.01 (1%).
func (s *CDCLSolver) SetRandomPhaseRate(r float64) {
	s.randomPhaseRate = r
}

// SetRestartPhaseFlipRate sets the probability of flipping each saved phase on
// restart. 0 disables (standard phase saving). Used to break fixed points on
// binary-heavy instances where restart + phase saving re-enters the same cascade.
func (s *CDCLSolver) SetRestartPhaseFlipRate(r float64) {
	s.restartPhaseFlipRate = r
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
	// Approximate memory: literals (4 bytes each) + per-clause metadata arrays
	// (learnedLoc 8B + learnedMetadata 64B + learnedWatchIdx0/1 8B each = 88B) × capacity
	cap := cap(s.learnedLoc)
	poolMemoryKB = (len(s.learnedLiterals)*4 + cap*88) / 1024
	return activeClauses, poolLiterals, poolMemoryKB
}

// getLearnedClauseLiterals returns the literals for a learned clause (view into contiguous pool)
func (s *CDCLSolver) getLearnedClauseLiterals(clauseIdx int) []cnf.Literal {
	loc := s.learnedLoc[clauseIdx]
	return s.learnedLiterals[loc.Offset : int(loc.Offset)+int(loc.Size)]
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
		clauses := s.cnf.Clauses
		if int(reasonClauseIdx) < len(clauses) {
			return clauses[reasonClauseIdx].Literals
		}
		return nil
	}
	if reasonClauseIdx <= -5 {
		learnedIdx := -reasonClauseIdx - 5
		if int(learnedIdx) < s.learnedCapacity && s.learnedLoc[learnedIdx].Size > 0 {
			return s.getLearnedClauseLiterals(int(learnedIdx))
		}
	}
	return nil
}

// recordLearnedClauseSize updates the learned-clause length histogram and the
// max-observed-size tracker. Called once per stored learned clause at learn
// time (after minimization, so this reflects final sizes). Buckets:
// [<=2, 3-5, 6-10, 11-20, 21-50, >50]. Pure instrumentation.
func (s *CDCLSolver) recordLearnedClauseSize(size int) {
	if size > s.maxLearnedClauseSize {
		s.maxLearnedClauseSize = size
	}
	bucket := 0
	switch {
	case size <= 2:
		bucket = 0
	case size <= 5:
		bucket = 1
	case size <= 10:
		bucket = 2
	case size <= 20:
		bucket = 3
	case size <= 50:
		bucket = 4
	default:
		bucket = 5
	}
	s.learnedLenHist[bucket]++
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
		minSize := int(s.learnedLoc[0].Size)
		maxSize := minSize
		totalSize := 0

		for i := range s.learnedLoc {
			size := int(s.learnedLoc[i].Size)
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
	s.logDiagnostics()
	s.Log("c \n")

	// When -stats is enabled without -verbose, emit a compact final summary to
	// stderr. This gives a terminal stats line at SAT/UNSAT/UNKNOWN exits without
	// the 20-30% overhead of -verbose (every s.Log call site boxes args even when
	// the gate is false). Mirrors printPeriodicStats so a run with -stats N shows
	// both periodic progress lines and a matching final line.
	if s.statsInterval > 0 && !s.verbose {
		s.printFinalStats()
	}
}

// printFinalStats writes a compact one-line final summary to stderr, tagged
// [final] to distinguish from the periodic [stats] lines. Bypasses the
// verbose gate so -stats produces a closing summary even without -verbose.
func (s *CDCLSolver) printFinalStats() {
	minRemoved := s.minimizeLiteralsIn - s.minimizeLiteralsOut
	minRate := 0.0
	if s.minimizeLiteralsIn > 0 {
		minRate = float64(minRemoved) * 100.0 / float64(s.minimizeLiteralsIn)
	}
	avgLBD := 0.0
	if s.lbdCount > 0 {
		avgLBD = float64(s.lbdSum) / float64(s.lbdCount)
	}
	totalAvgLBD := 0.0
	if s.totalLbdCount > 0 {
		totalAvgLBD = float64(s.totalLbdSum) / float64(s.totalLbdCount)
	}
	propsPerDec := 0.0
	if s.decisions > 0 {
		propsPerDec = float64(s.propagations) / float64(s.decisions)
	}
	shrunk := ""
	if s.maxLearnedShrunk {
		shrunk = " [DB shrunk]"
	}
	fmt.Fprintf(os.Stderr, "c [final] t=%.2fs conflicts=%d decisions=%d props=%d props/dec=%.1f learned=%d/%d emaLBD=%.1f avgLBD=%.1f totAvgLBD=%.1f%s | min: rate=%.1f%% | vivify: rounds=%d | subsump: rounds=%d sub=%d str=%d | hist=[%d %d %d %d %d %d]\n",
		s.elapsedSec(), s.conflicts, s.decisions, s.propagations, propsPerDec,
		s.learnedActiveCount, s.maxLearned, s.emaLBD, avgLBD, totalAvgLBD, shrunk,
		minRate,
		s.vivifyRoundsRun,
		s.subsumptionRoundsRun, s.subsumptionClausesSubsumed, s.subsumptionClausesStrengthened,
		s.learnedLenHist[0], s.learnedLenHist[1], s.learnedLenHist[2],
		s.learnedLenHist[3], s.learnedLenHist[4], s.learnedLenHist[5])
}

// logDiagnostics prints a compact one-line summary of learned-clause
// minimization activity: recursive self-subsumption counters, vivification
// counters, and the learned-clause length histogram. Pure instrumentation.
// Buckets: [<=2, 3-5, 6-10, 11-20, 21-50, >50].
func (s *CDCLSolver) logDiagnostics() {
	minRemoved := s.minimizeLiteralsIn - s.minimizeLiteralsOut
	minRate := 0.0
	if s.minimizeLiteralsIn > 0 {
		minRate = float64(minRemoved) * 100.0 / float64(s.minimizeLiteralsIn)
	}
	s.Log("c [diag] minimize: calls=%d in=%d out=%d removed=%d (%.1f%%) | BIG: calls=%d hits=%d | vivify: rounds=%d checked=%d modified=%d removed=%d | subsump: rounds=%d checked=%d sub=%d str=%d | hist=[%d %d %d %d %d %d] max=%d\n",
		s.minimizeCalls, s.minimizeLiteralsIn, s.minimizeLiteralsOut, minRemoved, minRate,
		s.bigMinimizeCalls, s.bigMinimizeHits,
		s.vivifyRoundsRun, s.vivifyClausesChecked, s.vivifyClausesModified, s.vivifyLiteralsRemoved,
		s.subsumptionRoundsRun, s.subsumptionClausesChecked, s.subsumptionClausesSubsumed, s.subsumptionClausesStrengthened,
		s.learnedLenHist[0], s.learnedLenHist[1], s.learnedLenHist[2],
		s.learnedLenHist[3], s.learnedLenHist[4], s.learnedLenHist[5], s.maxLearnedClauseSize)
}

// printPeriodicStats writes a one-line stats summary directly to stderr,
// bypassing the verbose-gated s.Log. This enables diagnosing timeout instances
// without the 20-30% overhead of -verbose (arg boxing at every s.Log call site
// plus runtime.ReadMemStats STW calls). Does NOT call ReadMemStats.
func (s *CDCLSolver) printPeriodicStats() {
	minRemoved := s.minimizeLiteralsIn - s.minimizeLiteralsOut
	minRate := 0.0
	if s.minimizeLiteralsIn > 0 {
		minRate = float64(minRemoved) * 100.0 / float64(s.minimizeLiteralsIn)
	}
	avgLBD := 0.0
	if s.lbdCount > 0 {
		avgLBD = float64(s.lbdSum) / float64(s.lbdCount)
	}
	totalAvgLBD := 0.0
	if s.totalLbdCount > 0 {
		totalAvgLBD = float64(s.totalLbdSum) / float64(s.totalLbdCount)
	}
	propsPerDec := 0.0
	if s.decisions > 0 {
		propsPerDec = float64(s.propagations) / float64(s.decisions)
	}
	shrunk := ""
	if s.maxLearnedShrunk {
		shrunk = " [DB shrunk]"
	}
	fmt.Fprintf(os.Stderr, "c [stats] t=%.2fs conflicts=%d level=%d decisions=%d props=%d props/dec=%.1f learned=%d emaLBD=%.1f avgLBD=%.1f totAvgLBD=%.1f%s | min: rate=%.1f%% | BIG: hits=%d/%d\n",
		s.elapsedSec(), s.conflicts, s.level, s.decisions, s.propagations, propsPerDec,
		s.learnedActiveCount, s.emaLBD, avgLBD, totalAvgLBD, shrunk,
		minRate,
		s.bigMinimizeHits, s.bigMinimizeCalls)
}

// elapsedSec returns seconds since the public Solve entry set solveStartNs.
// Returns 0 if no solve has started (e.g. stats printed before solve).
func (s *CDCLSolver) elapsedSec() float64 {
	if s.solveStartNs == 0 {
		return 0.0
	}
	return float64(time.Now().UnixNano()-s.solveStartNs) / 1e9
}

// InstanceStructure captures metrics about CNF structure for adaptive preprocessing
type InstanceStructure struct {
	Density            float64 // clauses / vars
	BinaryRatio        float64 // binary clauses / total
	TernaryRatio       float64 // 3-literal clauses / total
	LongClauseRatio    float64 // clauses with >3 literals / total
	SmallClauseRatio   float64 // (binary + ternary) / total
	StructuredScore    float64 // 0.0 = random, 1.0 = highly structured
	PolarityImbalance  float64 // mean per-variable |pos-neg|/(pos+neg); 0=balanced, 1=pure
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

	// Clause size distribution and polarity counts (single pass over clauses).
	binaryCount := 0
	ternaryCount := 0
	longCount := 0
	smallCount := 0
	posCount := make([]int, s.cnf.NumVars)
	negCount := make([]int, s.cnf.NumVars)

	for i := 0; i < s.cnf.NumClauses; i++ {
		offset, size := s.cnf.GetOriginalClauseInfo(i)
		if size == 2 {
			binaryCount++
			smallCount++
		} else if size == 3 {
			ternaryCount++
			smallCount++
		} else if size > 3 {
			longCount++
		}
		lits := s.cnf.GetLiteralPool()[offset : offset+size]
		for _, lit := range lits {
			if lit.IsNegated() {
				negCount[lit.Var()]++
			} else {
				posCount[lit.Var()]++
			}
		}
	}

	if s.cnf.NumClauses > 0 {
		structure.BinaryRatio = float64(binaryCount) / float64(s.cnf.NumClauses)
		structure.TernaryRatio = float64(ternaryCount) / float64(s.cnf.NumClauses)
		structure.LongClauseRatio = float64(longCount) / float64(s.cnf.NumClauses)
		structure.SmallClauseRatio = float64(smallCount) / float64(s.cnf.NumClauses)
	}

	// Polarity imbalance: mean per-variable |pos-neg|/(pos+neg).
	// 0 = perfectly balanced (occurrence-based polarity is noise), 1 = pure
	// (one polarity absent). Used to gate the polarity-based initial phase.
	imbSum := 0.0
	imbCount := 0
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		t := posCount[i] + negCount[i]
		if t > 0 {
			diff := float64(posCount[i] - negCount[i])
			if diff < 0 {
				diff = -diff
			}
			imbSum += diff / float64(t)
			imbCount++
		}
	}
	if imbCount > 0 {
		structure.PolarityImbalance = imbSum / float64(imbCount)
	}

	// Structured score: weighted combination of metrics
	// Key insight: structured instances have EITHER many binary clauses OR many
	// long (>3 literal) clauses with varied sizes. Random k-SAT has uniform clause
	// sizes (all k-literal), zero binary, zero long clauses.
	// 
	// Components:
	// 1. Size score (weight 0.6): max(binary ratio, long-clause ratio). Structured
	//    instances score high on at least one; random k-SAT scores 0 on both.
	// 2. Density (weight 0.2): moderate contribution
	// 3. Mixed sizes (weight 0.2): structured instances have varied clause sizes
	
	binaryScore := structure.BinaryRatio
	longScore := structure.LongClauseRatio
	// Ternary-heavy structured instances (e.g., graph coloring) score low on
	// binary/long signals but are still structured — they need unit propagation
	// and Glucose restarts, not the aggressive random-config decay/restart.
	// Weight at 0.65: pure ternary (0.65) + density + mixed → ~0.75, above the
	// 0.70 threshold. 88% ternary + 11% binary (69d72f81) → 0.74, was 0.66.
	ternaryScore := structure.TernaryRatio * 0.65

	sizeScore := binaryScore
	if longScore > sizeScore {
		sizeScore = longScore
	}
	if ternaryScore > sizeScore {
		sizeScore = ternaryScore
	}
	
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
	
	structure.StructuredScore = sizeScore*0.6 + densityScore*0.2 + mixedSizeScore*0.2

	return structure
}

// classifyInstance analyzes the formula structure and caches classifier output
// (structureScore, polarityImbalance, longClauseRatio, skipPolarityPhase,
// skipBVE) plus sets search parameters (VSIDS decay, restartBase, Glucose
// restart gating) that depend only on instance structure — NOT on the
// preprocessing decision.
//
// This is split from getAdaptivePreprocessingConfig so that the search-parameter
// tuning is available even when preprocessing is skipped (SolveWithoutPreprocessing).
// Previously -no-preprocess skipped BOTH preprocessing AND adaptive tuning,
// making it a polluted diagnostic axis. classifyInstance is read-only on s.cnf.
//
// Must be called once, before search begins, in every solve entry point that
// runs the CDCL loop (SolveWithResult, SolveWithoutPreprocessing).
func (s *CDCLSolver) classifyInstance() {
	structure := s.analyzeInstanceStructure()

	// Cache classifier output for downstream consumers (initVSIDSOccurrenceBonus
	// gates the polarity-based initial phase on these metrics; the level-capped
	// restart gate reads longClauseRatio).
	s.structureScore = structure.StructuredScore
	s.polarityImbalance = structure.PolarityImbalance
	s.longClauseRatio = structure.LongClauseRatio

	// Dense binary instances: the BIG is highly connected, so the default phase
	// propagates to a solution quickly (0 conflicts observed on 32baec6a, a
	// 2500v/49-density/100%-binary instance). The occurrence-based polarity
	// override fights the implication structure and sends the search into
	// thousands of conflicts. Skip it. Sparse binary instances (e.g. 8202af80,
	// density 24.5) still benefit from the override, so the density threshold
	// (35) separates the two regimes.
	s.skipPolarityPhase = structure.BinaryRatio > 0.9 && structure.Density > 35.0

	// BVE is pure overhead on dense binary instances: it hits the resolvent
	// budget eliminating only 7-14% of variables while spending 4-15s on
	// resolvent generation + post-BVE rebuild. The highly-connected BIG is
	// navigated in <2s by watch-based propagation alone. 8202af80 (density
	// 24.5, 99.8% binary): 16.7s→1.4s. bb34f22f (density 3.12, 67% binary) is
	// NOT gated (density ≤ 10) — it's the instance VE was tuned for.
	s.skipBVE = structure.BinaryRatio > 0.95 && structure.Density > 10.0

	// Subsumption is O(clauses × occurrences × clause-length). On very dense
	// instances (density > 60) the occurrence lists are huge, making each pass
	// take seconds while the instance often solves in <0.1s without it.
	// ramlb_6_6 (density 149): subsumption 5s, search 0.03s. kcliquebin_6
	// (density 298): subsumption 2s, search 0.01s. ramlb_5_5 (density 74):
	// subsumption 0.4s, search 0.01s. The threshold of 60 is above the
	// highest-density suite instance (32baec6a, density 49).
	s.skipSubsumption = structure.Density > 60.0

	s.Log("c [structure] Density=%.2f, Binary=%.1f%%, Ternary=%.1f%%, Long=%.1f%%, Structured=%.2f, PolImb=%.3f\n",
		structure.Density,
		structure.BinaryRatio*100,
		structure.TernaryRatio*100,
		structure.LongClauseRatio*100,
		structure.StructuredScore,
		structure.PolarityImbalance)
	if s.skipPolarityPhase {
		s.Log("c [structure] Dense binary instance (density=%.1f, binary=%.1f%%) - skipping polarity phase\n",
			structure.Density, structure.BinaryRatio*100)
	}

	// Random-like instances (StructuredScore < 0.7). Split into two sub-branches
	// by binaryRatio because they need opposite decay schedules:
	//   - Pure k-SAT (binaryRatio == 0, density > 4.5): default decay 0.95,
	//     restartBase=5, Glucose active. Matches the minisat config that solves
	//     566f366c (300v random 3-SAT) in ~0.04s; the aggressive-decay branch
	//     took ~12s (346x). Aggressive decay (0.30->0.60) gives only ~3-5
	//     conflict memory vs minisat's ~50-100, causing 118x more conflicts on
	//     phase-transition random 3-SAT. The density > 4.5 gate excludes
	//     30eb4ef4 (density 4.20, phase-transition pure ternary) which regresses
	//     to TIMEOUT under default decay (see D1).
	//   - Mixed (binaryRatio > 0): aggressive decay (0.30->0.60) + restartBase=5
	//     + Glucose disabled. The aggressive decay was tuned for this cluster
	//     (score<0.7 with 23-34% binary); removing it regressed 15+ instances
	//     (+30% PAR-2, see D1).
	// Unit propagation on random/mixed instances causes 76x more conflicts, so
	// preprocessing is disabled in getAdaptivePreprocessingConfig for both.
	if structure.StructuredScore < 0.7 {
		if structure.BinaryRatio == 0 && structure.Density > 4.5 {
			s.Log("c [classification] Pure k-SAT instance (score=%.2f, density=%.2f) - default decay, restartBase=5\n",
				structure.StructuredScore, structure.Density)
			s.restartBase = 5
			// Glucose restart policy stays at CLI defaults (ratio=10.0, min=10):
			// active as a safety net. decay stays at 0.95 (default).
			return
		}
		s.Log("c [classification] Random-like mixed instance (score=%.2f) - aggressive decay, restartBase=5\n", structure.StructuredScore)
		s.vsids.SetAggressiveDecay()
		s.restartBase = 5
		s.restartGlucoseRatio = 100.0
		s.restartGlucoseMinConflicts = 1000000
		return
	}

	s.Log("c [classification] Structured instance (score=%.2f)\n", structure.StructuredScore)

	// Cap Luby threshold growth to prevent Luby exhaustion on very long-clause
	// instances. The Luby sequence grows unboundedly; without a cap, the
	// threshold eventually exceeds the conflict budget and Luby restarts stop.
	// Gated on longClauseRatio > 0.95: instances with >95% long clauses (e.g.
	// 274099073, 99.4% long) benefit from the cap's frequent small restarts
	// (+48% faster). Instances at 80-95% long (e.g. 822378be, 83.9%) are hurt
	// by the disruption — their level-capped restarts inflate lubyIndex, causing
	// the cap to fire and reset to 0, triggering spurious frequent Luby restarts
	// that disrupt the search. Random instances (handled above, early return)
	// are exempt — their Luby growth aids convergence.
	if s.longClauseRatio > 0.95 {
		s.lubyThresholdCap = 10000
	}

	// Adaptive restart base for binary-heavy instances. Binary cascades produce
	// low-LBD glue clauses that prevent the Glucose restart criterion from firing
	// (EMA never exceeds avg×1.5). More frequent Luby restarts help escape
	// these cascades. MiniSat restarts 5-10x more on binary-heavy instances.
	// Skipped for dense binary instances (skipBVE): without BVE reshaping the
	// clause DB, restartBase=20 sends the search into a bad trajectory (de2b584e
	// times out). The default 200 matches the -no-preprocess behavior that
	// solves these instances in <2s.
	if structure.BinaryRatio > 0.5 && !s.skipBVE {
		s.restartBase = 20
		s.Log("c [classification] Binary-heavy (%.0f%%) - restartBase=20\n", structure.BinaryRatio*100)
	}
}

// getAdaptivePreprocessingConfig returns preprocessing config based on the
// cached structureScore (set by classifyInstance, which must have run first).
func (s *CDCLSolver) getAdaptivePreprocessingConfig() PreprocessingConfig {
	if s.structureScore < 0.7 {
		s.Log("c [preprocessing] Random-like instance (score=%.2f) - disabling preprocessing\n", s.structureScore)
		return PreprocessingConfig{
			EnableUnitProp: false,
			MaxPasses:      0,
		}
	}
	s.Log("c [preprocessing] Structured instance (score=%.2f) - enabling unit propagation only\n", s.structureScore)
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

// compactClauses removes clauses marked in the removed slice from s.cnf.Clauses,
// updates NumClauses, and recomputes originalUnitClauses (indices shift after
// compaction). Caller is responsible for calling RebuildLiteralPool afterwards.
func (s *CDCLSolver) compactClauses(removed []bool) {
	writeIdx := 0
	for readIdx := 0; readIdx < len(s.cnf.Clauses); readIdx++ {
		if !removed[readIdx] {
			if writeIdx != readIdx {
				s.cnf.Clauses[writeIdx] = s.cnf.Clauses[readIdx]
			}
			writeIdx++
		}
	}
	s.cnf.Clauses = s.cnf.Clauses[:writeIdx]
	s.cnf.NumClauses = writeIdx
	// Recompute original unit clause indices (shifted by compaction)
	s.originalUnitClauses = precomputeOriginalUnitClauses(s.cnf)
}

// removeTautologiesAndDuplicates scans all original clauses and:
//   - Removes tautological clauses (containing both x and ¬x for some variable)
//   - Deduplicates repeated literals within each clause
//
// Returns the number of clauses removed. Both operations are trivially sound:
// a tautology is always satisfied and a duplicate literal is redundant.
// Runs in O(total literals) with a temporary seen-array indexed by literal.
func (s *CDCLSolver) removeTautologiesAndDuplicates() int {
	numLits := int(s.cnf.NumVars) * 2
	if numLits == 0 {
		return 0
	}
	seenLit := make([]bool, numLits)
	var touched []int

	removed := make([]bool, s.cnf.NumClauses)
	removedCount := 0

	for i := range s.cnf.Clauses {
		lits := s.cnf.Clauses[i].Literals
		if len(lits) <= 1 {
			continue // unit and empty clauses cannot be tautologies
		}

		touched = touched[:0]
		writeIdx := 0
		isTautology := false

		for _, lit := range lits {
			litIdx := cnf.LitToIndex(lit)
			if seenLit[litIdx] {
				continue // duplicate literal — skip
			}
			negIdx := litIdx ^ 1
			if seenLit[negIdx] {
				isTautology = true
				break
			}
			seenLit[litIdx] = true
			touched = append(touched, litIdx)
			lits[writeIdx] = lit
			writeIdx++
		}

		// Reset seenLit for touched entries
		for _, idx := range touched {
			seenLit[idx] = false
		}

		if isTautology {
			removed[i] = true
			removedCount++
			continue
		}

		if writeIdx < len(lits) {
			s.cnf.Clauses[i].Literals = lits[:writeIdx]
		}
	}

	if removedCount > 0 {
		s.compactClauses(removed)
	}
	return removedCount
}

// pureLiteralElimination assigns variables that appear with only one polarity
// throughout the formula and removes all clauses satisfied by them. Returns the
// number of variables assigned. Sound: a pure literal's assignment never
// conflicts with any clause (no clause contains the opposite polarity).
// Runs in O(total literals).
func (s *CDCLSolver) pureLiteralElimination() int {
	if s.cnf.NumVars == 0 {
		return 0
	}
	posCount := make([]int, s.cnf.NumVars)
	negCount := make([]int, s.cnf.NumVars)

	for i := range s.cnf.Clauses {
		for _, lit := range s.cnf.Clauses[i].Literals {
			if lit.IsNegated() {
				negCount[lit.Var()]++
			} else {
				posCount[lit.Var()]++
			}
		}
	}

	// Identify and assign pure literals
	isPure := make([]bool, s.cnf.NumVars)
	pureValue := make([]bool, s.cnf.NumVars)
	assignedCount := 0

	for v := uint32(0); v < s.cnf.NumVars; v++ {
		if s.assignments[v].Level >= 0 {
			continue // already assigned (e.g., by tautology removal creating a unit)
		}
		if posCount[v] > 0 && negCount[v] == 0 {
			isPure[v] = true
			pureValue[v] = true
		} else if negCount[v] > 0 && posCount[v] == 0 {
			isPure[v] = true
			pureValue[v] = false
		}

		if isPure[v] {
			s.assignments[v] = Assignment{Value: pureValue[v], Level: 0}
			s.preprocessTrail = append(s.preprocessTrail, uint32(v))
			s.implication[v] = -3 // pure literal preprocessing
			assignedCount++
		}
	}

	if assignedCount == 0 {
		return 0
	}

	// Remove all clauses containing any pure literal (all occurrences are the
	// satisfying polarity since the variable is pure)
	removed := make([]bool, s.cnf.NumClauses)
	removedCount := 0

	for i := range s.cnf.Clauses {
		for _, lit := range s.cnf.Clauses[i].Literals {
			if isPure[lit.Var()] {
				removed[i] = true
				removedCount++
				break
			}
		}
	}

	if removedCount > 0 {
		s.compactClauses(removed)
	}
	return assignedCount
}

func (s *CDCLSolver) preprocessAggressive() SolveResult {
	// Empty original clause = immediately UNSAT (watched literals skip clauses <2 lits,
	// so an empty clause would be invisible to propagation and yield UNKNOWN instead of UNSAT)
	if s.hasEmptyClause() {
		s.printStats()
		return UNSAT
	}

	s.Log("c [verbose] Aggressive preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)

	// Tautology and duplicate-literal removal (all instances, trivially sound).
	// Runs before the structure gate: these never change the search trajectory
	// the way forced unit propagation does, so they are safe for random-like
	// instances too.
	if tautRemoved := s.removeTautologiesAndDuplicates(); tautRemoved > 0 {
		s.Log("c [preprocessing] Removed %d tautological clauses\n", tautRemoved)
		s.cnf.RebuildLiteralPool()
	}

	// Pure literal elimination (all instances, trivially sound).
	if pleAssigned := s.pureLiteralElimination(); pleAssigned > 0 {
		s.Log("c [preprocessing] Pure literal elimination: assigned %d variables\n", pleAssigned)
		s.cnf.RebuildLiteralPool()
	}

	// Empty clause may have appeared from dedup (e.g. a clause of all-identical
	// literals becomes a unit, not empty — but check defensively).
	if s.hasEmptyClause() {
		s.printStats()
		return UNSAT
	}

	// Classify structure and set restart/decay config BEFORE equiv.
	// Equiv changes clause structure (merges variables, removes tautological clauses),
	// so we classify on the original instance to get the true structure score.
	// This ensures structured instances like bb34f22f (score 0.73 pre-equiv) get
	// the structured config instead of being misclassified after equiv changes ratios.
	config := s.getAdaptivePreprocessingConfig()

	// SCC-based equivalence detection (all instances, sound).
	// Runs before the size gate: O(V+E) and can significantly reduce instance
	// size by merging equivalent variables.
	if equivResult := s.detectEquivalences(); equivResult != UNKNOWN {
		s.printStats()
		return equivResult
	}

	// For large instances, bound preprocessing with literal-visit/resolvent budgets.
	// Must be set BEFORE BVE and unit propagation so the budgets actually apply.
	if int(s.cnf.NumVars) > s.preprocessingMaxVars || s.cnf.NumClauses > s.preprocessingMaxClauses {
		s.unitPropBudget = 5000000 // ~5M literal visits, bounded at ~50ms
		s.veBudget = 2000000       // ~2M resolvents, bounded at ~200ms
		s.Log("c [verbose] Large instance (%d vars, %d clauses) — unit prop budget=%d, ve budget=%d\n",
			s.cnf.NumVars, s.cnf.NumClauses, s.unitPropBudget, s.veBudget)
	}

	// Iterative simplification loop: subsumption → BVE → unit propagation.
	// Each pass creates new opportunities for the others (e.g., subsumption
	// shrinks clauses → BVE eliminates variables → new subsumptions appear).
	// MiniSat/CaDiCaL iterate until fixpoint; we bound at 3 passes for time.
	simplificationPasses := 3
	if s.cnf.NumClauses > 100000 {
		simplificationPasses = 1 // Large instances: one pass only (subsumption is O(n²))
	}
	for pass := 0; pass < simplificationPasses; pass++ {
		passChanged := false

		// Subsumption pass (forward subsumption + self-subsumption/strengthening).
		// Skipped on very dense instances (skipSubsumption) where O(n²) cost is
		// pure overhead — the instance solves faster without it.
		if config.EnableUnitProp && !s.skipSubsumption {
			subSubsumed, subStrengthened := s.subsumptionPass()
			if subSubsumed > 0 || subStrengthened > 0 {
				s.Log("c [preprocessing] Subsumption pass %d: %d clauses subsumed, %d strengthened\n", pass+1, subSubsumed, subStrengthened)
				passChanged = true
			}
		}

		// Bounded variable elimination (standard Davis-Putnam VE, NOT the banned
		// pos=1 definitional variant). Eliminates variables by resolving all
		// (x∨A)×(¬x∨B) pairs, discarding tautological resolvents. Only eliminates
		// when it reduces clause count. Model reconstruction by trying x=true/x=false.
		// Skipped on dense binary instances (skipBVE) where it is pure overhead.
		if config.EnableUnitProp && !s.skipBVE {
			veResult := s.boundedVarElimination()
			if veResult < 0 {
				s.printStats()
				return UNSAT
			}
			if veResult > 0 {
				passChanged = true
			}
		}

		// Unit propagation
		if config.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		if !passChanged {
			break // Fixpoint reached
		}
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
			s.preprocessTrail = append(s.preprocessTrail, uint32(i))
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
			litTrue := lit.IsNegated() != s.assignments[varIdx].Value
			if !litTrue {
				return true // Conflict with unit clause - UNSAT
			}
			continue
		}
		value := !lit.IsNegated()
		s.assignments[varIdx] = Assignment{Value: value, Level: 0}
		s.preprocessTrail = append(s.preprocessTrail, varIdx)
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

	for learnedIdx := 0; learnedIdx < s.learnedCapacity; learnedIdx++ {
		if s.learnedLoc[learnedIdx].Size == 0 {
			continue // Skip tombstones
		}
		literals := s.getLearnedClauseLiterals(learnedIdx)
		// Create a temporary Clause struct for addLearnedClauseToWatches
		tmpClause := &cnf.Clause{Literals: literals, Learned: true}
		s.addLearnedClauseToWatches(learnedIdx, tmpClause, literals)
	}

	// CRITICAL: Set watchInitialized AFTER all clauses are watched.
	// Guards initWatches() idempotency (re-init after preprocessing resets it to false).
	s.watchInitialized = true

	// Initialize per-original-clause search hints (0 = no hint, scan from pos 2).
	// Allocated after preprocessing is complete (clause count is final).
	s.originalSearchHint = make([]int32, s.cnf.NumClauses)

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
			litTrue := lit.IsNegated() != s.assignments[varIdx].Value
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

	// Pack myPos into ClauseIdx bit 30: watch on idx0 (lit0 at position 0) has myPos=0,
	// watch on idx1 (lit1 at position 1) has myPos=1.
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: int32(clauseIdx),
		Blit:      litToBlit(lit1),

	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: int32(clauseIdx) | int32(watchMyPosBit),
		Blit:      litToBlit(lit0),

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

	// Pack learned flag (bit 31) + myPos (bit 30) into ClauseIdx.
	clauseIdx0 := int32(watchLearnedBit | uint32(learnedIdx))           // myPos=0
	clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)                     // myPos=1

	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: clauseIdx0,
		Blit:      litToBlit(lit1),

	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: clauseIdx1,
		Blit:      litToBlit(lit0),

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
	loc := s.learnedLoc[learnedIdx]
	offset := int(loc.Offset)
	size := int(loc.Size)
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
			litTrue := lit.IsNegated() != s.assignments[varIdx].Value
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

	s.vivifyRoundsRun++
	s.conflictsAtLastVivify = s.conflicts
	s.Log("c [vivify] Starting vivification round: %d active clauses\n", s.learnedActiveCount)

	s.inVivification = true
	defer func() { s.inVivification = false }()

	// Deterministic clause-count budget (replaces wall-clock time budget).
	// The old 500ms time budget made vivify nondeterministic: wall-clock
	// truncation checked different clauses per run, perturbing the search
	// trajectory and causing flaky SAT/TIMEOUT results on small instances.
	// A clause-count cap is fully deterministic and bounded.
	const maxCheckPerRound = 2000

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
	results := s.tmpVivifyResults[:0]

	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size <= 2 {
			continue
		}
		if protected[i] {
			continue
		}
		if s.learnedMetadata[i].LBD <= 2 {
			continue
		}

		// Deterministic clause-count budget (replaces wall-clock time budget)
		if checkedCount >= maxCheckPerRound {
			break
		}

		checkedCount++
		s.vivifyClausesChecked++
		oldSize := int(s.learnedLoc[i].Size)
		if s.vivifyClause(i) {
			// vivifyClause left the new literals in tmpLearnedLits
			newSize := len(s.tmpLearnedLits)
			if newSize > 0 && newSize < oldSize {
				newLits := make([]cnf.Literal, newSize)
				copy(newLits, s.tmpLearnedLits)
				results = append(results, vivifyResult{idx: i, newLits: newLits})
				modifiedCount++
				s.vivifyClausesModified++
				s.vivifyLiteralsRemoved += uint64(oldSize - newSize)
			}
		}

		// Check for UNSAT
		if s.emptyClauseFound {
			s.Log("c [vivify] UNSAT detected (conflicting unit clauses)\n")
			return true
		}
	}

	// Save results backing array (may have grown if cap was exceeded)
	s.tmpVivifyResults = results

	// Restore solver state (vivification may have left trail in a weird state).
	s.cancelUntil(0)
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	s.qhead = 0
	s.level = 0

	// Apply all modifications NOW (after the round — trial propagation is done).
	for _, r := range results {
		offset := int(s.learnedLoc[r.idx].Offset)
		oldSize := int(s.learnedLoc[r.idx].Size)
		// Remove old watches (while size is still oldSize >= 2)
		s.removeLearnedClauseWatches(r.idx)
		// Rewrite literals
		copy(s.learnedLiterals[offset:offset+oldSize], r.newLits)
		s.learnedLoc[r.idx].Size = int32(len(r.newLits))
		// Update LBD: LBD ≤ clause size, so if the clause shrank, the LBD
		// may have decreased. Use min(oldLBD, newSize) as a conservative
		// overestimate — prevents valuable post-vivification glue clauses
		// from being deleted as high-LBD.
		newSize := len(r.newLits)
		if newSize < int(s.learnedMetadata[r.idx].LBD) {
			s.learnedMetadata[r.idx].LBD = int32(newSize)
		}
		// Reset search hint — vivification rewrites clause literals and
		// rebuilds watches, invalidating any position-based hint.
		s.learnedMetadata[r.idx].SearchHint = 0
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
			s.unitsDirty = true
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
			s.litTrue[i*2] = false
			s.litTrue[i*2+1] = false
			s.implication[i] = -1
			s.numUnassigned++
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

	if s.learnedLoc[learnedIdx].Size < 2 {
		return
	}

	// Clause identity for comparison: mask out myPos bit (bit 30) since the
	// stored watches may have either myPos value.
	clauseID := uint32(watchLearnedBit | uint32(learnedIdx))

	idx0 := s.learnedWatchIdx0[learnedIdx]
	idx1 := s.learnedWatchIdx1[learnedIdx]

	if idx0 < 0 || idx1 < 0 || idx0 >= len(s.watchLists) || idx1 >= len(s.watchLists) {
		return
	}

	// Remove watch from lit0's watch list (swap-remove, no Blit update needed)
	wl0 := s.watchLists[idx0]
	for i := range wl0 {
		if uint32(wl0[i].ClauseIdx)&watchMyPosMask == clauseID {
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
		if uint32(wl1[i].ClauseIdx)&watchMyPosMask == clauseID {
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
	for {
		k := 1
		for {
			ki := 1 << uint(k)
			if i == ki-1 {
				return 1 << uint(k - 1)
			}
			if ki-1 > i {
				i -= (1 << uint(k-1)) - 1
				break
			}
			k++
		}
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
// 1. Clear the trail (all search assignments; preprocessing vars preserved on preprocessTrail)
// 2. Keep ALL learned clauses (deletion is handled by deleteLearnedClauses based on
//    database size, NOT by restart)
// 3. Reset lbdSum/lbdCount for fresh per-restart average. emaLBD is NOT reset —
//    it tracks the LBD trend across restart boundaries (B5 fix). The glucoseGap
//    field (default 100 conflicts) prevents double-restarts from the persistent
//    EMA carrying high pre-restart values vs low post-restart avgLBD.
// 4. Continue search with same VSIDS scores (activity preserved across restarts)
// 5. Run compaction if tombstones accumulated, vivification every Nth restart

// Why Keep Glue Clauses?
// Glue clauses (LBD ≤ 2) are the "backbone" of the search:
// - They connect few decision levels (highly general)
// - They propagate often and prune large parts of search space
// - Deleting them would cause the solver to re-explore the same conflicts
func (s *CDCLSolver) shouldRestart() bool {
	// Check Glucose-style adaptive restart first (if past min conflicts).
	// The gap gate (conflicts - restartCount >= glucoseGap) prevents double-
	// restarts: with a persistent EMA (B5 fix), the EMA carries high pre-
	// restart LBD values while avgLBD is low right after a restart (fresh
	// good clauses), so the criterion could fire immediately. The gap lets
	// avgLBD stabilize first.
	if s.conflicts >= s.restartGlucoseMinConflicts && s.lbdCount > 0 &&
		s.conflicts-s.restartCount >= s.glucoseGap {
		avgLBD := float64(s.lbdSum) / float64(s.lbdCount)

		// Glucose criterion: restart when the EMA of recent LBDs exceeds
		// ratio × overall average. Using EMA (α=0.1, half-life ~7 conflicts)
		// instead of single lastConflictLBD avoids noise from individual
		// LBD spikes triggering spurious restarts.
		if s.emaLBD > avgLBD*s.restartGlucoseRatio {
			s.Log("c [restart] Glucose: EMA LBD %.1f > avg %.1f × %.2f\n",
				s.emaLBD, avgLBD, s.restartGlucoseRatio)
			return true
		}
	}

	// Fall back to Luby sequence (configurable base).
	// The Luby sequence grows unboundedly (1, 1, 2, 1, 1, 2, 4, ..., 2^k, ...).
	// Without a cap, the threshold (lubyValue × restartBase) eventually exceeds
	// the conflict budget, and Luby restarts effectively stop — leaving the
	// solver without periodic diversification for the rest of the solve.
	// Fix: when the threshold exceeds lubyThresholdCap, reset lubyIndex to 0 —
	// re-running the Luby sequence from the start. This keeps the restart
	// cadence in the productive range indefinitely. CaDiCaL and Kissat use
	// similar bounded restart sequences. Disabled for random instances
	// (lubyThresholdCap=0) where Luby growth aids convergence.
	lubyValue := luby(s.lubyIndex + 1)
	threshold := lubyValue * s.restartBase

	if s.conflicts-s.restartCount >= threshold {
		return true
	}

	// If the threshold exceeds the cap, reset the index so the sequence
	// restarts from the beginning on the next restart.
	if s.lubyThresholdCap > 0 && threshold > s.lubyThresholdCap {
		s.lubyIndex = 0
	}

	// Props/dec-bounded restart: if the solver is going too deep per decision
	// (unproductive binary cascade), restart to escape the trajectory. This
	// catches the "deep search" pathology where props/dec >> 100 (e.g.,
	// binary-heavy instances where each decision cascades through hundreds of
	// binary clauses for a single conflict). The Glucose EMA criterion doesn't
	// fire here because binary cascades produce low-LBD glue clauses, making
	// the search look productive by LBD metrics when it's actually going nowhere.
	// Only fires after the first few restarts (lubyIndex >= 3) and requires a
	// minimum conflict gap to prevent thrashing (the solver needs time to
	// explore between restarts, and the phase flip needs time to take effect).
	if s.restartPropsDecLimit > 0 && s.lubyIndex >= 3 && s.decisions > 10 &&
		s.conflicts-s.restartCount >= s.propsDecRestartGap {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		if propsPerDec > float64(s.restartPropsDecLimit) {
			s.Log("c [restart] Props/dec %.1f > %d\n", propsPerDec, s.restartPropsDecLimit)
			return true
		}
	}

	// Level-capped restart: on long-clause instances, force a restart when the
	// conflict level is excessively deep. Long-clause instances produce high-LBD
	// clauses consistently, so the Glucose EMA criterion (emaLBD > avg*ratio)
	// never fires (EMA ≈ avg). Deep search produces high-LBD clauses with no
	// propagation guidance, creating a deep-search → high-LBD → no-guidance →
	// deep-search cycle. This breaks the cycle by restarting when level > cap.
	// Gated on LongClauseRatio > 0.8 (binary-heavy instances have the props/dec
	// restart instead). Anti-thrashing: lubyIndex >= 3, decisions > 10, min-conflict gap.
	if s.restartLevelCap > 0 && s.longClauseRatio > 0.8 &&
		s.lubyIndex >= 3 && s.decisions > 10 &&
		s.conflicts-s.restartCount >= s.levelRestartGap &&
		s.lastConflictLevel > s.restartLevelCap {
		s.Log("c [restart] Level-capped: conflict level %d > %d (long-clause instance)\n",
			s.lastConflictLevel, s.restartLevelCap)
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
		varIdx := s.trail[i]
		s.assignments[varIdx] = Assignment{Level: -1}
		s.litTrue[int(varIdx)*2] = false
		s.litTrue[int(varIdx)*2+1] = false
		s.implication[varIdx] = -1
		s.numUnassigned++
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:level+1]
	s.level = level
	s.qhead = len(s.trail)
}

func (s *CDCLSolver) restart() bool {
	s.unitsDirty = true // Restart clears all assignments; units need re-propagation
	s.Log("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)

	// VSIDS activity is NOT reset on restart. Standard CDCL solvers (MiniSat,
	// Glucose) preserve activity across restarts — it is the solver's memory of
	// which variables matter. A full reset (0.3×) was destroying search memory
	// and causing 100× regressions on most instances. A mild reset (0.8×)
	// helped some instances but hurt others. No reset gives the best net result.

	// Use stored LBD values (calculated at learning time) instead of recalculating
	// Recalculating during restart gives wrong values since assignments change
	// Gated on verbose — the count is only consumed by the s.Log call below.
	if s.verbose {
		glueCount := 0
		for i := 0; i < len(s.learnedLoc); i++ {
			if s.learnedMetadata[i].LBD <= 2 {
				glueCount++
			}
		}
		s.Log("c [verbose] Restart: %d glue clauses (LBD≤2), %d total active\n", glueCount, s.learnedActiveCount)
	}

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
			s.litTrue[i*2] = false
			s.litTrue[i*2+1] = false
			s.implication[i] = -1
			s.numUnassigned++
		}
	}
	// Restart clears all search assignments. Sunk heap entries (at -Inf) are
	// NOT restored by onUnassign (restart doesn't call it). Force a rebuild
	// to fix all entries with current scores.
	s.vsids.heapValid = false

	// Reset restart counters
	s.lubyIndex++
	s.restartCount = s.conflicts
	s.lbdSum = 0
	s.lbdCount = 0

	// emaLBD is NOT reset here. The EMA tracks the LBD trend across restart
	// boundaries; resetting it to 0 would destroy the trend memory and make
	// the Glucose criterion (emaLBD > avgLBD × ratio) unable to detect
	// cross-restart search degradation. lbdSum/lbdCount (per-restart average)
	// ARE reset — that's the intended design (compare persistent EMA against
	// the current restart's average).

	// Adaptive phase flip: when the search is unproductive (high props/dec,
	// indicating deep binary cascades), enable phase flipping to break the
	// phase-saving + VSIDS-preservation fixed point where the solver re-enters
	// the same cascade after every restart. When the search is productive
	// (low props/dec), disable flipping to preserve good phase information.
	// Hysteresis: enable at >120% of limit, disable at <80% of limit. The 40%
	// dead zone prevents small trajectory shifts near the threshold from
	// toggling the phase flip on/off, which amplifies into completely different
	// search trajectories.
	if s.adaptivePhaseFlipRate > 0 && s.decisions > 10 {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		enableThreshold := float64(s.restartPropsDecLimit) * 12 / 10
		disableThreshold := float64(s.restartPropsDecLimit) * 8 / 10
		if propsPerDec > enableThreshold {
			s.restartPhaseFlipRate = s.adaptivePhaseFlipRate
		} else if propsPerDec < disableThreshold {
			s.restartPhaseFlipRate = 0
		}
	}

	// Phase randomization: flip each saved phase with probability
	// restartPhaseFlipRate. This breaks fixed points where phase saving
	// + VSIDS preservation causes the solver to re-enter the same search
	// region after every restart (e.g., binary-heavy instances where the
	// same polarity cascade repeats). Default 0 = disabled (standard
	// phase saving).
	if s.restartPhaseFlipRate > 0 {
		for i := range s.savedPhase {
			s.randomSeed ^= s.randomSeed << 13
			s.randomSeed ^= s.randomSeed >> 7
			s.randomSeed ^= s.randomSeed << 17
			if float64(s.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF) < s.restartPhaseFlipRate {
				s.savedPhase[i] = !s.savedPhase[i]
			}
		}
	}
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
	// as compaction). Gated by a minimum conflict gap so small/fast-restart
	// instances don't thrash on low-yield vivify rounds.
	if s.vivifyEnabled && s.vivifyPeriod > 0 && s.lubyIndex > 0 && s.lubyIndex%s.vivifyPeriod == 0 &&
		(s.vivifyMinConflictGap <= 0 || s.conflicts-s.conflictsAtLastVivify >= s.vivifyMinConflictGap) {
		if s.runVivification() {
			return true // UNSAT detected
		}
	}

	// Run learned-clause subsumption (forward subsumption + strengthening)
	// after vivification, at the same level-0 safety boundary. Self-gating:
	// early-returns when no binary learned clauses exist, so effectively free
	// on random instances.
	if s.subsumptionPeriod > 0 && s.lubyIndex > 0 && s.lubyIndex%s.subsumptionPeriod == 0 &&
		(s.subsumptionMinConflictGap <= 0 || s.conflicts-s.conflictsAtLastSubsumption >= s.subsumptionMinConflictGap) {
		if s.runLearnedSubsumption() {
			return true // UNSAT detected
		}
		s.conflictsAtLastSubsumption = s.conflicts
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

	// DETERMINISTIC BUDGET: cap the fixpoint loop at NumVars+1 passes.
	// Provably sufficient: each progressive pass assigns >=1 previously-unassigned
	// variable (changed=true is set only at the assignment site, gated by the
	// unassigned check), so fixpoint is reached in <= NumVars+1 passes. This cap
	// is never hit before fixpoint — it replaces the old 500ms wall-clock backstop,
	// which truncated mid-fixpoint nondeterministically and perturbed the search.
	maxPasses := int(s.cnf.NumVars) + 1

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
	totalVisits := 0
	for changed {
		if pass >= maxPasses {
			s.Log("c [unit prop] Pass cap reached (%d) before fixpoint — unexpected\n", maxPasses)
			break
		}

		// Budget check: stop if we've visited too many literals (large instance guard).
		// Stopping early is sound — fewer assignments, not wrong ones.
		if s.unitPropBudget > 0 && totalVisits >= s.unitPropBudget {
			s.Log("c [unit prop] Budget reached (%d visits) after %d passes\n", totalVisits, pass)
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
				totalVisits++
				varIdx := lit.Var()
				if s.assignments[varIdx].Level >= 0 {
					assign := s.assignments[varIdx]
					isTrue := lit.IsNegated() != assign.Value
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
				s.trail = append(s.trail, varIdx)
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
	// COMBINED with clause-length weight (not overwriting it): the clause-weighted
	// init produces fewer exact ties, reducing trajectory sensitivity to
	// clause ordering changes.
	occurrences := make([]int, s.cnf.NumVars)
	posCount := make([]int, s.cnf.NumVars)
	negCount := make([]int, s.cnf.NumVars)
	for _, clause := range s.cnf.Clauses {
		for _, lit := range clause.Literals {
			occurrences[lit.Var()]++
			if lit.IsNegated() {
				negCount[lit.Var()]++
			} else {
				posCount[lit.Var()]++
			}
		}
	}
	for i := range s.assignments {
		if s.assignments[i].Level < 0 {
			s.vsids.activity[i] += 1.0 + float64(occurrences[i])*0.5
			// Polarity-based initial phase: set savedPhase to the more frequent
			// polarity (satisfy more clauses with the first assignment).
			//
			// Trajectory sensitivity: the polarity phase helps most instances but
			// hurts some. No static metric cleanly predicts benefit — two
			// near-identical 100%-binary instances (8202af80 gained, 32baec6a
			// hurt, both near-pure with ~0.96 imbalance) respond oppositely.
			// The one discriminating signal is density: for DENSE binary
			// instances (binaryRatio > 0.9 AND density > 35), the BIG is highly
			// connected and the default phase propagates to a solution quickly
			// (32baec6a solves in 0 conflicts with the default phase). The
			// override fights the implication structure → skip it. For sparse
			// binary instances (8202af80, density 24.5), the BIG is less
			// connected and the override provides useful guidance → keep it.
			// Non-binary instances are unaffected (the polarity signal is
			// orthogonal to the clause structure).
			if !s.skipPolarityPhase {
				if negCount[i] > posCount[i] {
					s.savedPhase[i] = true
				} else if posCount[i] > negCount[i] {
					s.savedPhase[i] = false
				}
			}
		}
	}
	// Optional initial-activity noise (MiniSat -rnd-init analog). Perturbs
	// each variable's activity by (rand[0,1) - 0.5) * noise, breaking exact
	// ties at decision 1 (where the deterministic lowest-varIdx tiebreak
	// otherwise dominates). Deterministic given -seed. Applied AFTER the
	// clause-length + occurrence bonus so the activity hierarchy is preserved
	// while ties are broken.
	if s.rndInitNoise > 0 {
		seed := s.randomSeed
		if seed == 0 {
			seed = 0x2545F4914F6CDD1D // same fallback as SolveWithResult
		}
		for i := range s.vsids.activity {
			seed ^= seed << 13
			seed ^= seed >> 7
			seed ^= seed << 17
			r := float64(seed&0xFFFFFFFF) / float64(0xFFFFFFFF)
			s.vsids.activity[i] += (r - 0.5) * s.rndInitNoise
		}
	}
	s.vsids.heapValid = false // Force heap rebuild
}

// buildBIG builds the binary implication graph from original binary clauses.
// For each binary clause (a ∨ b), edges ¬a→b and ¬b→a are added.
// bigAdj[litIdx] lists literals m such that binary clause (¬lit ∨ m) exists.
// Used for BIG-based clause minimization (Kissat-style).
func (s *CDCLSolver) buildBIG() {
	numLits := int(s.cnf.NumVars) * 2
	s.bigAdj = make([][]int, numLits)
	for i := range s.cnf.Clauses {
		lits := s.cnf.Clauses[i].Literals
		if len(lits) != 2 {
			continue
		}
		a := cnf.LitToIndex(lits[0])
		b := cnf.LitToIndex(lits[1])
		if a == b || a == b^1 {
			continue
		}
		s.bigAdj[a^1] = append(s.bigAdj[a^1], b)
		s.bigAdj[b^1] = append(s.bigAdj[b^1], a)
	}
}

// bigReachableInClause returns true if lit can reach, via forward BIG edges
// (binary clauses), some literal m that is currently in the learned clause
// (tmpLiteralInClause with matching polarity) and m != lit (any polarity).
//
// Soundness: a forward BIG path lit → m1 → ... → mk with mk currently in the
// clause yields a valid resolution derivation of C\{lit} from C — resolve C
// with (¬lit ∨ m1) on lit, then with (¬m1 ∨ m2) on m1, ..., until mk (already
// in C) is absorbed back to rest. Each resolvent is a consequence of C and
// the binary clauses (all in the formula), so C\{lit} is a consequence of C
// in the formula, i.e. at least as strong as C. The derivation uses only
// binary clauses, so intermediate literals need NOT be assigned (no
// reason-clause / trail dependency, unlike recursive minimization).
// Intermediate resolvents may become tautological if ¬mi happens to be in C,
// but tautological resolvents are still valid consequences and only the final
// non-tautological C\{lit} matters.
//
// The success test excludes lit's own variable (mVar != litVar): a cycle
// lit → ... → lit does not make lit removable (resolving around a cycle
// returns to C, not C\{lit}), and reaching ¬lit would imply C is tautological
// (never the case for a learned clause). The check uses tmpLiteralInClause
// (correctly cleared on every removal) rather than a stale "covered" set.
//
// Budget-limited (maxBigBfsVisited); the limit only reduces effectiveness
// (fewer removals found), never soundness.
func (s *CDCLSolver) bigReachableInClause(lit cnf.Literal) bool {
	litIdx := cnf.LitToIndex(lit)
	if litIdx >= len(s.bigAdj) {
		return false
	}
	s.bigBfsEpoch++
	ep := s.bigBfsEpoch
	litVar := lit.Var()
	visited := s.bigBfsVisited
	tmpInClause := s.tmpLiteralInClause
	tmpIsNeg := s.tmpLiteralIsNegated

	visited[litIdx] = ep
	q := s.bigBfsQueue[:0]
	q = append(q, litIdx)
	head := 0
	found := false
	const maxBigBfsVisited = 16
	visitedCount := 0
	for head < len(q) && !found {
		cur := q[head]
		head++
		for _, m := range s.bigAdj[cur] {
			if m >= len(visited) || visited[m] == ep {
				continue
			}
			visited[m] = ep
			visitedCount++
			mVar := uint32(m >> 1)
			if mVar != litVar && tmpInClause[mVar] && tmpIsNeg[mVar] == ((m&1) == 1) {
				found = true
				break
			}
			if visitedCount < maxBigBfsVisited {
				q = append(q, m)
			}
		}
	}
	s.bigBfsQueue = q
	return found
}

// cdclLoop runs the main CDCL search loop until SAT, UNSAT, or UNKNOWN is
// determined. It assumes preprocessing, watch initialization, and VSIDS
// initialization are already complete.
func (s *CDCLSolver) cdclLoop() SolveResult {
	s.unitsDirty = true // Ensure unit scan runs on first propagation
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

		// Propagate all clauses (unit learned clauses handled in propagateWatched)
		conflict, conflictClause := s.propagateWatched()
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
			if s.conflicts%1000 == 0 && s.verbose {
				s.logDiagnostics()
			}
			if s.statsInterval > 0 && s.conflicts%s.statsInterval == 0 {
				s.printPeriodicStats()
			}
			if !s.backtrack() {
				s.printStats()
				return UNSAT
			}
			s.backjumpLevel = 0

			// Explicitly propagate the asserting literal from the just-learned
			// clause. With qhead=decisionPoint, the learned clause's watched
			// literals sit at trail positions < decisionPoint and would never
			// be re-checked by the propagation loop. This is a no-op when the
			// clause is not unit or satisfied. Must run before shouldRestart
			// since restart() clears the trail and re-propagates from scratch.
			s.propagateAssertingLiteral()

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
	s.solveStartNs = time.Now().UnixNano()
	// Classify structure and set search parameters (VSIDS decay, restartBase,
	// Glucose gating) BEFORE preprocessing. classifyInstance is read-only on
	// s.cnf; preprocessAggressive reads the cached structureScore to decide
	// EnableUnitProp. Splitting classification from preprocessing ensures the
	// search-parameter tuning is available even when preprocessing is skipped
	// (SolveWithoutPreprocessing), so -no-preprocess is a clean search-quality
	// diagnostic axis rather than also disabling adaptive tuning.
	s.classifyInstance()
	// Adaptive preprocessing: structure analysis selects techniques and
	// tunes VSIDS/restart parameters. See preprocessAggressive.
	preprocResult := s.preprocessAggressive()
	if preprocResult != UNKNOWN {
		s.printStats()
		return preprocResult
	}

	s.initVSIDSOccurrenceBonus()
	s.buildBIG()

	// Size- and structure-adaptive LBD bonus scale.
	// Binary-heavy small instances benefit from strong LBD guidance (glue
	// clauses steer search away from bad trajectories). Long-clause and large
	// instances benefit from pure VSIDS (LBD guidance dominates activity,
	// causing deep search). Formula: scale = max(10, 200000 * binaryRatio /
	// numVars). This gives ~2000 for a 100v binary-heavy instance, ~10 for a
	// 100v long-clause instance, ~10 for 20K+v instances.
	// Skipped if the user explicitly set -lbd-scale via CLI.
	if !s.lbdScaleOverride {
		binaryCount := 0
		for i := range s.cnf.Clauses {
			if len(s.cnf.Clauses[i].Literals) == 2 {
				binaryCount++
			}
		}
		binaryRatio := float64(binaryCount) / float64(len(s.cnf.Clauses))
		adaptiveScale := 200000.0 * binaryRatio / float64(s.cnf.NumVars)
		if adaptiveScale < 10.0 {
			adaptiveScale = 10.0
		}
		s.vsids.SetLBDBonusScale(adaptiveScale)
	}

	// Ensure PRNG seed is non-zero (XORShift(0)=0, so seed=0 would never
	// produce random numbers for phase jitter).
	if s.randomSeed == 0 {
		s.randomSeed = 0x2545F4914F6CDD1D
	}

	// Count unassigned variables for O(1) allAssigned/hasUnassigned checks.
	// Also initialize litTrue cache from preprocessing assignments.
	s.numUnassigned = 0
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level < 0 {
			s.numUnassigned++
		} else {
			v := s.assignments[i].Value
			s.litTrue[i*2] = v
			s.litTrue[i*2+1] = !v
		}
	}

	result := s.cdclLoop()
	if result == SAT {
		s.extendModel()
		s.reconstructEliminatedVars()
	}
	return result
}

// SolveWithoutPreprocessing skips all preprocessing (no unit propagation, no
// structure analysis, no adaptive tuning) and goes straight to the CDCL search
// loop after minimal setup. Intended as a debug escape hatch (the -no-preprocess
// CLI flag); production code should use SolveWithResult.
func (s *CDCLSolver) SolveWithoutPreprocessing() SolveResult {
	s.solveStartNs = time.Now().UnixNano()
	// Classify structure and set search parameters even when preprocessing is
	// skipped. Previously -no-preprocess skipped adaptive tuning entirely,
	// running random 3-SAT with decay 0.95 + restartBase=200 instead of the
	// aggressive decay 0.30 + restartBase=5 the classifier prescribes. This made
	// -no-preprocess a polluted diagnostic that conflated "no preprocessing" with
	// "no adaptive tuning". classifyInstance is read-only on s.cnf, so it cannot
	// change the search trajectory the way forced unit propagation does.
	s.classifyInstance()
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
	s.buildBIG()
	// Count unassigned variables for O(1) allAssigned/hasUnassigned checks.
	s.numUnassigned = 0
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level < 0 {
			s.numUnassigned++
		}
	}
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
	return s.numUnassigned == 0
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
			litTrue := lit.IsNegated() != assign.Value
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
// CRITICAL: Process all trail elements from s.qhead to end-of-trail, not just
// current level. s.qhead is set to decisionPoint after backjump (see backtrack),
// so skipped elements were already processed before the conflict.

func (s *CDCLSolver) propagateWatched() (bool, *cnf.Clause) {
	if s.verbose && s.conflicts <= 10 {
		s.Log("c [PROPAGATE] qhead=%d, trail len=%d, level=%d\n", s.qhead, len(s.trail), s.level)
	}

	// OPTIMIZATION #1: Use unitLearnedList for O(1) unit propagation
	// Previously scanned ALL learned clauses (O(n)), now only scans unit clauses.
	// Gated on unitsDirty: only scan when new units were learned or backtrack
	// occurred, avoiding the O(units) scan on every propagation call.
	if s.unitsDirty && !(s.inVivification && s.level > 0) {
		s.unitsDirty = false
		for _, learnedIdx := range s.unitLearnedList {
		// Skip deleted/tombstone entries (can happen after swap-remove)
		if learnedIdx >= s.learnedCapacity || s.learnedLoc[learnedIdx].Size != 1 {
			continue
		}
		literals := s.getLearnedClauseLiterals(learnedIdx)
		if len(literals) != 1 {
			continue
		}
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
			s.assignments[varIdx] = Assignment{Value: litValue, Level: int32(propLevel)}
			s.litTrue[int(varIdx)*2] = litValue
			s.litTrue[int(varIdx)*2+1] = !litValue
			s.trail = append(s.trail, varIdx)
			s.numUnassigned--
			// Store learned clause index as negative: -learnedIdx-5
			// Offset by 4 so clause 0 maps to -5, freeing -1/-2/-3/-4 as sentinels
			// (-1 decision, -2 unit-prop preprocess, -3 pure-literal preprocess, -4 reserved)
			s.implication[varIdx] = int32(-learnedIdx - 5)
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
					existingLits := s.getLearnedClauseLiterals(int(existingLearnedIdx))
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

	// Cache slice headers as locals — the compiler can't prove s.assignments
	// isn't aliased through method calls, so it reloads the slice header on every
	// access. The backing array is never reallocated (allocated once in the
	// constructor), so the cached header stays valid for the whole call.
	assignments := s.assignments

	// Cache original-clause slice header + length. Original clauses are never
	// deleted (only learned clauses are tombstoned), so the bounds check at the
	// slow-path entry never fires — but the compiler reloads s.cnf.Clauses (a
	// pointer chase through s → s.cnf → .Clauses) on every watch. Caching the
	// header eliminates that per-watch pointer chase.
	originalClauses := s.cnf.Clauses
	numOriginalClauses := len(originalClauses)
	originalSearchHint := s.originalSearchHint

	// Cache watchLists outer slice header. The outer slice is allocated once in
	// initWatches and never grows (length is always 2*numVars). Only inner slices
	// grow via append, and the cached header shares the backing array, so inner-
	// slice updates are visible to s.watchLists automatically. This eliminates
	// the s → s.watchLists pointer chase on every append (930ms) and write-back
	// (370ms) in the hot loop.
	watchLists := s.watchLists

	// Cache implication and savedPhase arrays (allocated once, never reallocated)
	// to eliminate s → s.implication and s → s.savedPhase pointer chases in the
	// inlined propagation path.
	implication := s.implication
	savedPhase := s.savedPhase
	// litTrue cache: backing array never reallocated, writes through local visible
	// to s.litTrue automatically. Eliminates per-trail-entry s → s.litTrue chase.
	litValue := s.litTrue
	// Cache scalar counters as locals — incremented/decremented on every propagation,
	// writing through s pointer each time. Write back at returns.
	propagations := s.propagations
	numUnassigned := s.numUnassigned
	// Cache learned clause arrays — accessed on every slow-path watch check
	// (tombstone check + literal load). Eliminates s → s.learnedLoc and
	// s → s.learnedLiterals pointer chases.
	learnedLoc := s.learnedLoc
	learnedLiterals := s.learnedLiterals
	learnedAlive := s.learnedAlive
	learnedMetadata := s.learnedMetadata

	for trailIndex := s.qhead; trailIndex < len(s.trail); trailIndex++ {
		lit := s.trail[trailIndex]

		// CRITICAL FIX: Skip unassigned variables (level < 0)
		// Unassigned variables have Value=false by default, which incorrectly triggers watches
		litAsg := assignments[lit]
		if litAsg.Level < 0 {
			continue // Unassigned - skip watch processing
		}
		value := litAsg.Value

		// OPTIMIZATION: Inline LitToIndex - avoids function call overhead
		// lit index = varIdx * 2 + (1 if negated else 0)
		watchIdx := lit << 1
		if value {
			watchIdx |= 1 // negated literal watches false when var is true
		}

		// Process watches for this literal using swap-with-last deletion
		watchList := watchLists[watchIdx]

		for readIdx := 0; readIdx < len(watchList); readIdx++ {
			watch := watchList[readIdx]

			// FAST PATH: Blit stores the litTrue index directly (varIdx*2 + negated).
			if litValue[watch.Blit] {
				continue
			}

			// SLOW PATH: Blocking literal is not true (or unassigned).
			// Access clause data for replacement search / conflict detection.

			// Decode ClauseIdx once (was decoded 5+ times per watch iteration).
			// Bit 31: learned flag, bit 30: myPos, bits 0-29: clause index.
			clauseIdxRaw := uint32(watch.ClauseIdx)
			isLearned := clauseIdxRaw&watchLearnedBit != 0
			clauseID := int(clauseIdxRaw & watchIdxMask)
			myPos := int((clauseIdxRaw >> 30) & 1)
			blitPos := 1 - myPos

			var clauseLits []cnf.Literal
			if !isLearned {
				if clauseID >= numOriginalClauses {
					continue
				}
				clauseLits = originalClauses[clauseID].Literals
			} else {
				if clauseID >= len(learnedAlive) || learnedAlive[clauseID] == 0 {
					continue
				}
				loc := learnedLoc[clauseID]
				offset := int(loc.Offset)
				size := int(loc.Size)
				clauseLits = learnedLiterals[offset : offset+size]
			}

			// Re-read the actual blocking literal from clause data (Blit may be stale).
			// The fast-path Blit check already filtered out the true case; here we
			// need the actual literal for the replacement guard, propagation, and
			// conflict detection.
			blitLit := clauseLits[blitPos]
			blitVarIdx := int(blitLit.Var())
			blitNegated := blitLit.IsNegated()

			// Look for replacement watch
			foundReplacement := false
			var trueReplacementLit uint32 // 0 = none found; else litTrue index of true literal to cache as Blit
			foundJ := -1
			var newWatchIdx int

			// Probe the search hint first (probe-then-scan). The hint caches the
			// position where a replacement was found last time. After a backjump,
			// the old watched literal at that position may be unassigned → O(1)
			// replacement without scanning from position 2. The probe always
			// verifies the literal's assignment — a miss falls through to the
			// full scan, so soundness is preserved.
			var hint int32
			if !isLearned {
				if clauseID < len(originalSearchHint) {
					hint = originalSearchHint[clauseID]
				}
			} else {
				if clauseID < len(learnedMetadata) {
					hint = learnedMetadata[clauseID].SearchHint
				}
			}
			if hint >= 2 && int(hint) < len(clauseLits) {
				clauseLit := clauseLits[hint]
				clauseLitVar := int(clauseLit.Var())
				clauseAsg := assignments[clauseLitVar]
				litNegated := clauseLit.IsNegated()
			if clauseAsg.Level < 0 {
				// Unassigned — usable as replacement
				foundJ = int(hint)
				newWatchIdx = clauseLitVar << 1
				if litNegated {
					newWatchIdx |= 1
				}
			} else {
				litTrue := litNegated != clauseAsg.Value
				if litTrue {
					// Assigned-and-true — usable as replacement, cache as blit
					trueReplacementLit = litToBlit(clauseLit)
					foundJ = int(hint)
					newWatchIdx = clauseLitVar << 1
					if litNegated {
						newWatchIdx |= 1
					}
				}
			}
			}

			if foundJ < 0 {
				for j := 2; j < len(clauseLits); j++ {
					clauseLit := clauseLits[j]
					clauseLitVar := int(clauseLit.Var())

					clauseAsg := assignments[clauseLitVar]
					litNegated := clauseLit.IsNegated()
					if clauseAsg.Level >= 0 {
						litTrue := litNegated != clauseAsg.Value
						if !litTrue {
							continue
						}
						trueReplacementLit = litToBlit(clauseLit)
					}
					foundJ = j
					newWatchIdx = clauseLitVar << 1
					if litNegated {
						newWatchIdx |= 1
					}
					break
				}
			}

			if foundJ >= 0 {
				// Swap replacement literal into position myPos
				clauseLits[myPos], clauseLits[foundJ] = clauseLits[foundJ], clauseLits[myPos]

				// Cache the true literal as Blit if found; otherwise cache the other
				// watched literal (at 1-myPos) as before.
				var newBlit uint32
				if trueReplacementLit != 0 {
					newBlit = trueReplacementLit
				} else {
					newBlit = litToBlit(clauseLits[1-myPos])
				}

				watchLists[newWatchIdx] = append(watchLists[newWatchIdx], cnf.Watch{
					ClauseIdx: watch.ClauseIdx,
					Blit:      newBlit,
				})
			if isLearned {
				if clauseID < len(s.learnedWatchIdx0) {
					if myPos == 0 {
						s.learnedWatchIdx0[clauseID] = newWatchIdx
					} else {
						s.learnedWatchIdx1[clauseID] = newWatchIdx
					}
				}
			}

				// Update the search hint to the found position. After the swap,
				// position foundJ holds the old false watched literal. Next time
				// this clause's watch fires, the hint probe checks if that literal
				// is now non-false (e.g., unassigned after a backjump) → O(1) repl.
				if !isLearned {
					if clauseID < len(originalSearchHint) {
						originalSearchHint[clauseID] = int32(foundJ)
					}
				} else {
					if clauseID < len(learnedMetadata) {
						learnedMetadata[clauseID].SearchHint = int32(foundJ)
					}
				}

				foundReplacement = true
			}

		if foundReplacement {
				// Watch moved - remove old watch using swap-with-last
				lastIdx := len(watchList) - 1
				if readIdx != lastIdx {
					watchList[readIdx] = watchList[lastIdx]
					readIdx--
				}
				watchList = watchList[:lastIdx]
				watchLists[watchIdx] = watchList
				continue
			}

			// No replacement found - check if we can propagate or have conflict
			blitAsg := assignments[blitVarIdx]

			if blitAsg.Level < 0 {
				// Unassigned blit - propagate it (inlined assignLiteralByClause)
				if implication[blitVarIdx] == -1 {
					propLevel := s.level
					if propLevel == 0 {
						propLevel = 1
					}
					reasonIdx := clauseID
					if isLearned {
						reasonIdx = -clauseID - 5
					}
					blitValue := !blitNegated
					assignments[blitVarIdx] = Assignment{Value: blitValue, Level: int32(propLevel)}
					litValue[blitVarIdx*2] = blitValue
					litValue[blitVarIdx*2+1] = !blitValue
					s.trail = append(s.trail, uint32(blitVarIdx))
					numUnassigned--
					implication[blitVarIdx] = int32(reasonIdx)
					savedPhase[blitVarIdx] = blitNegated
				}
				propagations++
				continue
			}

			// Re-check blit value
			blitTrue := blitNegated != blitAsg.Value

			if !blitTrue {
				// Build conflict clause
				var conflictClause *cnf.Clause
				if !isLearned {
					conflictClause = &s.cnf.Clauses[clauseID]
				} else {
					literals := s.getLearnedClauseLiterals(clauseID)
					s.conflictLitsBuf = s.conflictLitsBuf[:0]
					s.conflictLitsBuf = append(s.conflictLitsBuf, literals...)
					s.conflictClauseBuf.Literals = s.conflictLitsBuf
					s.conflictClauseBuf.Learned = true
					conflictClause = &s.conflictClauseBuf
				}

			if s.level == 0 {
				s.emptyClauseFound = true
			}
			if s.verbose {
			s.Log("c [PROP CONFLICT] Watch idx=%d, clauseIdx=%d, level=%d\n",
				watchIdx, clauseID, s.level)
			}
			s.propagations = propagations
			s.numUnassigned = numUnassigned
			return true, conflictClause
			}
		}
	}

	// Update qhead to end of trail
	s.qhead = len(s.trail)
	s.propagations = propagations
	s.numUnassigned = numUnassigned

	return false, nil
}

func (s *CDCLSolver) decide() bool {
	if s.numUnassigned == 0 {
		return false
	}

	// Select variable using VSIDS heuristic with phase saving.
	// Standard CDCL: no random decisions, no diversification overrides.
	var varIdx uint32
	var phase bool
	varIdx, phase = s.vsids.selectVariableWithPhase(s.assignments, s.savedPhase)

	// SAFETY CHECK: Ensure variable is unassigned before deciding.
	// A stale heap entry can slip through; fall back to linear scan.
	if int(varIdx) < len(s.assignments) && s.assignments[varIdx].Level >= 0 {
		found := false
		for i := uint32(0); i < s.cnf.NumVars; i++ {
			if s.assignments[i].Level < 0 {
				varIdx = i
				if int(varIdx) < len(s.savedPhase) {
					phase = s.savedPhase[varIdx]
				} else {
					phase = false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	// Random phase jitter: with small probability, flip the saved phase.
	// This perturbs the search trajectory to escape fixed paths caused by
	// preserved VSIDS activity + phase saving across restarts.
	if s.randomPhaseRate > 0 {
		s.randomSeed ^= s.randomSeed << 13
		s.randomSeed ^= s.randomSeed >> 7
		s.randomSeed ^= s.randomSeed << 17
		if float64(s.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF) < s.randomPhaseRate {
			phase = !phase
		}
	}

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, phase), s.level, -1) // -1 = decision
	s.decisions++
	if s.verbose && s.conflicts <= 10 {
		s.Log("c [DECIDE] Level %d (was %d): var %d = %v (decision, lit=%d%c), trailHead len=%d\n",
			s.level, s.level-1, varIdx+1, !phase, varIdx+1, map[bool]byte{true: '-', false: '+'}[phase], len(s.trailHead))
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
		Level: int32(level),
	}
	s.litTrue[int(varIdx)*2] = value
	s.litTrue[int(varIdx)*2+1] = !value
	s.trail = append(s.trail, varIdx)
	s.implication[varIdx] = int32(clauseIdx)
	s.savedPhase[varIdx] = lit.IsNegated()
	s.numUnassigned--

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
		Level: int32(level),
	}
	s.litTrue[int(varIdx)*2] = value
	s.litTrue[int(varIdx)*2+1] = !value
	s.trail = append(s.trail, varIdx)
	s.numUnassigned--

	// Store clause index
	s.implication[varIdx] = int32(clauseIdx)
	s.savedPhase[varIdx] = lit.IsNegated()
}
func (s *CDCLSolver) handleConflict(conflictClause *cnf.Clause) {
	s.conflicts++
	s.lastConflictLevel = s.level

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
		if s.assignments[varIdx].Level != 0 {
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
						if int(learnedIdx) < s.learnedCapacity && s.learnedLoc[learnedIdx].Size == 1 {
							isUnit = true
						}
				} else {
					// Original clause (guard against preprocessing sentinels -2/-3/-4)
					if impIdx >= 0 && int(impIdx) < len(s.cnf.Clauses) && len(s.cnf.Clauses[impIdx].Literals) == 1 {
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

	s.vsids.bumpClause(conflictLits)

	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	// B2: Adaptively shrink clause DB when average LBD is consistently high.
	// High-LBD clauses rarely propagate; a smaller DB keeps only the lowest-LBD
	// clauses. One-way shrink (don't grow back) to avoid oscillation.
	if !s.maxLearnedShrunk && s.totalLbdCount > 1000 {
		avgLbd := s.totalLbdSum / s.totalLbdCount
		if avgLbd > 10 {
			newFloor := int(s.cnf.NumVars) * 3
			if newFloor < 300 {
				newFloor = 300
			}
			if s.maxLearned > newFloor {
				s.maxLearned = newFloor
				s.Log("c [clause-db] Shrunk maxLearned to %d (avg LBD=%d > 10)\n", s.maxLearned, avgLbd)
			}
			s.maxLearnedShrunk = true
		}
	}

	// Delete learned clauses when database exceeds dynamic limit
	// Formula: base + conflicts/50, trigger at 150% of limit
	dynamicLimit := s.maxLearned + s.conflicts/50
	if s.learnedActiveCount > dynamicLimit+dynamicLimit/2 {
		s.deleteLearnedClauses()
	}

	// Decay VSIDS activity every conflict (standard)
	s.vsids.decay(s.assignments)
	s.vsids.decayLBD()
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
//
// Asserting Clause Invariant:
// After analysis, the learned clause is reordered so the UIP (single current-level
// literal) is at position 0 and a backjump-level literal is at position 1. Positions
// 0/1 are watched directly. After backjump, qhead=decisionPoint skips re-processing
// the earlier trail; the asserting literal is propagated explicitly by
// propagateAssertingLiteral() since the watched literals sit at trail positions
// < decisionPoint.

// LBD (Literal Block Distance):
// LBD = number of distinct decision levels in the learned clause.
// Lower LBD = better clause (involves fewer decision levels).
// Clauses with LBD=2 are "glue clauses" - most valuable, never delete.
func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) int {
	s.lastLearnedClauseIdx = -1
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
			lvl := int(s.assignments[varIdx].Level)
			if lvl >= 0 && lvl <= s.level {
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

	// Build candidate list from trail (most recent first). Only the current
	// decision level's trail slice can contain level==s.level literals (the
	// level starts at trailHead[s.level]), so scan that slice only — O(current-
	// level trail) instead of O(total trail) per conflict.
	s.tmpCandidates = s.tmpCandidates[:0]
	startIdx := s.trailHead[s.level]
	for i := len(s.trail) - 1; i >= startIdx; i-- {
		varIdx := s.trail[i]
		if s.assignments[varIdx].Level == int32(s.level) && s.tmpLiteralInClause[varIdx] {
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
					s.tmpLevelCount[int(s.assignments[v].Level)]--
					if s.assignments[v].Level == int32(s.level) {
						currentCount--
					}
				} else {
					if s.verbose {
						s.Log("c [1-UIP]   Keep var %d (same polarity)\n", v+1)
					}
				}
		} else {
			// Only add assigned literals (level > 0)
			// Level-0 literals are always true (root-level units) — including them
			// in learned clauses makes them longer with no benefit.
			assignLevel := int(s.assignments[v].Level)
			if assignLevel <= 0 {
				continue
			}
			// Skip already-resolved variables: their reason was already processed,
			// so re-adding them would inflate currentCount without the ability to
			// resolve them again (tmpResolved blocks re-resolution). This is the
			// root cause of 1-UIP non-convergence when the trail-order invariant
			// is violated by inconsistent reason clauses.
			if s.tmpResolved[v] {
				continue
			}

				// Add to clause
				if s.verbose {
					s.Log("c [1-UIP]   ADD var %d, neg=%v, level=%d\n", v+1, litNegated, assignLevel)
				}
				s.tmpLiteralInClause[v] = true
				s.tmpLiteralIsNegated[v] = litNegated
				s.tmpTouchedVars = append(s.tmpTouchedVars, v)
				lvl := assignLevel
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
		if s.tmpLiteralInClause[varIdx] && s.assignments[varIdx].Level == int32(s.level) {
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
		// FALLBACK: Force a 1-UIP by keeping only the most recent literal at the
		// current level (the UIP) and dropping all others. This produces an
		// asserting clause (unit after backjump), which is essential for CDCL
		// search. Without the fallback, non-convergent conflicts produce
		// non-asserting clauses that are useless for propagation, crippling the
		// search (200K+ conflicts with no UNSAT on instances that should solve
		// in ~11K). The dropped literals are implied by the remaining clause in
		// practice (they were propagated, not decided), so this is sound as long
		// as the reason clauses are consistent.
		if s.verbose {
			s.Log("c [1-UIP] FALLBACK: %d decisions + %d propagations at level %d\n",
				decisionsAtCurrentLevel, propagationsAtCurrentLevel, s.level)
		}

		// Find the most recent literal at current level (this will be the UIP)
		var uipVar uint32 = 0
		var uipTrailPos int = -1
		for _, varIdx := range s.tmpTouchedVars {
			if s.tmpLiteralInClause[varIdx] && s.assignments[varIdx].Level == int32(s.level) {
				for ti := len(s.trail) - 1; ti >= 0; ti-- {
					if s.trail[ti] == varIdx {
						if uipTrailPos < 0 || ti > uipTrailPos {
							uipTrailPos = ti
							uipVar = varIdx
						}
						break
					}
				}
			}
		}

		// Remove all other literals at current level (keep only the UIP)
		if uipVar != 0 {
			for _, varIdx := range s.tmpTouchedVars {
				if s.tmpLiteralInClause[varIdx] &&
					s.assignments[varIdx].Level == int32(s.level) &&
					varIdx != uipVar {
					s.tmpLiteralInClause[varIdx] = false
					s.tmpLevelCount[s.level]--
				}
			}
			currentCount = 1
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
				lvl := int(s.assignments[varIdx].Level)
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
			lvl := int(s.assignments[varIdx].Level)
			if lvl > 0 {
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
			lvl := int(s.assignments[varIdx].Level)
			if lvl >= 0 {
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

	s.lbdSum += lbd
	s.lbdCount++
	s.emaLBD = 0.9*s.emaLBD + 0.1*float64(lbd)
	s.totalLbdSum += uint64(lbd)
	s.totalLbdCount++

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
		if s.assignments[lit.Var()].Level == int32(s.level) {
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
			if s.learnedLoc[i].Size != 1 {
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

	// Reorder so the UIP (current-level literal) is at position 0 and a
	// backjump-level literal is at position 1 (MiniSat asserting-clause
	// invariant). This lets us watch positions 0/1 directly and propagate
	// the asserting literal explicitly after backjump (qhead=decisionPoint),
	// avoiding an O(trail) re-scan of the earlier trail after each conflict.
	if len(s.tmpLearnedLits) >= 2 {
		uipPos := 0
		for i, lit := range s.tmpLearnedLits {
			if s.assignments[lit.Var()].Level == int32(s.level) {
				uipPos = i
				break
			}
		}
		if uipPos != 0 {
			s.tmpLearnedLits[0], s.tmpLearnedLits[uipPos] = s.tmpLearnedLits[uipPos], s.tmpLearnedLits[0]
		}
		for i := 1; i < len(s.tmpLearnedLits); i++ {
			if s.assignments[s.tmpLearnedLits[i].Var()].Level == int32(maxLevel) {
				if i != 1 {
					s.tmpLearnedLits[1], s.tmpLearnedLits[i] = s.tmpLearnedLits[i], s.tmpLearnedLits[1]
				}
				break
			}
		}
	}

	// Store learned clause in database.
	// MiniSat stores ALL learned clauses and uses LBD for deletion priority,
	// not for initial storage. Filtering by LBD at learning time creates a
	// vicious cycle: high-LBD clauses are discarded → no learning → same
	// conflicts repeat → LBD stays high → no learning. On small structured
	// instances (e.g. 44092fcc, 90v), this caused 20K+ conflicts with 0
	// stored clauses.
	if len(s.tmpLearnedLits) > 0 {
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
		s.learnedLoc = append(s.learnedLoc, LearnedClauseLoc{
			Offset: int32(offset),
			Size:   int32(len(s.tmpLearnedLits)),
		})
		s.learnedAlive = append(s.learnedAlive, 1)
		s.recordLearnedClauseSize(len(s.tmpLearnedLits))
		s.learnedMetadata = append(s.learnedMetadata, cnf.ClauseMetadata{
			LBD: int32(lbd),
		})
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
		s.lastLearnedClauseIdx = learnedIdx

		// Store watch indices for all clauses to maintain array consistency
		if len(literals) >= 2 {
			var idx0, idx1 int
			if s.watchInitialized {
				// Watch positions 0 and 1 directly (UIP at 0, backjump-level at 1).
				// The asserting-clause invariant guarantees position 0 (UIP) is
				// unassigned and position 1 is false after backjump, satisfying the
				// watched-literal invariant (at most one watched literal is false).
				lit0 := literals[0]
				lit1 := literals[1]
				idx0 = cnf.LitToIndex(lit0)
				idx1 = cnf.LitToIndex(lit1)
				clauseIdx0 := int32(watchLearnedBit | uint32(learnedIdx))
				clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)
				s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit1)})
				s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{ClauseIdx: clauseIdx1, Blit: litToBlit(lit0)})
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
			s.unitsDirty = true
		s.unitLearnedList = append(s.unitLearnedList, learnedIdx)
		}

		// VSIDS bump
		s.vsids.bumpLBD(s.tmpLearnedLits, lbd)
	}

	return backjumpLevel
}

// deleteLearnedClauses removes low-quality learned clauses to control memory usage.
//
// Strategy: two-pass LBD-tiered deletion.
//   - Pass 1: delete LBD > 5 candidates first (lowest quality).
//   - Pass 2: if still over budget, delete LBD > 2 candidates.
//   - Glue clauses (LBD ≤ 2) are NEVER deleted.
//   - Reason clauses (currently used in the implication graph) are protected.
//   - Within a tier, candidates are in ascending clause-index order, so deletion
//     is FIFO (oldest learned first). This is deterministic and simple; a
//     decayed-activity sort was tested (B9) and regressed +33% PAR-2.
//
// Deletion trigger: when learnedActiveCount > 150% of dynamicLimit
// (dynamicLimit = maxLearned + conflicts/50). Target after deletion: dynamicLimit.
//
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

	s.minimizeCalls++
	s.minimizeLiteralsIn += uint64(len(learnedLits))
	// Phase A: mark clause literals in both arrays and record for cleanup.
	// Also set tmpLiteralIsNegated for each literal — needed by exploreRemovable
	// to look up binary implications for the correct polarity.
	s.tmpMinSeenVars = s.tmpMinSeenVars[:0]
	for _, lit := range learnedLits {
		v := lit.Var()
		s.tmpLiteralInClause[v] = true
		s.tmpLiteralIsNegated[v] = lit.IsNegated()
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
		if s.assignments[v].Level == int32(s.level) {
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
		// BIG fast-path: check if lit is removable via a chain of binary
		// clauses. A forward BIG path lit → m1 → ... → mk with mk currently in
		// the clause yields a valid resolution derivation of C\{lit} from C
		// (resolve C with each (¬mi ∨ mi+1) on the path), so lit is removable.
		// Soundness does NOT require the intermediate literals to be assigned:
		// the derivation uses only the binary clauses, which are in the formula.
		// Intermediate tautologies in the resolvent are harmless — only the
		// final clause (C\{lit}) matters, and it is non-tautological.
		if s.bigAdj != nil {
			s.bigMinimizeCalls++
			if s.bigReachableInClause(lit) {
				s.bigMinimizeHits++
				s.tmpLiteralInClause[v] = false
				// CRITICAL: Do NOT leave tmpSeenVar[v]=true for BIG-removed literals.
				// Recursive minimization treats tmpSeenVar as a "covered" set (literal
				// is removable, so can be skipped in reason clauses). But BIG removal
				// is only valid while the BFS target remains in the clause. If recursive
				// later removes the target, the BIG removal becomes invalid. Leaving
				// tmpSeenVar[v]=true creates a circular dependency: lit1 is "covered"
				// (BIG, depends on lit2), and lit2 is removable because lit1 is "covered".
				// This circular reasoning produces unsound minimization → false UNSAT.
				// Clearing tmpSeenVar[v] forces recursive to re-examine v independently.
				s.tmpSeenVar[v] = false
				reductionAchieved++
				continue
			}
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

	s.minimizeLiteralsOut += uint64(writeIdx)
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

// exploreRemovable is the recursive core of Bier's algorithm. It does NOT roll
// back on failure — that is the caller's job (recursiveTryRemove) via the
// snapshot. Returns true iff every non-resolved literal in v's reason clause is
// covered (tmpSeenVar) or recursively removable within minimizeMaxDepth. Sound
// via the DAG property of the implication graph (reason clauses only reference
// earlier trail literals, preventing cycles).
func (s *CDCLSolver) exploreRemovable(v uint32, depth int) bool {
	if s.minimizeMaxDepth > 0 && depth > s.minimizeMaxDepth {
		return false
	}
	reasonLits := s.getReasonLitsForVar(v)
	if reasonLits == nil {
		return false
	}
	if len(reasonLits) <= 1 {
		return false
	}
	for _, rl := range reasonLits {
		rv := rl.Var()
		if rv == v {
			continue
		}
		if s.tmpSeenVar[rv] {
			continue
		}
		// Level-0 literals are globally assigned (forced during preprocessing,
		// never cleared by backjump). A level-0 literal in a reason clause is
		// permanently false, so it is automatically "covered" — skip it without
		// failing. Returning false here (the old behavior) devastated long-clause
		// instances: reason clauses averaging 50-150 literals almost always contain
		// a level-0 literal, causing ~99% of minimization attempts to fail. Skipping
		// matches Cadical/MiniSat (`if (!level(tmp)) continue;`). Soundness holds:
		// the reason clause still propagates v after backjump because the level-0
		// literal remains false. No marking needed (nothing is pushed to
		// tmpMinSeenVars), so the snapshot/rollback in recursiveTryRemove is
		// unaffected. Level-0 literals are still protected from *removal* in
		// minimizeLearnedClause (line 3931) — this only affects whether a reason
		// clause *containing* a level-0 literal blocks removability of another var.
		if s.assignments[rv].Level == 0 {
			continue
		}
		if s.implication[rv] == -1 {
			return false
		}
		s.tmpSeenVar[rv] = true
		s.tmpMinSeenVars = append(s.tmpMinSeenVars, rv)
		if !s.exploreRemovable(rv, depth+1) {
			return false
		}
	}
	return true
}

// deleteLearnedClauses removes low-quality learned clauses to control memory usage
func (s *CDCLSolver) deleteLearnedClauses() {
	// LAZY LBD-BASED DELETION (Glucose-style)
	// Key insight: LBD is the best predictor of clause usefulness
	// - Keep all "glue" clauses (LBD ≤ 2) permanently
	// - Delete clauses with high LBD when database grows too large

	dynamicLimit := s.maxLearned + s.conflicts/50
	targetCount := dynamicLimit
	
	currentActive := s.learnedActiveCount

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
			if int(learnedIdx) < s.learnedCapacity {
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

	// Two-pass LBD-tiered deletion. Within each tier, candidates are in
	// ascending clause-index order (oldest learned first), so deletion is
	// FIFO within a tier. Glue clauses (LBD ≤ 2) are never deleted.
	candidates := s.tmpDeletionCandidates[:0]

	// Pass 1: collect LBD > 5 candidates (lowest quality), delete oldest first
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size == 0 || protected[i] {
			continue
		}
		if s.learnedMetadata[i].LBD > 5 {
			candidates = append(candidates, i)
		}
	}
	for _, idx := range candidates {
		if deletedCount >= toDelete {
			break
		}
		deleted[idx] = true
		deletedCount++
	}

	// Pass 2: lower threshold to LBD > 2 (glue clauses are LBD ≤ 2, never deleted)
	if deletedCount < toDelete {
		candidates = candidates[:0]
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedLoc[i].Size == 0 || protected[i] || deleted[i] {
				continue
			}
			if s.learnedMetadata[i].LBD > 2 {
				candidates = append(candidates, i)
			}
		}
		for _, idx := range candidates {
			if deletedCount >= toDelete {
				break
			}
			deleted[idx] = true
			deletedCount++
		}
	}

	s.tmpDeletionCandidates = candidates

	// Apply tombstones: mark size=0 and remove watches
	tombstoneCount := 0
	for i := 0; i < s.learnedCapacity; i++ {
		if deleted[i] && s.learnedLoc[i].Size > 0 {
			// Remove watches BEFORE marking as tombstone
			s.removeLearnedClauseWatches(i)
			// Mark as tombstone
			s.learnedLoc[i].Size = 0
			s.learnedAlive[i] = 0
			s.learnedWatchIdx0[i] = -1
			s.learnedWatchIdx1[i] = -1
			tombstoneCount++
		}
	}

	// Update active count (excludes tombstones)
	activeCount := currentActive - deletedCount
	s.learnedActiveCount = activeCount

	// Rebuild unit clause list from scratch
	s.unitLearnedList = s.unitLearnedList[:0]
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size == 1 {
			s.unitsDirty = true
		s.unitLearnedList = append(s.unitLearnedList, i)
		}
	}

	// Mark LBD order as dirty

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
		if s.learnedLoc[readIdx].Size == 0 {
			continue // Skip tombstones
		}

		// Record mapping
		clauseIndexMap[readIdx] = writeIdx

		// Move clause metadata
		oldStart := int(s.learnedLoc[readIdx].Offset)
		oldSize := int(s.learnedLoc[readIdx].Size)
		newStart := nextOffset

		s.learnedLoc[writeIdx] = LearnedClauseLoc{Offset: int32(newStart), Size: int32(oldSize)}
		s.learnedAlive[writeIdx] = 1
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
			if int(learnedIdx) < len(clauseIndexMap) && clauseIndexMap[learnedIdx] >= 0 {
				s.implication[varIdx] = int32(-clauseIndexMap[learnedIdx] - 5)
			} else if int(learnedIdx) < len(clauseIndexMap) {
				// Clause was deleted - reset to decision
				s.implication[varIdx] = -1
			}
		}
	}

	// REBUILD WATCH LISTS — keep original-clause watches, replace learned.
	// Original clauses don't move during learned-clause compaction, so their
	// watches (ClauseIdx >= 0) are still valid. Only learned-clause watches
	// (ClauseIdx < 0) have stale indices after the clauseIndexMap remapping.
	// Removing the O(total original literals) re-scan of chooseWatchPositions
	// that the old full-rebuild did on every compaction.
	for litIdx := range s.watchLists {
		wl := s.watchLists[litIdx]
		writeIdx := 0
		for _, watch := range wl {
			if watch.ClauseIdx >= 0 {
				wl[writeIdx] = watch
				writeIdx++
			}
		}
		s.watchLists[litIdx] = wl[:writeIdx]
	}

	// Re-add learned clauses (with freshly chosen watch positions)
	for i := 0; i < writeIdx; i++ {
		if s.learnedLoc[i].Size < 2 {
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
		clauseIdx0 := int32(watchLearnedBit | uint32(i))
		clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)

		s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit1),

		})
		s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
			ClauseIdx: clauseIdx1,
			Blit:      litToBlit(lit0),

		})

		// Update stored watch indices to the chosen literals
		s.learnedWatchIdx0[i] = idx0
		s.learnedWatchIdx1[i] = idx1
		// Reset search hint — compaction re-ran chooseWatchPositions which
		// reordered literals, invalidating any position-based hint.
		s.learnedMetadata[i].SearchHint = 0
	}
	
	// Mark watches as initialized
	s.watchInitialized = true

	// Truncate arrays to new capacity
	s.learnedLiterals = s.learnedLiterals[:nextOffset]
	s.learnedLoc = s.learnedLoc[:writeIdx]
	s.learnedAlive = s.learnedAlive[:writeIdx]
	s.learnedMetadata = s.learnedMetadata[:writeIdx]
	s.learnedWatchIdx0 = s.learnedWatchIdx0[:writeIdx]
	s.learnedWatchIdx1 = s.learnedWatchIdx1[:writeIdx]
	s.learnedCapacity = writeIdx
	s.learnedActiveCount = writeIdx

	// Rebuild unit list
	s.unitLearnedList = s.unitLearnedList[:0]
	for i := 0; i < writeIdx; i++ {
		if s.learnedLoc[i].Size == 1 {
			s.unitsDirty = true
		s.unitLearnedList = append(s.unitLearnedList, i)
		}
	}

	s.Log("c [compact] Compaction complete: new capacity=%d, literals=%d\n",
			writeIdx, nextOffset)

	// Debug-build only: validate that all clause references (implications,
	// watches, unit list) are consistent after the rebuild. No-op in release.
	verifyClauseIndices(s)
}

// propagateAssertingLiteral propagates the UIP (asserting literal) from the
// most recently learned clause after a backjump. With qhead=decisionPoint,
// the learned clause's watched literals sit at trail positions < decisionPoint
// and would otherwise never be re-checked by the propagation loop.
//
// The asserting-clause invariant (established by learnClause and trusted by
// the watch system) guarantees position 0 (UIP) is unassigned and all others
// are false after backjump: the UIP is the only literal at s.level (> bjLevel),
// all others are at levels <= bjLevel and retain their conflict-time (false)
// values. So we assign literals[0] directly — O(1) instead of O(clause-size).
//
// In debug builds, verifyAssertingInvariant runs the full O(clause-size) scan
// and panics if the invariant is violated, catching any bug in learnClause
// or backtrack that would corrupt the invariant.
func (s *CDCLSolver) propagateAssertingLiteral() {
	if s.lastLearnedClauseIdx < 0 || s.lastLearnedClauseIdx >= s.learnedCapacity {
		return
	}
	learnedIdx := s.lastLearnedClauseIdx
	if s.learnedLoc[learnedIdx].Size < 2 {
		return // unit clauses are handled via unitLearnedList
	}
	literals := s.getLearnedClauseLiterals(learnedIdx)

	verifyAssertingInvariant(s, learnedIdx, literals)

	propLevel := s.level
	if propLevel == 0 {
		propLevel = 1
	}
	s.assignLiteralByClause(literals[0], propLevel, -learnedIdx-5)
}

// backtrack backtracks (or backjumps) to a lower decision level
// Returns false if backtracking to level 0 (UNSAT)

// Backjumping vs Chronological Backtracking:
// Traditional DPLL backtracks one level at a time (chronological).
// CDCL solvers use backjumping (non-chronological backtracking) to skip
// irrelevant decision levels.

// How Backjumping Works (standard CDCL):
// 1. After 1-UIP conflict analysis, the learned clause has exactly one literal
//    at the current decision level (the UIP - Unique Implication Point)
// 2. The backjump level is the second-highest level in the learned clause
// 3. Instead of backtracking to level-1, we jump directly to backjumpLevel
// 4. At backjumpLevel, the learned clause is unit: all non-UIP literals are
//    false at levels <= backjumpLevel. propagateAssertingLiteral() assigns the
//    UIP at backjumpLevel. The decision at backjumpLevel is NOT flipped.

// Why Backjumping is Sound:
// The learned clause explains why the conflict occurred. All literals in the
// learned clause except the UIP are already false at levels < current.
// By backjumping to the second-highest level, the learned clause becomes unit
// and propagates the UIP literal, preventing the same conflict.

// Example:
// Decisions: x=1 (level 1), y=1 (level 2), z=1 (level 3)
// Conflict at level 3
// Learned clause: (¬x ∨ ¬y ∨ ¬z) with LBD=3 (levels 1,2,3)
// Backjump level = 2 (second-highest in learned clause)
// After backjump: trail = [x=1, y=1], z is unassigned
// The learned clause is now unit: ¬z is forced at level 2

// This skips exploring the entire subtree under (x=1, y=1, z=1) at level 3,
// which would all lead to the same conflict.
func (s *CDCLSolver) backtrack() bool {
	s.unitsDirty = true // Backtrack may unassign unit-propagated variables
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
	// Always backtrack at least one level below the conflict level.
	if bjLevel >= s.level {
		bjLevel = s.level - 1
	}
	if bjLevel < 0 {
		s.Log("c [BACKTRACK] bjLevel=%d invalid at level %d - returning UNSAT\n", bjLevel, s.level)
		return false
	}

	// Standard CDCL backjump: unassign everything at levels > bjLevel, KEEP the
	// decision and propagations at bjLevel. The asserting literal (UIP) is then
	// propagated at bjLevel by propagateAssertingLiteral().
	//
	// The previous implementation used trailHead[bjLevel] (start of level bjLevel)
	// as the decision point, which unassigned the decision at bjLevel and then
	// re-assigned it with the FLIPPED value. That is DPLL chronological backtracking:
	// it discards all propagations at bjLevel and explores the opposite branch,
	// defeating conflict-driven learning. Standard CDCL (MiniSat, Glucose) keeps
	// level bjLevel and lets the asserting literal propagate — that is what
	// cancelUntil() already does.
	var decisionPoint int
	if bjLevel+1 < len(s.trailHead) {
		decisionPoint = s.trailHead[bjLevel+1]
	} else {
		decisionPoint = len(s.trail)
	}

	// Clear all assignments above bjLevel.
	// No preprocessing check needed — preprocessing vars are on preprocessTrail (not s.trail).
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := s.trail[i]
		s.assignments[varIdx] = Assignment{Level: -1}
		s.litTrue[int(varIdx)*2] = false
		s.litTrue[int(varIdx)*2+1] = false
		s.implication[varIdx] = -1
		s.vsids.onUnassign(varIdx)
		s.numUnassigned++
	}
	s.trail = s.trail[:decisionPoint]
	s.qhead = decisionPoint
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel

	return true
}
