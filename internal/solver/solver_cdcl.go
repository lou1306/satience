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
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"
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
	DefaultRestartBase      = 200   // Base for Luby restart sequence (MiniSat-style)
	IterationReportInterval = 10000 // Report progress every N iterations

	// propsDecPhaseFlipBase is the nominal props/dec reference for the adaptive
	// phase-flip hysteresis (enable at 120% of this, disable at 80%). Decoupled
	// from the restart trigger's low limit so that only genuinely cascade-bound
	// instances flip phases; lowering the restart limit must not toggle flip on
	// for productive shallow instances.
	propsDecPhaseFlipBase = 100

	// maxAdaptiveRestartGear caps the multiplicative gear on the Luby fallback
	// base (see adaptiveRestartGear). Bounding ensures the fallback can never
	// grow so rare that the solver loses all periodic diversification — the
	// cap is a safety bound on the adaptive multiplier, not a per-instance
	// threshold.
	maxAdaptiveRestartGear = 16.0

	// restartPatienceConflicts is the uniform accumulated-conflict horizon that
	// must elapse before the adaptive gear may start deepening the Luby
	// fallback. Instances that solve within an ordinary conflict budget (all
	// suite instances solve below ~85K) never reach it and keep their tuned
	// frequent-restart schedule untouched; only long grinders, whose default
	// schedule has demonstrably failed to converge, are deepened. This is a
	// single uniform horizon — a "give the default schedule a fair chance"
	// rule — not a per-family or per-instance threshold.
	restartPatienceConflicts = 120000

	// Debugging thresholds.
	DebugConflictLimit = 100 // Verbose debug output for first N conflicts

	// maxCheckPerRound caps the number of clauses examined per inprocessing round
	// (vivification and learned subsumption), bounding per-call cost.
	maxCheckPerRound = 2000

	// Watch.ClauseIdx bit encoding:
	//   Bit 31: 0 = original clause, 1 = learned clause (sign bit, so ClauseIdx < 0 = learned)
	//   Bit 30: myPos — which watch position (0 or 1) this watch occupies in the clause
	//   Bits 0-28: clause index (supports up to 33M clauses)
	// Packing myPos into ClauseIdx eliminates the clauseLits[0] != falseLit cache miss
	// that was the #1 hotspot in propagation (13-14% of CPU).
	watchLearnedBit uint32 = 0x80000000
	watchMyPosBit   uint32 = 0x40000000
	watchBinaryBit  uint32 = 0x20000000 // Set for size-2 clauses: enables binary fast path
	watchIdxMask    uint32 = 0x1FFFFFFF
	watchMyPosMask  uint32 = 0x9FFFFFFF // bits 0-28 + bit 31 (clears myPos + binary for identity comparison)
)

// litToBlit converts a Literal to a litTrue index for storage in Watch.Blit.
// litTrue index = varIdx*2 + negated, matching the litValue array layout.
// Storing the index directly eliminates bit manipulation in the fast path.
func litToBlit(lit cnf.Literal) uint32 {
	l := uint32(lit)
	return (l&0x7FFFFFFF)<<1 | (l >> 31)
}

// calculateMaxLearned scales the clause database limit with instance size.
// MiniSat-style: base limit proportional to variables, grows with conflicts.
// maxLearnedMult overrides the numVars multiplier (default 0 = use built-in 10).
func calculateMaxLearned(numVars uint32, numClauses int, maxLearnedMult float64) int {
	// Learned clause limit: max(numClauses/3, min(numVars*mult, 5000)).
	// - numClauses/3: MiniSat-style base, keeps database proportional to instance.
	// - min(numVars*mult, 5000): Floor for small instances (need room to learn),
	//   capped at 5000 to prevent excessive database size on large instances
	//   (e.g. 7807v → 5000 instead of 78070, which caused 100K+ clause databases
	//   and 6x slower propagation).
	if maxLearnedMult <= 0 {
		maxLearnedMult = 10.0
	}
	baseLimit := numClauses / 3

	varFloor := int(float64(numVars) * maxLearnedMult)
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
	// ===== HOT FIELDS (touched every propagation/decision) =====
	// First ~4 cache lines. Adding fields below this block does NOT shift these
	// cache lines. This prevents struct-layout sensitivity where adding a cold
	// field (e.g., vivify counters) shifts the cache lines of the hottest fields,
	// perturbing borderline instances (30eb4ef4 SAT ~12s ↔ TIMEOUT 30s — see
	// V1/V5 in AGENTS.md). Keep this block contiguous at the top of the struct.
	cnf                *cnf.CNF
	assignments        []Assignment
	trail              []uint32
	trailHead          []int
	trailPos           []int // trailPos[varIdx] = position in trail (-1 if not on trail); O(1) lookup for 1-UIP fallback
	level              int
	vsids              *VSIDS
	numUnassigned      int           // Count of unassigned variables (O(1) allAssigned/hasUnassigned)
	watchLists         [][]cnf.Watch // watchLists[lit] = clauses watching lit
	originalSearchHint []int32       // Per-original-clause search hint for replacement scan (0=no hint)
	qhead              int           // Watched literals: next trail index to process
	litTrue            []bool        // Cached assigned-and-true bitmap (varIdx*2 + negated); blit fast path reads this instead of decoding literal + loading assignments[]

	// ===== WARM FIELDS (per-conflict / per-restart) =====
	preprocessTrail   []uint32 // Permanent preprocessing assignments (Level 0, never cleared/backtracked)
	probeTrail        []int    // Reusable trail for FLP non-watch BCP (literal indices)
	conflicts         int
	iterations        int
	propagations      int // Total propagations (assignments by unit propagation)
	maxIter           int
	decisions         int
	uipFallbackCount  int // Number of times 1-UIP resolution didn't converge (diagnostic)
	uipFallbackLogged int // Counter for capping per-solve fallback diagnostic output (diagnostic)
	backjumpLevel     int
	maxLearned        int
	restartCount      int
	lubyIndex         int
	// Restart-reason attribution counters (instrumentation). Which mechanism
	// triggered each restart: Luby fallback, Glucose LBD restarts, props/dec
	// bounded, level-capped, or MiniSat geometric. Used to diagnose where
	// search time goes (flat-high-LBD instances never fire Glucose, so the
	// Luby fallback dominates).
	restartReasons        [5]int // [0]=glucose, [1]=luby, [2]=propsdec, [3]=levelcap, [4]=geometric
	restartSegStartConf   int    // conflicts at the start of the current restart segment
	restartSegStartDec    int    // decisions at the start of the current restart segment
	restartSegStartProps  int    // propagations at the start of the current restart segment
	restartSegProdRatio   float64 // conflicts per 1000 decisions over the last segment (negative=unset)
	restartSegGlueCount   int    // glue clauses learned in the current segment
	restartSegStartGlue   int    // glueLearned at the start of the current segment
	// Branch-quality telemetry (instrumentation): which VSIDS bump scheme and
	// init mode were actually in effect. 1=analyze_toclear (bump all touched),
	// 0=bumpClause only; initMode 1=clause/occurrence-weighted, 0=zero-init.
	branchBumpScheme int
	branchInitMode   int
	// Adaptive restart gear: multiplier on the Luby fallback base. Starts at
	// 1.0 (exact baseline). Raised to maxAdaptiveRestartGear by restart() when
	// the search has ground past restartPatienceConflicts conflicts with no
	// Glucose ever firing and no glue learned — the flat-LBD grinder signature
	// (rphp). Decayed back to 1.0 whenever Glucose/glue activity appears, since
	// healthy instances need their frequent diversification preserved.
	adaptiveRestartGear      float64
	restartSegTotalConflicts uint64 // accumulated conflicts across segments
	restartSegSteps          int    // number of restart segments observed
	lbdSum            int
	lbdCount          int
	emaLBD            float64 // Exponential moving average of LBD (smooth restart signal)
	// B2: Cumulative LBD accumulator (NOT reset on restart, unlike lbdSum/lbdCount).
	// Used to detect consistently high-LBD instances and shrink the clause DB.
	totalLbdSum   uint64
	totalLbdCount uint64
	// B1: Level-capped restart for long-clause instances. Long-clause instances
	// produce high-LBD clauses consistently, so the Glucose EMA criterion never
	// fires (EMA ≈ avg). Deep search produces high-LBD clauses with no propagation
	// guidance, creating a deep-search → high-LBD → no-guidance → deep-search cycle.
	// This breaks the cycle by restarting when conflict level exceeds the cap.
	// Gated on LongClauseRatio > 0.8 (binary-heavy instances have the props/dec restart).
	lastConflictLevel    int     // Level at which the last conflict occurred (captured in handleConflict)
	longClauseRatio      float64 // Cached LongClauseRatio from classifier (for B1 gate)
	randomSeed           uint64  // Seed for deterministic random selection
	rndInitNoise         float64 // Magnitude of random noise added to initial VSIDS activity (0=disabled)
	unitLearnedList      []int   // List of learned clause indices that are unit clauses (for O(1) propagation)
	unitsDirty           bool    // True when unit scan needs to run (new unit learned or backtrack occurred)
	randomPhaseRate      float64 // Probability of flipping the saved phase per decision (0=disabled)
	restartPhaseFlipRate float64 // Probability of flipping each saved phase on restart (0=disabled)
	lbdScaleOverride     bool    // True if user explicitly set LBD scale via CLI (skip adaptive)
	inVivification       bool    // True during vivification trial propagation (suppresses false UNSAT from unit scan)
	emptyClauseFound     bool    // Set when empty learned clause derived (UNSAT)
	compactPending       bool    // Set when learned-clause tombstone ratio is high; compaction runs at the next restart (level 0)
	lastLearnedClauseIdx int     // Index of most recently learned clause (-1 = none); for asserting literal propagation
	watchInitialized     bool    // True if watches have been initialized
	originalUnitClauses  []int   // Precomputed indices of original unit clauses (for restart re-propagation)

	// Memory pool for learned clauses - contiguous literal storage to eliminate per-clause allocations
	learnedLiterals    []cnf.Literal        // All learned clause literals in one contiguous slice
	learnedLoc         []LearnedClauseLoc   // Packed (Offset, Size) per learned clause; Size=0 means tombstone
	learnedMetadata    []cnf.ClauseMetadata // Per-clause metadata (LBD, Activity) — cold path only (deletion/rescale)
	learnedSearchHint  []int32              // Per-learned-clause search hint for replacement scan (0=no hint) — hot path
	learnedWatchIdx0   []int                // First watched literal index (for fast watch removal)
	learnedWatchIdx1   []int                // Second watched literal index (for fast watch removal)
	learnedActiveCount int                  // Number of active clauses (excludes tombstones)
	learnedCapacity    int                  // Total capacity including tombstones

	// Binary implication graph (BIG), CSR layout: bigAdjData holds the flat
	// successor lists and bigAdjOff[litIdx..litIdx+1] delimits litIdx's slice
	// (bigAdjOff has len numLits+1). bigAdjOff==nil signals "not built".
	// Successors m satisfy binary clause (¬litIdx ∨ m) (i.e., litIdx → m in the
	// implication graph). Built once from original binary clauses in buildBIG.
	// Used for transitive BIG-based clause minimization (see bigReachableInClause).
	bigAdjData []int32
	bigAdjOff  []int32
	// BIG BFS state for transitive clause minimization. Per-literal epoch stamps
	// avoid re-zeroing the visited array on each minimization call (epoch just
	// increments). The queue is reused across calls (sliced to [:0]).
	bigBfsVisited []uint16
	bigBfsEpoch   uint16
	bigBfsQueue   []int

	// Reusable buffers for conflict analysis (avoid per-conflict allocation)
	tmpLiteralInClause  []bool
	tmpSeenVar          []bool // Pre-allocated bitset for duplicate/tautology checks (replaces per-conflict maps)
	tmpLiteralIsNegated []bool
	tmpLevelCount       []int
	tmpLevelCountUsed   []bool // Track which levels have non-zero tmpLevelCount
	tmpCandidates       []resolveCandidate
	tmpLevelSet         []int         // For LBD calculation (replaces map)
	tmpLevelSetUsed     []bool        // Track which levels are in tmpLevelSet
	tmpResolved         []bool        // Track resolved variables in 1-UIP to prevent re-resolution cycles
	tmpResolvedVars     []uint32      // Track which variables were resolved (for fast reset)
	tmpTouchedVars      []uint32      // Track which variables were modified (for fast reset)
	tmpLearnedLits      []cnf.Literal // Reusable buffer for learned clause literals
	tmpMinSeenVars      []uint32      // Vars marked in tmpSeenVar during minimization (for fast cleanup)
	conflictClauseBuf   cnf.Clause    // Pre-allocated conflict clause (avoids per-conflict heap alloc)
	conflictLitsBuf     []cnf.Literal // Pre-allocated buffer for conflict clause literal copies

	// Reusable buffer for vivification results (avoid per-round allocation)
	tmpVivifyResults []vivifyResult

	// Reusable buffers for learned-clause subsumption (avoid per-round allocation)
	tmpLearnedSubOcc     [][]int             // occurrence lists: occ[litIdx] = learned clause indices
	tmpLearnedSubSeen    []bool              // literal mark for subsumption checking
	tmpLearnedSubTouched []int               // marked literals (for fast clear of tmpLearnedSubSeen)
	tmpLearnedSubResults []subsumptionResult // subsumption/strengthening results

	// Reusable buffers for clause deletion (avoid per-deletion allocation)
	tmpDeleted            []bool // Bitmap for deleted clauses
	tmpClauseUsedAsReason []bool // Track clauses used as implications
	tmpClauseIndexMap     []int  // Pre-allocated buffer for old->new clause index mapping
	tmpDeletionCandidates []int  // Pre-allocated buffer for deletion candidate sorting

	// Equivalence detection (SCC-based): stores mapping for model reconstruction.
	equivRep        []uint32        // Representative variable for each variable (identity if not merged)
	equivNeg        []bool          // Whether variable is equivalent to negation of its representative
	hasEquivalences bool            // True if detectEquivalences found and merged any equivalences
	eliminatedVars  []eliminatedVar // Variables eliminated by BVE (for model reconstruction)

	// ===== COLD CONFIG (set once at startup, rarely touched in hot path) =====
	// Clause-activity deletion (VSIDS-style decayed activity for within-tier
	// deletion ordering). When claActivityEnabled=false, all Activity fields
	// stay 0 and the sort's index tiebreak reduces to pure FIFO (no behavior
	// change vs pre-activity code).
	claInc             float64 // Activity increment (grows via O(1) decay), starts 1.0
	claDecayFactor     float64 // 0.99 (slower than MiniSat 0.95 to preserve activity longer)
	claActivityEnabled bool    // false = pure FIFO within LBD tiers
	maxLearnedShrunk   bool    // True if maxLearned has been shrunk (one-way, no grow-back)
	skipClassify       bool    // Skip classifyInstance (keep CLI defaults for tuning)
	explicitFlags      map[string]bool
	decayFloor         float64 // Random-like mixed t=0 initial decay (default 0.50)
	decayCeil          float64 // Random-like mixed t=0 max decay (default 0.80)
	// Clause DB deletion thresholds (Tier 1 tunables). These control which
	// learned clauses are deleted and when. Sweeping them matters because the
	// level-0 literal filter changed clause DB composition.
	lbdTier1Threshold          int     // Pass 1 deletion: delete LBD > threshold (default 5)
	lbdTier2Threshold          int     // Pass 2 deletion: delete LBD > threshold (default 2; glue ≤ threshold never deleted)
	dbGrowthDivisor            int     // dynamicLimit = maxLearned + conflicts/divisor (default 50)
	deletionTriggerRatio       float64 // Trigger deletion when activeCount > ratio × dynamicLimit (default 1.5)
	dbShrinkThreshold          int     // Shrink maxLearned when avgLBD > threshold (default 10)
	dbShrinkFloorMultiplier    int     // Shrink floor = numVars × multiplier (default 3)
	restartBase                int     // Luby restart base (default 200; classifier may override)
	lubyThresholdCap           int     // Max Luby threshold before resetting lubyIndex to 0 (prevents Luby exhaustion)
	restartPropsDecLimit       int     // Props/dec threshold for restart (0=disabled, default 100)
	adaptPropDecLimit          int     // Tier-2 low props/dec threshold for deep-search escape restart
	adaptPropDecDeepGate       int     // Tier-2 gate: deep-search escape fires when conflict level exceeds this (0=disabled)
	adaptivePhaseFlipRate      float64 // Phase flip rate when props/dec is high (0=disabled, default 0.1)
	propsDecRestartGap         int     // Min conflicts between props/dec-bounded restarts (default 100)
	restartLevelCap            int     // Force restart when conflict level exceeds this on long-clause instances (0=disabled)
	levelRestartGap            int     // Min conflicts between level-capped restarts (anti-thrashing)
	minimizeMaxDepth           int     // Max recursion depth for recursive clause minimization (default 0=unlimited)
	unitPropBudget             int     // Max literal visits for unit propagation preprocess (0=unlimited)
	veBudget                   int     // Max resolvents for variable elimination (0=unlimited)
	subsumptionBudget          int     // Max clause-pair comparisons in subsumptionPass (0=unlimited)
	vivifyPeriod               int     // Run vivification every Nth restart (0=disabled, default 50)
	vivifyMinConflictGap       int     // Min conflicts between vivify rounds (default 5000)
	conflictsAtLastVivify      int     // conflict count at last vivify round (for gap gate)
	vivifyEnabled              bool    // Whether vivification is enabled (adaptive: structured instances only)
	subsumptionPeriod          int     // Run subsumption every Nth restart (0=disabled, default 100)
	subsumptionPeriodSet       bool    // True if SetSubsumptionPeriod was called (skip adaptive override)
	subsumptionMinConflictGap  int     // Min conflicts between subsumption rounds (0=restart-based only)
	conflictsAtLastSubsumption int     // conflict count at last subsumption round (for gap gate)
	skipSubsumption            bool
	skipBVE                    bool
	skipPolarityPhase          bool
	occurrenceWeight           float64
	skipVSIDSInit              bool
	minisatRestart             bool
	geometricRestartThreshold  float64 // Cached threshold for minisatRestart (= restartBase × 1.5^lubyIndex)
	lazyInit                   *lazyInitState
	// Cached classifier output (set in getAdaptivePreprocessingConfig). Used by
	// initVSIDSOccurrenceBonus to gate the polarity-based initial phase: the
	// occurrence-based phase is trajectory-sensitive and helps some instances
	// while hurting others with near-identical structure, so the classifier
	// score alone cannot predict benefit. The PolarityImbalance metric (mean
	// per-variable |pos-neg|/(pos+neg)) is stored as a secondary signal.
	structureScore             float64 // Cached StructuredScore from analyzeInstanceStructure
	polarityImbalance          float64 // Mean per-variable polarity imbalance (0=balanced, 1=pure)
	maxLearnedClauseSize       int
	preprocessingMaxVars       int     // Skip preprocessing if > N vars (default 50000)
	preprocessingMaxClauses    int     // Skip preprocessing if > N clauses (default 500000)
	restartGlucoseRatio        float64 // Glucose-style restart when LBD > ratio × avg (default 1.5)
	restartGlucoseMinConflicts int     // Min conflicts before Glucose restarts kick in (default 50)
	glucoseGap                 int     // Min conflicts between Glucose restarts (anti-thrashing, default 100)

	// ===== DEBUG/STATS (only touched in verbose or -stats paths) =====
	verbose       bool
	statsInterval int   // Print stats every N conflicts (0=disabled, bypasses verbose gate)
	solveStartNs  int64 // Wall-clock start (UnixNano) of the public Solve entry; for elapsed in stats
	// Diagnostic counters for learned-clause minimization (always-on; reported in
	// printStats and a periodic solve-loop log). Pure instrumentation — no behavior.
	minimizeCalls                  uint64 // recursive self-subsumption invocations (clauses >2 lits)
	minimizeLiteralsIn             uint64 // literals seen by minimizer across all calls
	minimizeLiteralsOut            uint64 // literals remaining after minimization across all calls
	bigMinimizeCalls               uint64 // BIG minimization attempts
	bigMinimizeHits                uint64 // BIG minimization successes
	vivifyRoundsRun                uint64 // vivification rounds actually executed
	vivifyClausesChecked           uint64 // clauses passed to vivifyClause
	vivifyClausesModified          uint64 // clauses shortened by vivify
	vivifyLiteralsRemoved          uint64 // literals removed by vivify
	subsumptionRoundsRun           uint64 // subsumption rounds actually executed
	subsumptionClausesChecked      uint64 // non-binary learned clauses scanned
	subsumptionClausesSubsumed     uint64 // clauses deleted by forward subsumption
	subsumptionClausesStrengthened uint64 // literals removed by self-subsumption
	// Histogram of final learned-clause sizes (post-minimization, at learn time).
	// Buckets: [<=2, 3-5, 6-10, 11-20, 21-50, >50].
	learnedLenHist [6]uint64

	// Cached classifier output (set once in classifyInstance, read occasionally).
	// Placed at struct end to avoid shifting hot/warm cache lines (op_15 regression).
	binaryRatio    float64 // Cached BinaryRatio from classifier
	useBumpAnalyze bool    // True: bump all touched vars (minisat analyze_toclear); false: bump conflict clause only
	// True when the user explicitly set useBumpAnalyze via CLI (-ms-analyze),
	// so classifyInstance should not overwrite it.
	useBumpAnalyzeOverride bool

	// Runtime decay adaptation (periodic re-check with rolling windows).
	// glueLearned counts learned clauses with LBD ≤ 2 (glue clauses) since
	// search start; decayAdapted gates the one-way override (once fired, stays).
	// adaptNextConflict is the next conflict count at which to re-evaluate.
	// *AtLastAdapt snapshots cumulative counters at the previous check for
	// rolling-window delta computation. See maybeAdaptDecay for the rationale.
	// Placed at struct end with other cold fields to avoid shifting hot/warm
	// cache lines (69d72f81 regressed from 9s to TMO when these were mid-struct).
	glueLearned       uint64
	decayAdapted      bool
	adaptNextConflict int
	glueAtLastAdapt   uint64
	lbdSumAtLastAdapt uint64
	countAtLastAdapt  uint64

	// Unified search governor: runtime self-correction compensating for
	// classifier misclassification WITHOUT static per-instance gates. Evaluated
	// at restart boundaries (cold path) using rolling-window deltas of live
	// search signals. See governor.go / maybeAdaptSearch.
	govWindows      int    // number of governor windows evaluated
	govStartConf    uint64 // cumulative conflicts at last window snapshot
	govStartDec     uint64 // cumulative decisions at last window snapshot
	govStartProps   uint64 // cumulative propagations at last window snapshot
	govStartGlue    uint64 // cumulative glue at last snapshot
	govStartLbd     uint64 // cumulative LBD sum at last snapshot
	govNextConflict int    // next conflict count at which to evaluate the governor
	govGlucoseOn    bool   // Detector 3 fired: reactive Glucose enabled
	govGearRaised   bool   // Detector 1 fired: restart base already lowered

	// Governor tuning knobs (CLI-exposed for sweeps; defaults match the
	// empirically-tuned governor). See governor.go.
	govGrindBase  int     // Det1: target restartBase when cascade grind fires (default 5)
	govGrindPDec  float64 // Det1: props/dec threshold to qualify as cascade grind (default 120)
	govGrindConf  int     // Det1: min conflicts before Det1 may fire (default 30000)
	govWanderDecC float64 // Det3: min decisions/conflict to qualify as wander (default 40)
	govWanderGlue float64 // Det3: max glue ratio to qualify as wander (default 0.10)
	govWindow     int     // window scope in conflicts per governor eval (default 20000)
}

// lazyInitState holds lazy init detection state. Heap-allocated only when
// -lazy-init is enabled, keeping CDCLSolver's hot cache lines unchanged.
type lazyInitState struct {
	done            bool
	emaLBDSnapshots []float64
	avgLBDThreshold float64
	glueRateLimit   float64
	minConflicts    int
	trendWindow     int
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
	maxLearned := calculateMaxLearned(formula.NumVars, formula.NumClauses, 0)
	restartBase := DefaultRestartBase

	// Ensure literal pool is built for efficient propagation
	formula.RebuildLiteralPool()

	solver := &CDCLSolver{
		cnf:                  formula,
		assignments:          make([]Assignment, formula.NumVars),
		trail:                make([]uint32, 0, formula.NumVars),
		preprocessTrail:      make([]uint32, 0, formula.NumVars),
		trailHead:            make([]int, 1),
		trailPos:             make([]int, formula.NumVars),
		qhead:                0,
		lastLearnedClauseIdx: -1,
		level:                0,
		vsids:                NewVSIDS(formula.NumVars),
		conflicts:            0,
		iterations:           0,
		maxIter:              0,
		// P0: Pre-allocate learned clause arrays with generous capacity to avoid growth
		// learnedLiterals: 8 literals per clause average (covers most learned clauses)
		learnedLiterals:    make([]cnf.Literal, 0, maxLearned*8),
		learnedLoc:         make([]LearnedClauseLoc, 0, maxLearned),
		learnedMetadata:    make([]cnf.ClauseMetadata, 0, maxLearned), // Packed metadata
		learnedSearchHint:  make([]int32, 0, maxLearned),              // Hot-path search hints
		learnedWatchIdx0:   make([]int, 0, maxLearned),                // Watched literal indices
		learnedWatchIdx1:   make([]int, 0, maxLearned),
		learnedActiveCount: 0,
		learnedCapacity:    0,
		unitLearnedList:    make([]int, 0, 64), // Pre-allocate for unit clause tracking
		verbose:            false,
		decisions:          0,
		backjumpLevel:      0,
		maxLearned:         maxLearned,
		restartBase:        restartBase,
		restartCount:       0,
		adaptiveRestartGear: 1.0,
		lubyIndex:          0,
		lubyThresholdCap:   0, // Disabled by default; enabled for structured instances in classifyInstance
		lbdSum:             0,
		lbdCount:           0,
		// Clause-activity deletion: VSIDS-style decayed activity for within-tier
		// deletion ordering. Defaults enable activity (can be disabled via CLI).
		claInc:             1.0,
		claDecayFactor:     0.99,
		claActivityEnabled: true,
		randomSeed:         0,
		// P1: Pre-allocate reusable buffers with generous capacity to avoid reallocation
		tmpLiteralInClause:  make([]bool, formula.NumVars),
		tmpSeenVar:          make([]bool, formula.NumVars),
		tmpLiteralIsNegated: make([]bool, formula.NumVars),
		bigBfsVisited:       make([]uint16, int(formula.NumVars)*2),
		bigBfsQueue:         make([]int, 0, 256),
		tmpLevelCount:       make([]int, formula.NumVars+1),
		tmpLevelCountUsed:   make([]bool, formula.NumVars+1),
		tmpCandidates:       make([]resolveCandidate, 0, 200), // Increased from 100
		tmpLevelSet:         make([]int, 0, formula.NumVars),
		tmpLevelSetUsed:     make([]bool, formula.NumVars+1),
		tmpResolved:         make([]bool, formula.NumVars),
		tmpResolvedVars:     make([]uint32, 0, formula.NumVars),
		tmpTouchedVars:      make([]uint32, 0, formula.NumVars),
		// P1: Increased buffer capacity from 64 to 256 to handle larger learned clauses
		tmpLearnedLits:   make([]cnf.Literal, 0, 256),
		tmpMinSeenVars:   make([]uint32, 0, 256),
		conflictLitsBuf:  make([]cnf.Literal, 0, 256),
		tmpVivifyResults: make([]vivifyResult, 0, 64),
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
		// Subsumption budget: 0 = unlimited. Sized adaptively before preprocessing
		// (in preprocessAggressive) to bound the O(n²)-ish clause-pair scans.
		subsumptionBudget: 0,
		// Vivification: run every 50 restarts (configurable via CLI)
		vivifyPeriod:  50,
		vivifyEnabled: true,
		// Min conflicts between vivify rounds. Without this gate, small/fast-restart
		// instances fire vivify every ~50 restarts = every few hundred conflicts,
		// thrashing the solver with low-yield rounds. 20000 ensures vivify only
		// fires on genuinely hard instances (those exceeding ~20K conflicts); easy
		// instances solve before vivify ever triggers.
		vivifyMinConflictGap: 20000,
		// Learned-clause subsumption: same gating cadence as vivification.
		// Self-gating via early-return on no binary learned clauses means it
		// is effectively free on random instances, so it is always enabled.
		subsumptionPeriod:         100,
		subsumptionMinConflictGap: 20000,
		// Runtime decay adaptation: first check after 500-conflict warmup.
		adaptNextConflict:    500,
		govNextConflict:      500,
		randomPhaseRate:      0,
		restartPhaseFlipRate: 0,
		// Configurable parameters with defaults
		preprocessingMaxVars:    50000,
		preprocessingMaxClauses: 500000,
		// Governor tuning knobs (CLI-exposed).
		govGrindBase:  5,
		govGrindPDec:  120.0,
		govGrindConf:  30000,
		govWanderDecC: 40.0,
		govWanderGlue: 0.10,
		govWindow:     20000,
		// Restart policy defaults (aggressive Glucose-style for better performance)
		restartGlucoseRatio:        1.5, // Standard Glucose value (aggressive restarts)
		restartGlucoseMinConflicts: 50,  // Start Glucose restarts early
		glucoseGap:                 100, // Min 100 conflicts between Glucose restarts
		restartPropsDecLimit:       100, // Tier 1: restart when props/dec > 100 (cascade-bound)
		adaptPropDecLimit:          20,  // Tier 2: low props/dec deep-search escape threshold
		adaptPropDecDeepGate:       85,  // Tier 2: escape fires only above this conflict level
		adaptivePhaseFlipRate:      0.1, // Flip 10% of phases when props/dec is high
		propsDecRestartGap:         100, // Min 100 conflicts between props/dec restarts
		restartLevelCap:            40,  // Force restart when conflict level > 40 on long-clause instances
		levelRestartGap:            100, // Min 100 conflicts between level-capped restarts
		litTrue:                    make([]bool, int(formula.NumVars)*2),
		decayFloor:                 0.50,
		decayCeil:                  0.80,
		lbdTier1Threshold:          5,
		lbdTier2Threshold:          2,
		dbGrowthDivisor:            50,
		deletionTriggerRatio:       1.5,
		dbShrinkThreshold:          10,
		dbShrinkFloorMultiplier:    3,
		occurrenceWeight:           0.5,
	}

	// Initialize all assignments as unassigned (Level=-1, Reason=-1) with default
	// positive phase (SavedPhase=true). make zero-fills to 0, which would look like
	// "original clause 0" for Reason — explicitly set to -1 (decision/no reason).
	for i := range solver.assignments {
		solver.assignments[i] = Assignment{Level: -1, Reason: -1, SavedPhase: true}
	}

	// Enable LBD-based VSIDS for better variable selection
	// Variables in low-LBD clauses get higher priority
	solver.vsids.SetUseLBD(true)

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

func (s *CDCLSolver) SetAdaptPropDecDeepGate(limit, gate int) {
	s.adaptPropDecLimit = limit
	s.adaptPropDecDeepGate = gate
}

// SetGovernorParams overrides the runtime search-governor tuning knobs
// (Detector 1 cascade-grind and Detector 3 wander). Used for parameter sweeps.
func (s *CDCLSolver) SetGovernorParams(grindBase int, grindPDec float64, grindConf int, wanderDecC, wanderGlue float64, window int) {
	if grindBase >= 1 {
		s.govGrindBase = grindBase
	}
	if grindPDec > 0 {
		s.govGrindPDec = grindPDec
	}
	if grindConf > 0 {
		s.govGrindConf = grindConf
	}
	if wanderDecC > 0 {
		s.govWanderDecC = wanderDecC
	}
	if wanderGlue >= 0 {
		s.govWanderGlue = wanderGlue
	}
	if window > 0 {
		s.govWindow = window
	}
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

// SetClaDecay sets the clause-activity decay factor (0 < d < 1). Lower = faster
// decay (shorter memory). Only meaningful when claActivityEnabled=true.
func (s *CDCLSolver) SetClaDecay(d float64) {
	if d > 0 && d < 1.0 {
		s.claDecayFactor = d
	}
}

// SetClaActivityEnabled toggles activity-based clause deletion. When false, all
// Activity fields stay 0 and the within-tier sort's index tiebreak reduces to
// pure FIFO (identical to pre-activity behavior).
func (s *CDCLSolver) SetClaActivityEnabled(enabled bool) {
	s.claActivityEnabled = enabled
}

// SetSkipClassify disables instance classification, keeping CLI defaults for
// all search parameters. Used for tuning: tests whether the classifier's
// parameter overrides (tuned for the previous propLevel=1 behavior) are
// harmful under the corrected Level-0 root propagation.
func (s *CDCLSolver) SetSkipClassify(skip bool) {
	s.skipClassify = skip
}

// SetExplicitFlags records which CLI flags were explicitly set by the user.
// classifyInstance respects user-set flags: it skips clobbering any parameter
// whose flag appears in this set. Without this, the classifier's per-category
// overrides would unconditionally overwrite CLI values, making most search
// flags no-ops for the categories where tuning matters most (Cat B decay and
// Glucose, Cat A/B/C-binary-heavy restartBase). The map keys are the CLI flag
// names (e.g. "restart-base", "initial-decay").
func (s *CDCLSolver) SetExplicitFlags(flags map[string]bool) {
	s.explicitFlags = flags
}

// flagSet reports whether the user explicitly passed the named CLI flag.
// Returns false if SetExplicitFlags was never called (no flags recorded).
func (s *CDCLSolver) flagSet(name string) bool {
	return s.explicitFlags != nil && s.explicitFlags[name]
}

// SetDecayFloorCeil sets the random-like mixed decay floor and ceiling. At
// t=0 (pure aggressive), initialDecay=floor, maxDecay=ceil. At t=1
// (near-default), both interpolate to 0.95. Defaults: floor=0.50, ceil=0.80.
func (s *CDCLSolver) SetDecayFloorCeil(floor, ceil float64) {
	s.decayFloor = floor
	s.decayCeil = ceil
}

// SetClauseDBParams sets the clause DB deletion thresholds. All default to
// the pre-tuning hardcoded values.
func (s *CDCLSolver) SetClauseDBParams(lbdTier1, lbdTier2, dbGrowthDiv int, delTriggerRatio float64, dbShrinkThresh, dbShrinkFloorMult int) {
	s.lbdTier1Threshold = lbdTier1
	s.lbdTier2Threshold = lbdTier2
	s.dbGrowthDivisor = dbGrowthDiv
	s.deletionTriggerRatio = delTriggerRatio
	s.dbShrinkThreshold = dbShrinkThresh
	s.dbShrinkFloorMultiplier = dbShrinkFloorMult
}

func (s *CDCLSolver) SetOccurrenceWeight(w float64) {
	s.occurrenceWeight = w
}

func (s *CDCLSolver) SetSkipVSIDSInit(skip bool) {
	s.skipVSIDSInit = skip
}

func (s *CDCLSolver) SetMinisatRestart(enabled bool) {
	s.minisatRestart = enabled
}

func (s *CDCLSolver) SetMinisatBumps(enabled bool) {
	s.vsids.SetMinisatBumps(enabled)
}

// SetUseBumpAnalyze forces updateScores-style analyze_toclear behavior:
// bump all variables touched during 1-UIP analysis (matching MiniSat) instead
// of bumping only the conflict clause's variables. Pure diagnostic toggle; when
// true it overrides the classifier's useBumpAnalyze decision for A/B testing.
func (s *CDCLSolver) SetUseBumpAnalyze(enabled bool) {
	s.useBumpAnalyzeOverride = true
	s.useBumpAnalyze = enabled
}

func (s *CDCLSolver) SetNoLBDBonus(enabled bool) {
	s.vsids.SetUseLBD(!enabled)
}

func (s *CDCLSolver) SetLazyInit(enabled bool) {
	if enabled {
		s.lazyInit = &lazyInitState{
			avgLBDThreshold: 12.0,
			glueRateLimit:   0.05,
			minConflicts:    50,
			trendWindow:     3,
		}
	}
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
	s.subsumptionPeriodSet = true
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
	// (learnedLoc 8B + learnedMetadata 16B + learnedWatchIdx0/1 8B each = 48B) × capacity
	cap := cap(s.learnedLoc)
	poolMemoryKB = (len(s.learnedLiterals)*4 + cap*48) / 1024
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
//
//	>=0  → original clause at that index
//	<=-5 → learned clause (index = -impl-5)
//	-1   → decision (no reason)
//	-2/-3/-4 → preprocessing sentinels (no resolvable reason clause)
func (s *CDCLSolver) getReasonLitsForVar(v uint32) []cnf.Literal {
	reasonClauseIdx := s.assignments[v].Reason
	if reasonClauseIdx >= 0 {
		clauseLocs := s.cnf.GetOriginalClauseLocs()
		if int(reasonClauseIdx) < len(clauseLocs) {
			loc := clauseLocs[reasonClauseIdx]
			if loc.Size > 0 {
				pool := s.cnf.GetLiteralPool()
				return pool[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
			}
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
	s.Log("c Restarts:      %s (last segment %d conf/1kdec, %d glue)\n",
		s.restartReasonSummary(), int(s.restartSegProdRatio), s.restartSegGlueCount)
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
	fmt.Fprintf(os.Stderr, "c [final] t=%.2fs conflicts=%d decisions=%d props=%d props/dec=%.1f learned=%d/%d emaLBD=%.1f avgLBD=%.1f totAvgLBD=%.1f%s | br=bump=%d/init=%d | min: rate=%.1f%% | vivify: rounds=%d | subsump: rounds=%d sub=%d str=%d | uip-fallback=%d | hist=[%d %d %d %d %d %d]\n",
		s.elapsedSec(), s.conflicts, s.decisions, s.propagations, propsPerDec,
		s.learnedActiveCount, s.maxLearned, s.emaLBD, avgLBD, totalAvgLBD, shrunk,
		s.branchBumpScheme, s.branchInitMode,
		minRate,
		s.vivifyRoundsRun,
		s.subsumptionRoundsRun, s.subsumptionClausesSubsumed, s.subsumptionClausesStrengthened,
		s.uipFallbackCount,
		s.learnedLenHist[0], s.learnedLenHist[1], s.learnedLenHist[2],
		s.learnedLenHist[3], s.learnedLenHist[4], s.learnedLenHist[5])
}

// restartReasonSummary returns a descriptive string of restart attribution.
// Instrumentation only — no effect on search behavior.
func (s *CDCLSolver) restartReasonSummary() string {
	total := s.restartReasons[0] + s.restartReasons[1] + s.restartReasons[2] +
		s.restartReasons[3] + s.restartReasons[4]
	if total == 0 {
		return "no restarts"
	}
	names := []string{"glucose", "luby", "propsdec", "levelcap", "geometric"}
	var sb strings.Builder
	for i, n := range s.restartReasons {
		if n > 0 {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(names[i])
			sb.WriteString("=")
			sb.WriteString(strconv.Itoa(n))
		}
	}
	return sb.String()
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
	glueRatio := 0.0
	if s.totalLbdCount > 0 {
		glueRatio = float64(s.glueLearned) / float64(s.totalLbdCount)
	}
	fmt.Fprintf(os.Stderr, "c [stats] t=%.2fs conflicts=%d level=%d decisions=%d props=%d props/dec=%.1f learned=%d emaLBD=%.1f avgLBD=%.1f totAvgLBD=%.1f glue=%.3f%s | br=bump=%d/init=%d | min: rate=%.1f%% | BIG: hits=%d/%d | restarts[%s] seg=%.0f/1kdec glue=%d\n",
		s.elapsedSec(), s.conflicts, s.level, s.decisions, s.propagations, propsPerDec,
		s.learnedActiveCount, s.emaLBD, avgLBD, totalAvgLBD, glueRatio, shrunk,
		s.branchBumpScheme, s.branchInitMode,
		minRate,
		s.bigMinimizeHits, s.bigMinimizeCalls,
		s.restartReasonSummary(), s.restartSegProdRatio, s.restartSegGlueCount)
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
	Density           float64 // clauses / vars
	BinaryRatio       float64 // binary clauses / total
	TernaryRatio      float64 // 3-literal clauses / total
	LongClauseRatio   float64 // clauses with >3 literals / total
	SmallClauseRatio  float64 // (binary + ternary) / total
	StructuredScore   float64 // 0.0 = random, 1.0 = highly structured
	PolarityImbalance float64 // mean per-variable |pos-neg|/(pos+neg); 0=balanced, 1=pure
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

	// Track whether long clauses have varied sizes. Random k-SAT (k≥4) has
	// all long clauses the same size; structured instances have varied sizes.
	// Used to penalize longScore for random k-SAT misclassified as structured.
	firstLongSize := 0
	longSizeVaried := false

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
			if firstLongSize == 0 {
				firstLongSize = size
			} else if size != firstLongSize {
				longSizeVaried = true
			}
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
	// Penalize uniform-size long clauses: random k-SAT (k≥4) has all long
	// clauses the same size, which is not "structured". Without this, rand4sat
	// (density 9.8, 100% long, all size 4) scores 0.80 and gets the structured
	// path (restartBase=200, no aggressive decay) — but it needs the random
	// path. Penalizing to 0.3 drops the score to ~0.38, below the 0.70
	// threshold. Structured instances (e.g. 274099073, density 15.6, 99.4%
	// long with VARIED sizes) keep full longScore.
	if longCount > 0 && !longSizeVaried {
		longScore *= 0.3
	}
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
	} else if structure.BinaryRatio > 0 && structure.LongClauseRatio > 0 {
		// Binary + long (no ternary) - also structured (e.g., rphp family)
		mixedSizeScore = 0.7
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

	// When minisatRestart is enabled, skip all classifier restart tuning —
	// use MiniSat's fixed geometric base=100 for all instances.
	if s.minisatRestart {
		s.structureScore = structure.StructuredScore
		s.polarityImbalance = structure.PolarityImbalance
		s.longClauseRatio = structure.LongClauseRatio
		s.binaryRatio = structure.BinaryRatio
		s.restartBase = 100
		return
	}

	// Cache classifier output for downstream consumers (initVSIDSOccurrenceBonus
	// gates the polarity-based initial phase on these metrics; the level-capped
	// restart gate reads longClauseRatio).
	s.structureScore = structure.StructuredScore
	s.polarityImbalance = structure.PolarityImbalance
	s.longClauseRatio = structure.LongClauseRatio
	s.binaryRatio = structure.BinaryRatio

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

	// Fix 1: Size-adaptive subsumption period. Small instances (numVars < 500)
	// benefit from period=50 (subsumption fires at restart 50 vs 100); op_18
	// regresses +1.6s and rand3sat_200 +1.4s with period=100. Large instances
	// keep period=100 to avoid subsumption overhead on instances that don't
	// reach 50 restarts before solving. Skipped if the caller explicitly set
	// the period via SetSubsumptionPeriod (CLI or tests).
	if !s.subsumptionPeriodSet && s.cnf.NumVars < 500 {
		s.subsumptionPeriod = 50
	}

	// Activity-based clause deletion gate. Activity-based deletion (VSIDS-style
	// decayed activity for within-LBD-tier deletion ordering) helps structured
	// instances with enough clause diversity but hurts instances whose search
	// trajectories were carefully tuned by other optimizations (D2 pure k-SAT
	// split, B11 adaptive phase flip, etc.). The gate disables activity for:
	//   - Random-like/Pure k-SAT instances (StructuredScore < 0.7): phase-transition
	//     ternary (30eb4ef4) and random 3-SAT (566f366c) timeout with activity.
	//   - Low-density binary-heavy instances (binaryRatio > 0.5 AND density < 4.0):
	//     bb34f22f (binary 67%, density 3.12) regresses from 18s to 31s with activity.
	// The 44092fcc help (binary 97.6%, density 4.61, -12.36s) is preserved because
	// density 4.61 > 4.0.
	if structure.StructuredScore < 0.7 ||
		(structure.BinaryRatio > 0.5 && structure.Density < 4.0) {
		s.claActivityEnabled = false
	}

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
	//   - Pure k-SAT (binaryRatio == 0, density > 4.0): default decay 0.95.
	//     density > 4.5: restartBase=20 (swept: 6x faster than 5 on 566f366c).
	//     density 4.0-4.5: restartBase=100, Glucose=1.5 (phase transition needs
	//     deep search; base≤50 times out on 30eb4ef4).
	//   - Mixed (binaryRatio > 0): D4 smooth threshold interpolation. Instead of
	//     a hard 0.7 cliff, decay interpolates from aggressive (0.50→0.80) to
	//     near-default (0.95). Below 0.60: fully aggressive (D1: needed by 15+
	//     mixed instances). Above 0.70: structured path.
	//     The floor/ceiling were re-tuned after the level-0 literal filter fix:
	//     shorter clauses (no level-0 literals) give a cleaner VSIDS signal, so
	//     less aggressive decay (longer memory) works better. 0.30→0.60 was the
	//     optimum with level-0 literals in clauses; 0.50→0.80 is the optimum
	//     without them. 30eb4ef4: 11s→6.5s, daf59d67: 7s→4.3s.
	// Unit propagation on random/mixed instances causes 76x more conflicts, so
	// preprocessing is disabled in getAdaptivePreprocessingConfig for both.
	if structure.StructuredScore < 0.7 {
		// Pure k-SAT: random 3-SAT (binaryRatio==0, ternaryRatio>0, density>4.0).
		// Uses default decay 0.95. Restart params depend on density:
		//   - density > 4.5 (easy, above phase transition): restartBase=20. Swept
		//     {5,20,50,100} on 566f366c: 5=0.12s, 20=0.02s, 50=0.31s, 100=5.9s.
		//     20 is 6x faster than 5 — the old value was never optimal.
		//   - density 4.0-4.5 (hard, at phase transition): restartBase=100,
		//     Glucose=1.5. restartBase≤50 times out (30eb4ef4: base=20 → 87s,
		//     base=50 → TMO). Only base=100 builds enough search depth.
		//
		// Gate: ternaryRatio > 0. Random k-SAT with k≥4 (all long clauses,
		// uniform size, penalized by Fix 3) needs aggressive decay (0.50→0.80),
		// not default — rand4sat_75: 0.58s aggressive vs 1.45s default.
		// These fall through to the D4 interpolation with t=0 (fully aggressive).
		if structure.BinaryRatio == 0 && structure.TernaryRatio > 0 && structure.Density > 4.0 {
			if structure.Density > 4.5 {
				s.Log("c [classification] Pure k-SAT instance (score=%.2f, density=%.2f) - default decay, restartBase=20\n",
					structure.StructuredScore, structure.Density)
				if !s.flagSet("restart-base") {
					s.restartBase = 20
				}
				if !s.useBumpAnalyzeOverride {
					s.useBumpAnalyze = true
				}
			} else {
				s.Log("c [classification] Pure k-SAT phase-transition (score=%.2f, density=%.2f) - default decay, restartBase=100, Glucose=1.5\n",
					structure.StructuredScore, structure.Density)
				if !s.flagSet("restart-base") {
					s.restartBase = 100
				}
				if !s.flagSet("restart-glucose-ratio") {
					s.restartGlucoseRatio = 1.5
				}
				if !s.flagSet("restart-glucose-min") {
					s.restartGlucoseMinConflicts = 100
				}
				// Analyze_toclear (bump all touched vars, like MiniSat) is the
				// dominant branching-quality lever for random 3-SAT at phase
				// transition: rand3sat_200 drops 99K->21K conflicts (matches
				// minisat 22K). Enabled here for the phase-transition branch,
				// mirroring the density>4.5 pure-k-SAT branch above.
				if !s.useBumpAnalyzeOverride {
					s.useBumpAnalyze = true
				}
			}
			return
		}
		// Random k-SAT k≥4 (binaryRatio==0, ternaryRatio==0, all long clauses
		// with uniform size). Fix 3 penalizes these to score < 0.7. Needs
		// aggressive initial decay (0.50) for quick exploration but high max
		// decay (0.999) so activities persist — long clauses mean more
		// variables per conflict, and forgetting important ones hurts. The D4
		// path's max 0.80 is too aggressive: rand4sat_75 0.5s with 0.999 vs
		// 6.8s with 0.80.
		if structure.BinaryRatio == 0 && structure.TernaryRatio == 0 {
			if !s.flagSet("initial-decay") && !s.flagSet("max-decay") && !s.flagSet("decay-rampup") {
				s.vsids.SetDecayParams(0.50, 0.999, 5000)
			}
			if !s.flagSet("restart-base") {
				s.restartBase = 20
			}
			if !s.flagSet("restart-glucose-ratio") {
				s.restartGlucoseRatio = 100.0
			}
			if !s.flagSet("restart-glucose-min") {
				s.restartGlucoseMinConflicts = 1000000
			}
			s.Log("c [classification] Random k-SAT k≥4 (score=%.2f, density=%.2f) - decay 0.50→0.999, restartBase=20\n",
				structure.StructuredScore, structure.Density)
			return
		}
		// D4: Smooth threshold interpolation in [0.60, 0.70].
		// t=0 (score ≤ 0.60): fully aggressive (0.30→0.60) — D1's 15+ instances.
		// t=1 (score ≥ 0.70): near-default (0.95) — but falls through to structured.
		// In between: smooth interpolation. restartBase=50, Glucose disabled
		// throughout (decay is the main lever; D1 showed decay dominates).
		// Swept {5,20,50,100} on 8 hard mixed instances (1-5s each): total
		// 5=26.5s, 20=24.3s, 50=22.9s, 100=25.5s. 50 is best overall; 5 was
		// never optimal for any instance.
		// Pure ternary instances (binaryRatio=0, density>4.0) are handled by
		// the pure k-SAT path above. This path handles mixed instances only.
		// Gate: binaryRatio > 0.4 for t>0 interpolation. rphp (62% binary) has
		// strong binary implication structure that benefits from longer conflict
		// memory. D1 mixed cluster (23-34% binary) stays fully aggressive (t=0).
		t := 0.0
		if structure.BinaryRatio > 0.4 && structure.StructuredScore > 0.60 {
			t = (structure.StructuredScore - 0.60) / 0.10
			if t > 1.0 {
				t = 1.0
			}
		}
		initialDecay := s.decayFloor + t*(0.95-s.decayFloor)
		maxDecay := s.decayCeil + t*(0.95-s.decayCeil)
		// The decay interpolation is a unit: if the user set any of the decay
		// flags, skip the whole SetDecayParams call and keep CLI values for all
		// three. Partial override produces a nonsensical mix.
		if !s.flagSet("initial-decay") && !s.flagSet("max-decay") && !s.flagSet("decay-rampup") {
			s.vsids.SetDecayParams(initialDecay, maxDecay, 5000)
		}
		if !s.flagSet("restart-base") {
			s.restartBase = 50
		}
		// High-density ternary-heavy mixed instances (e.g. course-timetabling:
		// daf59d, 262ba88b) score 0.62-0.69, just under the structured threshold,
		// but are genuinely structured: they wander in decision space (~200
		// decisions/conflict during SAT model search) and benefit critically
		// from Glucose restarts. The generic D4 path disables Glucose (ratio=100,
		// min=1e6) because aggressive decay is the dominant lever for low-density
		// mixed/random instances, but that leaves these wanderers unguided.
		// Gate on Density>4.5 && TernaryRatio>0.7: captures all four 0.69
		// wanderers, excludes lower-density mixed instances (66e6fea density
		// 3.91, which regresses with Glucose: 4.41s->4.79s) and never touches
		// pure-k-SAT/random (density<=4.5 or ternary<=0.7).
		wanderer := structure.Density > 4.5 && structure.TernaryRatio > 0.7
		if !s.flagSet("restart-glucose-ratio") {
			if wanderer {
				s.restartGlucoseRatio = 1.5
			} else {
				s.restartGlucoseRatio = 100.0
			}
		}
		if !s.flagSet("restart-glucose-min") {
			if wanderer {
				s.restartGlucoseMinConflicts = 100
			} else {
				s.restartGlucoseMinConflicts = 1000000
			}
		}
		if wanderer {
			s.Log("c [classification] Mixed wanderer (density=%.2f, ternary=%.1f%%) - Glucose restarts enabled (1.5/%d)\n",
				structure.Density, structure.TernaryRatio*100, s.restartGlucoseMinConflicts)
		}
		s.Log("c [classification] Random-like mixed (score=%.2f, t=%.2f) - decay %.2f→%.2f, restartBase=50\n",
			structure.StructuredScore, t, initialDecay, maxDecay)
		return
	}

	s.Log("c [classification] Structured instance (score=%.2f)\n", structure.StructuredScore)

	// Enable bumpAnalyze for structured non-binary instances (default decay).
	// Binary-heavy instances (binaryRatio > 0.5) are excluded to protect
	// bb34f22f's phase-flip escape dynamics.
	// High-density + low-PolImb instances are excluded: 274099073 (density=15.6,
	// PolImb=0.015, 99.4% long) regresses -8.8s — balanced polarities mean VSIDS
	// is the sole guidance signal; diluting it across all touched vars hurts.
	// 69d72f81 (density=11.6, PolImb=0.839) is kept — high PolImb means phase
	// saving provides strong guidance, so broader VSIDS exploration helps.
	if !s.useBumpAnalyzeOverride {
		s.useBumpAnalyze = structure.BinaryRatio <= 0.5 &&
			(structure.Density < 10.0 || structure.PolarityImbalance > 0.4)
	}

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
		if !s.flagSet("restart-base") {
			s.restartBase = 20
		}
		s.Log("c [classification] Binary-heavy (%.0f%%) - restartBase=20\n", structure.BinaryRatio*100)
	}
}

// maybeAdaptDecay is the runtime correction layer for the static classifier.
//
// Motivation: classifyInstance predicts search behavior from SYNTACTIC features
// (clause-size histogram, density). The prediction is fragile — a single
// off-size clause among 1000 uniform-k clauses flips longSizeVaried=true and
// misclassifies random k-SAT as structured (wrong decay, restartBase=200,
// Glucose active → slow or TMO). Random 4-SAT needed Fix 3; random 5-SAT and
// noisier variants are one accident away from the same failure.
//
// This layer measures BEHAVIORAL structure (glue ratio = fraction of learned
// clauses with LBD ≤ 2) over rolling windows and corrects the decay schedule
// when behavior disagrees with the syntactic prediction. Glue ratio is the
// canonical structured-vs-random discriminator: structured instances (Tseitin,
// hardware, combinatorial) produce many glues via tight implication chains;
// random k-SAT produces almost none.
//
// Periodic re-check with rolling windows: instead of a single one-shot at
// conflict 500, we re-evaluate every adaptWindowSize conflicts. Each window
// sees only RECENT behavior (delta of cumulative counters), not diluted
// cumulative stats. This catches instances whose behavior shifts mid-search
// (starts structured, becomes random) and instances where the first window
// was borderline. The one-shot version could miss both.
//
// Safety scoping (one-way forward guard):
//   - Exempt when structureScore < 0.7: random branches (pure 3-SAT, random
//     k≥4, D4-mixed) already have appropriate aggressive decay set by the
//     classifier. Pure 3-SAT (566f366c) correctly uses default 0.95 and has a
//     low glue ratio — overriding it to aggressive decay would regress 118×
//     (per existing tuning notes). The score < 0.7 gate exempts it.
//   - Exempt when the user set any decay flag: respect explicit CLI.
//   - One-way: once decayAdapted is set, the override is permanent. Reverting
//     aggressive→default would distort VSIDS — under aggressive decay, varInc
//     grows huge (÷0.5 each conflict); switching back to 0.95 would leave a
//     massive varInc that freezes scores until rescaling. The aggressive
//     ramp's max (0.999) self-heals for the reverse case.
func (s *CDCLSolver) maybeAdaptDecay() {
	if s.decayAdapted {
		return
	}

	// Respect explicit CLI decay flags (initial-decay, max-decay, decay-rampup).
	if s.flagSet("initial-decay") || s.flagSet("max-decay") || s.flagSet("decay-rampup") {
		return
	}

	// Only correct the STRUCTURED branch. Random branches (score < 0.7) were
	// tuned with aggressive decay that low glue ratio would (wrongly) confirm.
	if s.structureScore < 0.7 {
		return
	}

	// Exempt binary-heavy instances. Binary cascades produce high-LBD learned
	// clauses (spanning many decision levels), so few are glue (LBD≤2) — the
	// low glue ratio means "binary cascade," not "random." bb34f22f (67% binary,
	// glue ratio 0.04) and de2b584e (99.8% binary, glue ratio 0.00) are genuine
	// structured instances that need default decay. The target (noisy random
	// k-SAT) always has binaryRatio ≈ 0 (pure k-SAT has no binary clauses), so
	// exempting binaryRatio > 0.5 doesn't weaken the guard's coverage.
	if s.binaryRatio > 0.5 {
		return
	}

	// Not time to re-check yet. adaptNextConflict starts at the warmup (500)
	// and advances by adaptWindowSize after each evaluation.
	if s.conflicts < s.adaptNextConflict {
		return
	}

	// Rolling window: compute stats from the delta of cumulative counters since
	// the last check. This isolates recent behavior rather than diluting it
	// with cumulative stats from earlier (possibly different) search phases.
	const adaptWindowSize = 500
	winCount := s.totalLbdCount - s.countAtLastAdapt
	if winCount == 0 {
		s.adaptNextConflict += adaptWindowSize
		return
	}
	winGlue := s.glueLearned - s.glueAtLastAdapt
	winLbdSum := s.totalLbdSum - s.lbdSumAtLastAdapt
	glueRatio := float64(winGlue) / float64(winCount)
	avgLBD := float64(winLbdSum) / float64(winCount)

	// Snapshot cumulative counters for the next window's delta computation.
	s.glueAtLastAdapt = s.glueLearned
	s.lbdSumAtLastAdapt = s.totalLbdSum
	s.countAtLastAdapt = s.totalLbdCount
	s.adaptNextConflict = s.conflicts + adaptWindowSize

	// Two-signal test: BOTH must indicate random behavior.
	//   - glueRatio < 0.10: few glue clauses (LBD ≤ 2). Necessary but not
	//     sufficient — long-clause structured instances also have low glue
	//     ratios because long clauses naturally produce high-LBD learned clauses.
	//   - avgLBD > 25: conflicts span ~25+ decision levels with no tight
	//     implication chains. This separates random k-SAT (avgLBD 30-40, no
	//     structure) from long-clause structured instances (avgLBD ~15, structure
	//     exists but is obscured by clause length). Measured: noisy random 5-SAT
	//     avgLBD=34.4 → fires; 274099073 avgLBD=15.9 → correctly exempt.
	//
	// The conjunction prevents false positives on structured instances that
	// happen to have low glue ratios (binary cascades, long clauses) while
	// still catching genuinely random search behavior.
	const glueRatioThreshold = 0.10
	const avgLBDThreshold = 25.0
	if glueRatio < glueRatioThreshold && avgLBD > avgLBDThreshold {
		s.vsids.SetDecayParams(0.50, 0.999, 5000)
		s.decayAdapted = true
		s.Log("c [adapt] decay override at conflict %d: window glueRatio=%.3f avgLBD=%.1f → decay 0.50→0.999\n",
			s.conflicts, glueRatio, avgLBD)
		return
	}
	// Verbose-only: confirm each window so structured instances don't spam the
	// log. The override log above always fires (it's rare and actionable).
	if s.verbose {
		s.Log("c [adapt] window at conflict %d: glueRatio=%.3f avgLBD=%.1f — keeping default decay\n",
			s.conflicts, glueRatio, avgLBD)
	}
}

// getAdaptivePreprocessingConfig returns preprocessing config based on the
// cached structureScore (set by classifyInstance, which must have run first).
func (s *CDCLSolver) getAdaptivePreprocessingConfig() PreprocessingConfig {
	if s.structureScore < 0.7 && s.binaryRatio <= 0.5 {
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
			s.assignments[v].Level = 0
			s.assignments[v].Value = pureValue[v]
			s.assignments[v].Reason = -3 // pure literal preprocessing
			s.preprocessTrail = append(s.preprocessTrail, uint32(v))
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

	ppStart := time.Now()
	ppMark := func(step string) {
		if s.verbose {
			s.Log("c [preproc-time] %-28s %8.1fms\n", step, float64(time.Since(ppStart).Microseconds())/1000.0)
		}
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
	ppMark("tautology/duplicate removal")

	// Pure literal elimination (all instances, trivially sound).
	if pleAssigned := s.pureLiteralElimination(); pleAssigned > 0 {
		s.Log("c [preprocessing] Pure literal elimination: assigned %d variables\n", pleAssigned)
		s.cnf.RebuildLiteralPool()
	}
	ppMark("pure-literal elimination")

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
	ppMark("adaptive config")

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

	// Bound subsumption's O(n²)-ish clause-pair scans adaptively. Subsumption
	// only runs on structured instances (config.EnableUnitProp). Small instances
	// keep unlimited behavior (proven tuning); larger ones get a budget scaled
	// to the original literal count so heavy long-clause processing stays
	// bounded instead of dominating the whole run (e.g. stone_3_tree12 was ~69%
	// of wall time in subsumptionPass). 0 = unlimited.
	if s.subsumptionBudget == 0 && int(s.cnf.NumVars) >= 8000 {
		if totalLits := len(s.cnf.GetLiteralPool()); totalLits > 0 {
			s.subsumptionBudget = 25 * totalLits
			s.Log("c [verbose] subsumption budget=%d (%d vars, %d lits)\n",
				s.subsumptionBudget, s.cnf.NumVars, totalLits)
		}
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
			// Self-subsumption can strengthen a clause to empty (removing all
			// literals one by one). An empty clause means UNSAT.
			if s.hasEmptyClause() {
				s.Log("c [preprocessing] Empty clause after subsumption — UNSAT\n")
				s.printStats()
				return UNSAT
			}
		}
		ppMark("subsumption pass")

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
			// VE expands the clause DB; tighten the next subsumption scan so later
			// passes don't burn the full budget re-scanning the bloated DB.
			if s.subsumptionBudget > 0 && veResult > 0 {
				s.subsumptionBudget /= 2
			}
		}

		// Unit propagation
		if config.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}
		ppMark("unit propagation")

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
	// Only clear Reason for unassigned variables. Preprocessing assignments
	// (Level >= 0) must keep their Reason to prevent re-propagation.
	for i := range s.assignments {
		if s.assignments[i].Level < 0 {
			s.assignments[i].Reason = -1
		}
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
	ppMark("rebuild pool")

	// Initialize watches after unit propagation
	// CRITICAL: Reset watchInitialized flag so watches are re-initialized
	s.watchInitialized = false
	s.initWatches()
	ppMark("init watches")

	// CRITICAL: Propagate original unit clauses + activate watches for preprocessing vars.
	if unsat := s.propagateOriginalUnitsAndActivateWatches(); unsat {
		return UNSAT
	}
	ppMark("propagate units")

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
		if len(clause.Literals) == 0 {
			continue
		}
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
		s.assignments[varIdx].Level = 0
		s.assignments[varIdx].Value = value
		s.assignments[varIdx].Reason = -2
		s.preprocessTrail = append(s.preprocessTrail, varIdx)
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
	// Sync litTrue with preprocessing assignments. Preprocessing (unit prop,
	// BVE, pure-literal) sets assignments without updating litTrue — litTrue is
	// only initialized later in SolveWithResult (line ~3148). The binary fast
	// path in propagateWatched trusts litValue[watch.Blit] to skip satisfied
	// clauses; if litTrue is stale (false for an actually-true literal), it
	// falls through to the conflict path and reports a false UNSAT.
	for _, v := range s.trail {
		val := s.assignments[v].Value
		s.litTrue[int(v)*2] = val
		s.litTrue[int(v)*2+1] = !val
	}
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

// failedLiteralProbing performs failed-literal probing (FLP) as a preprocessing
// step. For each unassigned variable v, tries v=true and v=false; if either
// assignment causes a conflict, v is a failed literal and its negation is
// forced (added as a level-0 assignment).
//
// Uses probePropagate (non-watch BCP via occurrence lists) so NO solver state
// (trail, assignments, watches) is modified during probing — only temporary
// tmpValue/tmpAssigned arrays are touched and restored after each probe.
//
// Gate: only runs on random-like instances (structureScore < 0.7 and
// binaryRatio <= 0.5). On structured instances, aggressive preprocessing
// (subsumption, BVE, equivalence) already simplifies the formula, and FLP's
// trajectory change from the relatively few failed literals it finds can hurt.
func (s *CDCLSolver) failedLiteralProbing() SolveResult {
	if int(s.cnf.NumVars) > s.preprocessingMaxVars {
		return UNKNOWN
	}

	if s.structureScore >= 0.7 || s.binaryRatio > 0.5 {
		return UNKNOWN
	}

	numVars := int(s.cnf.NumVars)
	if numVars == 0 || s.cnf.NumClauses == 0 {
		return UNKNOWN
	}
	numLits := numVars * 2

	occ := make([][]int, numLits)
	for i := range s.cnf.Clauses {
		lits := s.cnf.Clauses[i].Literals
		if len(lits) == 0 {
			continue
		}
		for _, lit := range lits {
			occ[cnf.LitToIndex(lit)] = append(occ[cnf.LitToIndex(lit)], i)
		}
	}

	tmpValue := make([]bool, numVars)
	tmpAssigned := make([]bool, numVars)
	for v := 0; v < numVars; v++ {
		if s.assignments[v].Level >= 0 {
			tmpAssigned[v] = true
			tmpValue[v] = s.assignments[v].Value
		}
	}

	budget := 2000
	probed := 0
	var failedLits []cnf.Literal

	s.probeTrail = s.probeTrail[:0]

	for v := uint32(0); v < s.cnf.NumVars && probed < budget; v++ {
		if tmpAssigned[v] {
			continue
		}
		probed++

		trailLen := len(s.probeTrail)
		tmpAssigned[v] = true
		tmpValue[v] = true
		s.probeTrail = append(s.probeTrail, int(v)*2)
		conflict := s.probePropagate(occ, tmpValue, tmpAssigned, trailLen)
		for i := trailLen; i < len(s.probeTrail); i++ {
			tmpAssigned[uint32(s.probeTrail[i])>>1] = false
		}
		s.probeTrail = s.probeTrail[:trailLen]
		tmpAssigned[v] = false

		if conflict {
			failedLits = append(failedLits, cnf.NewLiteral(v, true))
			continue
		}

		trailLen = len(s.probeTrail)
		tmpAssigned[v] = true
		tmpValue[v] = false
		s.probeTrail = append(s.probeTrail, int(v)*2+1)
		conflict = s.probePropagate(occ, tmpValue, tmpAssigned, trailLen)
		for i := trailLen; i < len(s.probeTrail); i++ {
			tmpAssigned[uint32(s.probeTrail[i])>>1] = false
		}
		s.probeTrail = s.probeTrail[:trailLen]
		tmpAssigned[v] = false

		if conflict {
			failedLits = append(failedLits, cnf.NewLiteral(v, false))
		}
	}

	s.Log("c [preprocessing] Failed literal probing: %d probes, %d failed literals\n", probed, len(failedLits))

	if len(failedLits) == 0 {
		return UNKNOWN
	}

	s.numUnassigned = 0
	for v := uint32(0); v < s.cnf.NumVars; v++ {
		if s.assignments[v].Level < 0 {
			s.numUnassigned++
		}
	}

	for _, lit := range failedLits {
		v := lit.Var()
		if s.assignments[v].Level >= 0 {
			if s.assignments[v].Value != !lit.IsNegated() {
				return UNSAT
			}
			continue
		}
		value := !lit.IsNegated()
		s.assignments[v] = Assignment{Value: value, Level: 0, Reason: -2}
		s.litTrue[int(v)*2] = value
		s.litTrue[int(v)*2+1] = !value
		s.preprocessTrail = append(s.preprocessTrail, v)
		s.numUnassigned--
	}

	if s.propagateOriginalUnitsAndActivateWatches() {
		return UNSAT
	}

	return UNKNOWN
}

// probePropagate performs Boolean constraint propagation without watch lists.
// Scans occurrence lists read-only, appending newly derived literals to
// s.probeTrail. Returns true if a conflict is detected.
// tmpValue/tmpAssigned are modified in-place; the caller restores them after.
func (s *CDCLSolver) probePropagate(occ [][]int, tmpValue []bool, tmpAssigned []bool, trailStart int) bool {
	clauses := s.cnf.Clauses
	head := trailStart
	for head < len(s.probeTrail) {
		litIdx := s.probeTrail[head]
		head++

		negIdx := litIdx ^ 1
		for _, ci := range occ[negIdx] {
			clauseLits := clauses[ci].Literals
			unassignedCount := 0
			unassignedLitIdx := 0
			satisfied := false
			for _, lit := range clauseLits {
				v := lit.Var()
				if tmpAssigned[v] {
					if tmpValue[v] != lit.IsNegated() {
						satisfied = true
						break
					}
					continue
				}
				unassignedCount++
				unassignedLitIdx = cnf.LitToIndex(lit)
			}
			if satisfied {
				continue
			}
			if unassignedCount == 0 {
				return true
			}
			if unassignedCount == 1 {
				v := uint32(unassignedLitIdx >> 1)
				tmpAssigned[v] = true
				tmpValue[v] = !(unassignedLitIdx&1 == 1)
				s.probeTrail = append(s.probeTrail, unassignedLitIdx)
			}
		}
	}
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
	// Set binary bit (bit 29) for size-2 clauses to enable binary fast path.
	binaryBit := int32(0)
	if len(literals) == 2 {
		binaryBit = int32(watchBinaryBit)
	}
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseIdx: int32(clauseIdx) | binaryBit,
		Blit:      litToBlit(lit1),
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseIdx: int32(clauseIdx) | int32(watchMyPosBit) | binaryBit,
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

	// Pack learned flag (bit 31) + myPos (bit 30) + binary (bit 29) into ClauseIdx.
	clauseIdx0 := int32(watchLearnedBit | uint32(learnedIdx)) // myPos=0
	clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)           // myPos=1
	if len(literals) == 2 {
		clauseIdx0 |= int32(watchBinaryBit)
		clauseIdx1 |= int32(watchBinaryBit)
	}

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

	// Mark clauses used as reasons (cannot vivify these — they're in use).
	protected := s.markProtectedClauses()

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
		s.learnedSearchHint[r.idx] = 0
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
			s.assignments[i].Level = -1
			s.assignments[i].Value = false
			s.assignments[i].Reason = -1
			s.litTrue[i*2] = false
			s.litTrue[i*2+1] = false
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

	// Clause identity for comparison: mask out myPos bit (bit 30) and binary bit
	// (bit 29) since the stored watches may have either myPos value.
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
				return 1 << uint(k-1)
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
	// MiniSat-style mode: pure geometric restart, no Glucose LBD criterion.
	// threshold = restartBase * 1.5^lubyIndex. Cached incrementally in
	// geometricRestartThreshold (updated in restart() alongside lubyIndex) to
	// avoid math.Pow on every conflict. With base=100: 100, 150, 225, 337, ...
	if s.minisatRestart {
		if s.geometricRestartThreshold == 0 {
			s.geometricRestartThreshold = float64(s.restartBase)
		}
		if float64(s.conflicts-s.restartCount) >= s.geometricRestartThreshold {
			s.restartReasons[4]++ // geometric
			return true
		}
		// Still allow props/dec and level-capped early escape (below).
	} else {
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
				s.restartReasons[0]++ // glucose
				s.Log("c [restart] Glucose: EMA LBD %.1f > avg %.1f × %.2f\n",
					s.emaLBD, avgLBD, s.restartGlucoseRatio)
				return true
			}
		}

		// Fall back to Luby sequence (configurable base), scaled by an adaptive
		// gear. The raw Luby sequence alone oscillates back to short segments
		// (1,1,2,1,1,2,4,...) forever, so on flat-high-LBD instances where
		// Glucose can never fire (EMA ≈ avg), the fallback throttles search into
		// a permanent cycle of frequent shallow restarts, never letting it go
		// deep. Raising the gear (rarer restarts) on such grinders lets deep
		// search develop. Gear = 1.0 reproduces baseline exactly; it is moved
		// only by the behavioral governor in restart(). Bounded via
		// maxAdaptiveRestartGear so it can never starve diversification.
		lubyValue := luby(s.lubyIndex + 1)
		threshold := float64(lubyValue*s.restartBase) * s.adaptiveRestartGear

		if float64(s.conflicts-s.restartCount) >= threshold {
			s.restartReasons[1]++ // luby
			return true
		}

		// If the threshold exceeds the cap, reset the index so the sequence
		// restarts from the beginning on the next restart.
		if s.lubyThresholdCap > 0 && threshold > float64(s.lubyThresholdCap) {
			s.lubyIndex = 0
		}
	}

	// Props/dec-bounded restart: if the solver is going too deep per decision
	// (unproductive binary cascade), restart to escape the trajectory. This
	// catches the "deep search" pathology where props/dec >> 100 (e.g.,
	// binary-heavy instances where each decision cascades through hundreds of
	// binary clauses for a single conflict). The Glucose EMA criterion doesn't
	// fire here because binary cascades produce low-LBD glue clauses, making
	// the search look productive by LBD metrics when it's actually going nowhere.
	// Requires a minimum conflict gap to prevent thrashing (the solver needs
	// time to explore between restarts, and the phase flip needs time to take
	// effect). In no-init mode, fires from conflict 0 (early escape needed to
	// compensate for missing VSIDS init). In default init-on mode, gated on
	// lubyIndex >= 3 (matches B3 baseline behavior).
	if s.restartPropsDecLimit > 0 && s.decisions > 10 &&
		(s.skipVSIDSInit || s.lubyIndex >= 3) &&
		s.conflicts-s.restartCount >= s.propsDecRestartGap {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		if propsPerDec > float64(s.restartPropsDecLimit) {
			s.restartReasons[2]++ // propsdec
			s.Log("c [restart] Props/dec %.1f > %d\n", propsPerDec, s.restartPropsDecLimit)
			return true
		}
	}

	// Tier 2: deep-search escape. A low props/dec threshold gated on unrewarded
	// deep search (conflict level exceeding adaptPropDecDeepGate). This restarts
	// instances stuck in a deep cascade (high conflict level) despite only modest
	// average props/dec (e.g. 8d58ca: level up to 4400), while leaving shallow
	// productive cascades (30eb4e2 level<=28) and high-props/dec cascade-bound
	// instances (caught by Tier 1) untouched.
	if s.adaptPropDecDeepGate > 0 &&
		s.decisions > 10 && (s.skipVSIDSInit || s.lubyIndex >= 3) &&
		s.conflicts-s.restartCount >= s.propsDecRestartGap {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		if propsPerDec > float64(s.adaptPropDecLimit) &&
			s.lastConflictLevel > s.adaptPropDecDeepGate {
			s.restartReasons[2]++ // propsdec
			s.Log("c [restart] Props/dec %.1f > %d (deep-search, level %d > gate %d)\n", propsPerDec, s.adaptPropDecLimit, s.lastConflictLevel, s.adaptPropDecDeepGate)
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
	// restart instead). Anti-thrashing: decisions > 10, min-conflict gap.
	// In no-init mode, fires from conflict 0. In default init-on mode, gated
	// on lubyIndex >= 3 (matches B3 baseline behavior).
	if s.restartLevelCap > 0 && s.longClauseRatio > 0.8 &&
		(s.skipVSIDSInit || s.lubyIndex >= 3) &&
		s.decisions > 10 &&
		s.conflicts-s.restartCount >= s.levelRestartGap &&
		s.lastConflictLevel > s.restartLevelCap {
		s.restartReasons[3]++ // levelcap
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
// unassignVar clears the assignment for a single variable and updates all
// derived state (litTrue, implication, numUnassigned). Used by cancelUntil,
// backtrack, and restart to unassign variables from the trail.
func (s *CDCLSolver) unassignVar(varIdx uint32) {
	s.assignments[varIdx].Level = -1
	s.assignments[varIdx].Value = false
	s.assignments[varIdx].Reason = -1
	s.litTrue[int(varIdx)*2] = false
	s.litTrue[int(varIdx)*2+1] = false
	s.numUnassigned++
}

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
		s.unassignVar(s.trail[i])
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:level+1]
	s.level = level
	s.qhead = len(s.trail)
}

func (s *CDCLSolver) restart() bool {
	s.unitsDirty = true // Restart clears all assignments; units need re-propagation
	s.Log("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)

	// Record the just-ended segment's productivity (instrumentation): conflicts
	// per 1000 decisions. A low value means the search made little progress per
	// decision (unproductive deep/branching search); high means many conflicts
	// per decision (dense cascades). Both extremes are signals the governor can
	// use. Segments with no decisions (preprocessing) yield a neutral 0.
	segDec := s.decisions - s.restartSegStartDec
	segConf := s.conflicts - s.restartSegStartConf
	if segDec > 0 {
		s.restartSegProdRatio = float64(segConf) * 1000.0 / float64(segDec)
	} else {
		s.restartSegProdRatio = 0.0
	}
	s.restartSegGlueCount = int(s.glueLearned) - s.restartSegStartGlue
	if s.verbose {
		s.Log("c [verbose] segment: %d conflicts / %d decisions = %.1f conflicts/1kdec, %d glue learned\n",
			s.conflicts-s.restartSegStartConf, segDec, s.restartSegProdRatio, s.restartSegGlueCount)
	}
	s.restartSegStartConf = s.conflicts
	s.restartSegStartDec = s.decisions
	s.restartSegStartProps = s.propagations
	s.restartSegStartGlue = int(s.glueLearned)

	// Adaptive restart gear governor. Advance only when BOTH hold:
	//   1. The search has passed the uniform conflict horizon
	//      (restartPatienceConflicts) without a result — the default schedule
	//      has demonstrably failed to converge, so deepening is justified.
	//      Instances that solve within an ordinary budget never reach this.
	//   2. There has been NO Glucose ever and no glue learned in this segment —
	//      the flat-LBD grinder signature where the LBD-Glucose restart is
	//      dead (rphp: zero glue, zero Glucose, grinds past 300K conflicts).
	// Decay back to 1.0 whenever Glucose or glue activity appears, since those
	// instances need their frequent diversification preserved.
	s.restartSegTotalConflicts += uint64(segConf)
	s.restartSegSteps++
	if s.adaptiveRestartGear < maxAdaptiveRestartGear {
		if s.restartSegTotalConflicts > restartPatienceConflicts &&
			s.restartReasons[0] == 0 && s.restartSegGlueCount == 0 {
			s.adaptiveRestartGear += 0.5
			if s.adaptiveRestartGear > maxAdaptiveRestartGear {
				s.adaptiveRestartGear = maxAdaptiveRestartGear
			}
			if s.verbose {
				s.Log("c [governor] flat-LBD grind: advancing gear -> %.1f\n", s.adaptiveRestartGear)
			}
		} else if s.restartReasons[0] > 0 || s.restartSegSteps > 8 {
			// Any Glucose activity, or a well-settled instance without sign of
			// grind, pulls the gear back toward baseline diversification.
			s.adaptiveRestartGear -= 0.5
			if s.adaptiveRestartGear < 1.0 {
				s.adaptiveRestartGear = 1.0
			}
		}
	}

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
	// Iterate the trail (only assigned vars) instead of scanning the full assignment array.
	for i := 0; i < len(s.trail); i++ {
		s.unassignVar(s.trail[i])
	}
	s.trail = s.trail[:0]
	s.trailHead = append(s.trailHead[:0], 0)
	s.qhead = 0
	s.level = 0
	// Restart clears all search assignments. Sunk heap entries (at -Inf) are
	// NOT restored by onUnassign (restart doesn't call it). Force a rebuild
	// to fix all entries with current scores.
	s.vsids.heapValid = false

	// Reset restart counters
	s.lubyIndex++
	if s.minisatRestart {
		if s.geometricRestartThreshold == 0 {
			s.geometricRestartThreshold = float64(s.restartBase)
		}
		s.geometricRestartThreshold *= 1.5
	}
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
		// Phase flip tracks the ABSOLUTE props/dec magnitude (deep binary
		// cascades), independent of the restart trigger's low limit and its
		// deep-search gate. Baseline thresholds (120% / 80% of 100) preserve
		// pre-existing behavior: only genuinely cascade-bound instances (e.g.
		// bb34f22f at props/dec ~175) enable flipping.
		enableThreshold := float64(propsDecPhaseFlipBase) * 12 / 10
		disableThreshold := float64(propsDecPhaseFlipBase) * 8 / 10
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
		for i := range s.assignments {
			s.randomSeed ^= s.randomSeed << 13
			s.randomSeed ^= s.randomSeed >> 7
			s.randomSeed ^= s.randomSeed << 17
			if float64(s.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF) < s.restartPhaseFlipRate {
				s.assignments[i].SavedPhase = !s.assignments[i].SavedPhase
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

	// Runtime decay adaptation: periodic re-check with rolling windows.
	// Called at restart boundaries (cold path) — calling from the hot CDCL loop
	// caused 69d72f81 to regress 9s→TMO due to compiler generating worse loop
	// code around the non-inlinable call target, even when gated to 1/500
	// conflicts. Restart boundaries are already cold (vivify/subsumption run
	// here), so the call is free.
	//
	// Gate on lubyIndex%adaptPeriod == 0 (like vivify/subsumption) to avoid
	// calling the function at every restart — even the call overhead at every
	// restart caused 1.7s regression on 69d72f81 (8.8s→10.5s). adaptPeriod=10
	// means the function is called every 10th restart; internally it early-
	// returns if s.conflicts < s.adaptNextConflict, so most calls are a single
	// comparison + return.
	const adaptRestartPeriod = 10
	if !s.decayAdapted && s.lubyIndex > 0 && s.lubyIndex%adaptRestartPeriod == 0 {
		s.maybeAdaptDecay()
	}

	// Unified search governor: runtime self-correction compensating for
	// classifier misclassification. Same cadence as decay adaptation (every
	// adaptRestartPeriod restarts, cold path); internally window-gated on
	// govNextConflict (govWindowConfScope conflicts), so it is effectively a
	// compare+return plus a cheap window eval every 20K conflicts.
	if s.lubyIndex > 0 && s.lubyIndex%adaptRestartPeriod == 0 {
		s.maybeAdaptSearch()
	}

	// Lazy init: detect bad trajectory and inject occurrence-based VSIDS bump.
	// Only fires when skipVSIDSInit is true — checked here to avoid the
	// function call overhead when lazy init is enabled but skipVSIDSInit is not.
	if s.lazyInit != nil && !s.lazyInit.done && s.skipVSIDSInit {
		s.maybeLazyInit()
	}

	return false // No UNSAT detected
}

// maybeLazyInit detects when the search has locked onto a bad trajectory and
// injects an occurrence-based VSIDS bump to escape it. This is the reactive
// counterpart to initVSIDSOccurrenceBonus: instead of always injecting an init
// prior (which creates trajectory sensitivity), it injects only when the search
// is demonstrably stuck. One-shot (li.done).
//
// Only active when skipVSIDSInit is true — when static init is already applied,
// the occurrence signal is already in VSIDS activity, and injecting more just
// amplifies the prior harmfully. Lazy init is a REPLACEMENT for static init,
// not a supplement.
//
// Detection requires ALL of:
//   - Enough data: totalLbdCount > li.minConflicts
//   - High avg LBD: totalAvgLBD > li.avgLBDThreshold (consistently bad learned clauses)
//   - Low glue rate: glueLearned/totalLbdCount < li.glueRateLimit (no tight implication chains)
//   - Flat/increasing LBD trend: emaLBD hasn't improved across the last li.trendWindow restarts
//
// The trend check distinguishes "bad trajectory" (stuck, not improving) from
// "legitimately hard instance" (slow but improving — LBD trend is downward).
func (s *CDCLSolver) maybeLazyInit() {
	li := s.lazyInit

	// Snapshot emaLBD at each restart for trend analysis.
	li.emaLBDSnapshots = append(li.emaLBDSnapshots, s.emaLBD)

	// Size guard: occurrence-based priors are most informative for small
	// instances where the search space is compact. For large instances,
	// the occurrence distribution adds noise — skip injection.
	if s.cnf.NumVars > 2000 {
		li.done = true
		return
	}

	// Need enough data for stable metrics.
	if s.totalLbdCount < uint64(li.minConflicts) {
		return
	}

	// Condition 1: high average LBD (consistently bad learned clauses).
	totalAvgLBD := float64(s.totalLbdSum) / float64(s.totalLbdCount)
	if totalAvgLBD <= li.avgLBDThreshold {
		return
	}

	// Condition 2: low glue rate (no tight implication chains found).
	glueRate := float64(s.glueLearned) / float64(s.totalLbdCount)
	if glueRate >= li.glueRateLimit {
		return
	}

	// Condition 3: flat or increasing LBD trend across recent restarts.
	// The search isn't learning — emaLBD is not going down.
	n := len(li.emaLBDSnapshots)
	if n < li.trendWindow+1 {
		return // not enough snapshots yet
	}
	oldest := li.emaLBDSnapshots[n-li.trendWindow-1]
	newest := li.emaLBDSnapshots[n-1]
	// Trend must be flat or increasing (not improving by more than 5%).
	if newest < oldest*0.95 {
		return // improving — don't interfere
	}

	// Bad trajectory detected. Inject occurrence-based VSIDS bump.
	s.Log("c [lazy-init] Bad trajectory detected: avgLBD=%.1f, glueRate=%.3f, emaLBD %.1f→%.1f (flat/increasing)\n",
		totalAvgLBD, glueRate, oldest, newest)
	s.injectOccurrenceBonus()
	li.done = true
}

// injectOccurrenceBonus resets VSIDS activity to zero, then injects an
// occurrence-based prior: variables appearing in more original clauses get
// higher activity. The bump is scaled relative to current varInc so it's
// competitive with future conflict bumps. Used by lazy init to recreate
// the init-on state mid-search after a bad trajectory is detected.
func (s *CDCLSolver) injectOccurrenceBonus() {
	// Reset VSIDS activity to clear bad trajectory accumulation, then inject
	// occurrence-based prior.
	s.vsids.ResetActivity()

	occurrences := make([]int, s.cnf.NumVars)
	maxOcc := 0
	for _, clause := range s.cnf.Clauses {
		for _, lit := range clause.Literals {
			occurrences[lit.Var()]++
		}
	}
	for _, occ := range occurrences {
		if occ > maxOcc {
			maxOcc = occ
		}
	}
	if maxOcc == 0 {
		return
	}

	// Scale: bump = occurrenceWeight × varInc × (occ/maxOcc).
	// The most-occurring var gets occurrenceWeight × varInc (comparable to
	// one conflict bump). Less-occurring vars get proportionally less.
	scale := s.occurrenceWeight * s.vsids.varInc / float64(maxOcc)
	for i := range s.assignments {
		if s.assignments[i].Level < 0 {
			s.vsids.activity[i] += float64(occurrences[i]) * scale
		}
	}
}

func (s *CDCLSolver) unitPropagationPreprocess() SolveResult {
	// FIX: Do NOT clear existing assignments (from pure literal elimination, etc.)
	// Only reset trail and propagate NEW unit clauses from current state
	// Initialize trail for preprocessing (reuse capacity)
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.trailHead[0] = 0
	s.level = 0 // Unit propagations at level 0, decisions start at level 1

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

			// Empty clause = UNSAT (can appear from subsumption strengthening)
			if len(clause.Literals) == 0 {
				s.Log("c [verbose] Preprocessing: empty clause detected during unit propagation\n")
				return UNSAT
			}

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
					Value:  value,
					Level:  0,  // Unit propagations at level 0
					Reason: -2, // Assigned by preprocessing unit propagation
				}
				s.trail = append(s.trail, varIdx)
				changed = true
				if s.verbose && len(clause.Literals) == 1 {
					s.Log("c [unit prop] Propagated unit clause: var %d = %v, Reason=%d\n", varIdx, value, s.assignments[varIdx].Reason)
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
	if s.skipVSIDSInit {
		return
	}
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
			s.vsids.activity[i] += float64(occurrences[i]) * s.occurrenceWeight
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
					s.assignments[i].SavedPhase = true
				} else if posCount[i] > negCount[i] {
					s.assignments[i].SavedPhase = false
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
// bigAdjOff[litIdx]..bigAdjOff[litIdx+1] delimits successors m such that
// binary clause (¬lit ∨ m) exists. Used for BIG-based clause minimization
// (Kissat-style).
func (s *CDCLSolver) buildBIG() {
	numLits := int(s.cnf.NumVars) * 2
	off := make([]int32, numLits+1)
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
		off[a^1+1]++
		off[b^1+1]++
	}
	for i := 1; i <= numLits; i++ {
		off[i] += off[i-1]
	}
	data := make([]int32, off[numLits])
	cur := make([]int32, numLits)
	copy(cur, off[:numLits])
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
		data[cur[a^1]] = int32(b)
		cur[a^1]++
		data[cur[b^1]] = int32(a)
		cur[b^1]++
	}
	s.bigAdjData = data
	s.bigAdjOff = off
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
	if litIdx >= len(s.bigAdjOff)-1 {
		return false
	}
	s.bigBfsEpoch++
	if s.bigBfsEpoch == 0 {
		// Wrap: clear all stamps so stale values can't masquerade as "visited".
		clear(s.bigBfsVisited)
		s.bigBfsEpoch = 1
	}
	ep := s.bigBfsEpoch
	litVar := lit.Var()
	visited := s.bigBfsVisited
	tmpInClause := s.tmpLiteralInClause
	tmpIsNeg := s.tmpLiteralIsNegated
	bigAdjData := s.bigAdjData
	bigAdjOff := s.bigAdjOff

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
		start := int(bigAdjOff[cur])
		end := int(bigAdjOff[cur+1])
		for i := start; i < end; i++ {
			m := int(bigAdjData[i])
			if visited[m] == ep {
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
	if !s.skipClassify {
		s.classifyInstance()
	}
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
	// Branch-quality telemetry: record the effective bump scheme and init mode.
	if s.useBumpAnalyze {
		s.branchBumpScheme = 1
	}
	if s.skipVSIDSInit {
		s.branchInitMode = 0
	} else {
		s.branchInitMode = 1
	}
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level < 0 {
			s.numUnassigned++
		} else {
			v := s.assignments[i].Value
			s.litTrue[i*2] = v
			s.litTrue[i*2+1] = !v
		}
	}

	if flpResult := s.failedLiteralProbing(); flpResult != UNKNOWN {
		s.printStats()
		return flpResult
	}

	// Release the Clauses slice (32B/clause) — all solving-path access now
	// goes through originalClauseLocs + literalPool. Preprocessing modified
	// Clauses directly; the final RebuildLiteralPool (inside preprocessAggressive
	// or SolveWithoutPreprocessing) captured everything into the pool.
	s.cnf.Clauses = nil

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
	// aggressive decay 0.30 + restartBase=20 the classifier prescribes. This made
	// -no-preprocess a polluted diagnostic that conflated "no preprocessing" with
	// "no adaptive tuning". classifyInstance is read-only on s.cnf, so it cannot
	// change the search trajectory the way forced unit propagation does.
	if !s.skipClassify {
		s.classifyInstance()
	}
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
		} else {
			v := s.assignments[i].Value
			s.litTrue[i*2] = v
			s.litTrue[i*2+1] = !v
		}
	}
	// Release the Clauses slice (32B/clause) — solving uses the pool.
	s.cnf.Clauses = nil
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

// verifyModel checks if the current assignment satisfies all clauses.
// Runs on the SAT path inside cdclLoop, AFTER s.cnf.Clauses has been released
// (set to nil), so it validates against the surviving SoA representation
// (originalClauseLocs + literalPool) rather than the (now-nil) Clauses slice —
// otherwise the check would iterate zero clauses and vacuously pass.
// Returns true if model is valid, false otherwise.
func (s *CDCLSolver) verifyModel() bool {
	numOrig := s.cnf.NumOriginalClauses()
	locs := s.cnf.GetOriginalClauseLocs()
	pool := s.cnf.GetLiteralPool()
	s.Log("c [VERIFY] Checking %d clauses...\n", numOrig)
	for ci := 0; ci < numOrig; ci++ {
		loc := locs[ci]
		clause := pool[loc.Offset : loc.Offset+loc.Size]
		clauseSat := false
		for _, lit := range clause {
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
			for _, lit := range clause {
				if lit.IsNegated() {
					s.Log("-%d ", lit.Var()+1)
				} else {
					s.Log("%d ", lit.Var()+1)
				}
				s.Log("(assignments: ")
				for _, lit := range clause {
					v := lit.Var()
					s.Log("var%d={V=%v,L=%d,I=%d} ", v+1, s.assignments[v].Value, s.assignments[v].Level, s.assignments[v].Reason)
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
				// Root-level learned units propagate at s.level (0 after restart).
				// Level 0 is correct: 1-UIP skips level-0 literals (always-true),
				// so they neither pollute the working clause nor appear as
				// candidates. They sit at the front of s.trail (before trailHead[1])
				// and survive cancelUntil(0).
				// Do NOT update trailHead — it tracks decisions only.
				propLevel := s.level
				if s.verbose {
					s.Log("c [UNIT PROP] var=%d, value=%v, level=%d (s.level=%d)\n", varIdx+1, litValue, propLevel, s.level)
				}
				s.assignments[varIdx] = Assignment{Value: litValue, Level: int32(propLevel), Reason: int32(-learnedIdx - 5)}
				s.litTrue[int(varIdx)*2] = litValue
				s.litTrue[int(varIdx)*2+1] = !litValue
				s.trail = append(s.trail, varIdx)
				s.numUnassigned--
				s.propagations++
			} else if s.assignments[varIdx].Value != litValue {
				// Conflict: unit learned clause conflicts with existing assignment
				existingIdx := s.assignments[varIdx].Reason
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

	// Cache watchLists outer slice header. The outer slice is allocated once in
	// initWatches and never grows (length is always 2*numVars). Only inner slices
	// grow via append, and the cached header shares the backing array, so inner-
	// slice updates are visible to s.watchLists automatically. This eliminates
	// the s → s.watchLists pointer chase on every append (930ms) and write-back
	// (370ms) in the hot loop.
	watchLists := s.watchLists

	// litTrue cache: backing array never reallocated, writes through local visible
	// to s.litTrue automatically. litValueBase provides bounds-check-free access
	// via unsafe.Add — the invariant watch.Blit < len(litValue) always holds
	// (Blit = varIdx*2+negated, varIdx < NumVars, litValue has 2*NumVars entries),
	// so the CMPQ+JLS the compiler emits for litValue[watch.Blit] is pure overhead.
	litValue := s.litTrue
	var litValueBase unsafe.Pointer
	if len(litValue) > 0 {
		litValueBase = unsafe.Pointer(&litValue[0])
	}
	// Cache scalar counters as locals — incremented/decremented on every propagation,
	// writing through s pointer each time. Write back at returns.
	propagations := s.propagations
	numUnassigned := s.numUnassigned

	// Cache the trail slice header. trail grows via append below; the cached
	// header must be written back to s.trail at every return so subsequent
	// calls see the grown trail. Same aliasing rationale as assignments above.
	// Also cache s.level (constant for the whole call — no decisions/conflicts
	// occur inside this loop) and derive propLevel once. Root-level propagations
	// (s.level==0) get Level 0: 1-UIP correctly skips them as always-true.
	trail := s.trail
	level := s.level
	propLevel := level

	// A1: Slow-path-only slice headers (originalClauseLocs, originalLiteralPool,
	// originalSearchHint, learnedLoc, learnedLiterals, learnedSearchHint) are
	// NOT cached here. They are loaded on demand inside the slow path (after the
	// blit check fails). This prevents the compiler from carrying 6 slice headers
	// (18 words) across the fast-path inner loop, which was forcing the
	// fast-path-critical values (litValueBase, watchList, readIdx) to spill to
	// the stack on every iteration.

	for trailIndex := s.qhead; trailIndex < len(trail); trailIndex++ {
		lit := trail[trailIndex]

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
			// unsafe.Add skips the bounds check — the invariant Blit < len(litValue)
			// always holds (Blit = varIdx*2+negated, varIdx < NumVars).
			if *(*bool)(unsafe.Add(litValueBase, watch.Blit)) {
				continue
			}

			// SLOW PATH: Blocking literal is not true (or unassigned).
			// Access clause data for replacement search / conflict detection.
			//
			// A1: Slow-path-only slice headers are loaded here (after the blit
			// check fails), not at function entry. This keeps the fast-path inner
			// loop free of 6 extra slice headers (18 words) that would otherwise
			// force litValueBase/watchList/readIdx to spill to the stack.
			originalClauseLocs := s.cnf.GetOriginalClauseLocs()
			originalLiteralPool := s.cnf.GetLiteralPool()
			numOriginalClauses := len(originalClauseLocs)
			originalSearchHint := s.originalSearchHint
			learnedLoc := s.learnedLoc
			learnedLiterals := s.learnedLiterals
			learnedSearchHint := s.learnedSearchHint

			// Decode ClauseIdx once (was decoded 5+ times per watch iteration).
			// Bit 31: learned flag, bit 30: myPos, bit 29: binary, bits 0-28: clause index.
			clauseIdxRaw := uint32(watch.ClauseIdx)
			isLearned := clauseIdxRaw&watchLearnedBit != 0
			clauseID := int(clauseIdxRaw & watchIdxMask)
			myPos := int((clauseIdxRaw >> 30) & 1)
			blitPos := 1 - myPos

			// BINARY FAST PATH: For size-2 clauses, the Blit field already has
			// the blocking literal. No replacement search is possible (only 2
			// literals), so we skip the clause data load entirely and go straight
			// to propagate/conflict.
			if clauseIdxRaw&watchBinaryBit != 0 {
				blitVarIdx := int(watch.Blit >> 1)
				blitNegated := watch.Blit&1 == 1
				blitAsg := assignments[blitVarIdx]

				if blitAsg.Level < 0 {
					// Unassigned — propagate the blocking literal
					reasonIdx := clauseID
					if isLearned {
						reasonIdx = -clauseID - 5
					}
					blitValue := !blitNegated
					assignments[blitVarIdx] = Assignment{Value: blitValue, Level: int32(propLevel), Reason: int32(reasonIdx), SavedPhase: blitNegated}
					litValue[blitVarIdx*2] = blitValue
					litValue[blitVarIdx*2+1] = !blitValue
					trail = append(trail, uint32(blitVarIdx))
					numUnassigned--
					propagations++
					continue
				}

				// Assigned — check for conflict (blit is false since fast path
				// already filtered out the true case)
				if level == 0 {
					s.emptyClauseFound = true
				}
				// Build conflict clause (rare path — clause data load OK here)
				if !isLearned {
					s.conflictClauseBuf.Literals = originalLiteralPool[int(originalClauseLocs[clauseID].Offset) : int(originalClauseLocs[clauseID].Offset)+int(originalClauseLocs[clauseID].Size)]
					s.conflictClauseBuf.Learned = false
				} else {
					loc := learnedLoc[clauseID]
					s.conflictLitsBuf = s.conflictLitsBuf[:0]
					s.conflictLitsBuf = append(s.conflictLitsBuf, learnedLiterals[int(loc.Offset):int(loc.Offset)+int(loc.Size)]...)
					s.conflictClauseBuf.Literals = s.conflictLitsBuf
					s.conflictClauseBuf.Learned = true
				}
				s.propagations = propagations
				s.numUnassigned = numUnassigned
				s.trail = trail
				return true, &s.conflictClauseBuf
			}

			var clauseLits []cnf.Literal
			if !isLearned {
				if clauseID >= numOriginalClauses {
					continue
				}
				loc := originalClauseLocs[clauseID]
				clauseLits = originalLiteralPool[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
			} else {
				loc := learnedLoc[clauseID]
				if int(loc.Size) == 0 {
					continue
				}
				clauseLits = learnedLiterals[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
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
				if clauseID < len(learnedSearchHint) {
					hint = learnedSearchHint[clauseID]
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
					if clauseID < len(learnedSearchHint) {
						learnedSearchHint[clauseID] = int32(foundJ)
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
				if blitAsg.Reason == -1 {
					reasonIdx := clauseID
					if isLearned {
						reasonIdx = -clauseID - 5
					}
					blitValue := !blitNegated
					assignments[blitVarIdx] = Assignment{Value: blitValue, Level: int32(propLevel), Reason: int32(reasonIdx), SavedPhase: blitNegated}
					litValue[blitVarIdx*2] = blitValue
					litValue[blitVarIdx*2+1] = !blitValue
					trail = append(trail, uint32(blitVarIdx))
					numUnassigned--
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
					loc := originalClauseLocs[clauseID]
					s.conflictClauseBuf.Literals = originalLiteralPool[loc.Offset : loc.Offset+loc.Size]
					s.conflictClauseBuf.Learned = false
					conflictClause = &s.conflictClauseBuf
				} else {
					literals := s.getLearnedClauseLiterals(clauseID)
					s.conflictLitsBuf = s.conflictLitsBuf[:0]
					s.conflictLitsBuf = append(s.conflictLitsBuf, literals...)
					s.conflictClauseBuf.Literals = s.conflictLitsBuf
					s.conflictClauseBuf.Learned = true
					conflictClause = &s.conflictClauseBuf
				}

				if level == 0 {
					s.emptyClauseFound = true
				}
				if s.verbose {
					s.Log("c [PROP CONFLICT] Watch idx=%d, clauseIdx=%d, level=%d\n",
						watchIdx, clauseID, level)
				}
				s.propagations = propagations
				s.numUnassigned = numUnassigned
				s.trail = trail
				return true, conflictClause
			}
		}
	}

	// Update qhead to end of trail
	s.qhead = len(trail)
	s.trail = trail
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
	varIdx, phase = s.vsids.selectVariableWithPhase(s.assignments)

	// SAFETY CHECK: Ensure variable is unassigned before deciding.
	// A stale heap entry can slip through; fall back to linear scan.
	if int(varIdx) < len(s.assignments) && s.assignments[varIdx].Level >= 0 {
		found := false
		for i := uint32(0); i < s.cnf.NumVars; i++ {
			if s.assignments[i].Level < 0 {
				varIdx = i
				if int(varIdx) < len(s.assignments) {
					phase = s.assignments[varIdx].SavedPhase
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
		Value:      value,
		Level:      int32(level),
		Reason:     int32(clauseIdx),
		SavedPhase: lit.IsNegated(),
	}
	s.litTrue[int(varIdx)*2] = value
	s.litTrue[int(varIdx)*2+1] = !value
	s.trail = append(s.trail, varIdx)
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

	// CRITICAL FIX: Check Reason instead of Level != 0
	// Level 0 can mean both "unassigned" AND "assigned at level 0" after backtracking
	// Reason != -1 properly indicates the variable is already assigned
	if s.assignments[varIdx].Reason != -1 {
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value:      value,
		Level:      int32(level),
		Reason:     int32(clauseIdx),
		SavedPhase: lit.IsNegated(),
	}
	s.litTrue[int(varIdx)*2] = value
	s.litTrue[int(varIdx)*2+1] = !value
	s.trail = append(s.trail, varIdx)
	s.numUnassigned--
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
				impIdx := s.assignments[varIdx].Reason
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
						if impIdx >= 0 {
							locs := s.cnf.GetOriginalClauseLocs()
							if int(impIdx) < len(locs) && locs[impIdx].Size == 1 {
								isUnit = true
							}
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

	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	// B2: Adaptively shrink clause DB when average LBD is consistently high.
	// High-LBD clauses rarely propagate; a smaller DB keeps only the lowest-LBD
	// clauses. One-way shrink (don't grow back) to avoid oscillation.
	// Gated on structured instances only: random k-SAT naturally has high LBD
	// (unstructured conflicts), so shrinking there cripples the DB.
	if !s.maxLearnedShrunk && s.structureScore >= 0.75 && s.totalLbdCount > 1000 {
		avgLbd := s.totalLbdSum / s.totalLbdCount
		if int(avgLbd) > s.dbShrinkThreshold {
			newFloor := int(s.cnf.NumVars) * s.dbShrinkFloorMultiplier
			if newFloor < 300 {
				newFloor = 300
			}
			if s.maxLearned > newFloor {
				s.maxLearned = newFloor
				s.Log("c [clause-db] Shrunk maxLearned to %d (avg LBD=%d > %d)\n", s.maxLearned, avgLbd, s.dbShrinkThreshold)
			}
			s.maxLearnedShrunk = true
		}
	}

	// Delete learned clauses when database exceeds dynamic limit
	// Formula: base + conflicts/growthDivisor, trigger at deletionTriggerRatio × limit
	dynamicLimit := s.maxLearned + s.conflicts/s.dbGrowthDivisor
	if float64(s.learnedActiveCount) > float64(dynamicLimit)*s.deletionTriggerRatio {
		s.deleteLearnedClauses()
	}

	// Decay VSIDS activity every conflict (standard)
	s.vsids.decay(s.assignments)
	s.vsids.decayLBD()
	// Clause activity decay (O(1), mirrors VSIDS varInc trick). Only consults
	// Activity at deletion time, so no heap to invalidate. No-op when disabled.
	s.claDecay()
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
// runOneUIPResolution performs 1-UIP conflict analysis: it marks the conflict
// clause literals, then resolves on candidates from the current decision level
// until exactly one literal (the UIP) remains. If resolution does not converge
// (inconsistent reason clauses), a fallback forces the most recent literal as
// the UIP. Returns currentCount (number of literals at the current level after
// resolution; 0 = non-asserting, 1 = asserting).
func (s *CDCLSolver) runOneUIPResolution(conflictLits []cnf.Literal) (currentCount int) {
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
	currentCount = s.tmpLevelCount[s.level]

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

		reasonClauseIdx := s.assignments[varIdx].Reason
		// Clause-activity bump: each learned reason clause traversed during
		// 1-UIP resolution gets claInc added to its Activity (MiniSat
		// claBumpEvent equivalent). Only learned clauses (impIdx <= -5) are
		// bumped; original clauses and preprocessing sentinels (-2..-4) are
		// skipped. Reason clauses are protected from deletion while in use,
		// so the bump only matters once the clause becomes a candidate. When
		// claActivityEnabled=false this is a no-op (Activity stays 0).
		if s.claActivityEnabled && reasonClauseIdx <= -5 {
			s.learnedMetadata[-reasonClauseIdx-5].Activity += s.claInc
		}
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
			if s.assignments[varIdx].Reason == -1 {
				decisionsAtCurrentLevel++
			} else {
				propagationsAtCurrentLevel++
			}
		}
	}

	if currentCount > 1 {
		s.uipFallbackCount++
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

		// Find the most recent literal at current level (this will be the UIP).
		// Build trailPos for the current-level trail slice (O(current-level trail))
		// for O(1) position lookup, avoiding O(touched × total trail) scanning.
		startIdx := s.trailHead[s.level]
		for ti := startIdx; ti < len(s.trail); ti++ {
			s.trailPos[s.trail[ti]] = ti
		}

		// Diagnostic: dump residual-literal state before fallback selection.
		// trailPos is currently populated for the current-level slice.
		s.logUIPFallback(decisionsAtCurrentLevel, propagationsAtCurrentLevel, startIdx)

		var uipVar uint32 = 0
		var uipTrailPos int = -1
		for _, varIdx := range s.tmpTouchedVars {
			if s.tmpLiteralInClause[varIdx] && s.assignments[varIdx].Level == int32(s.level) {
				ti := s.trailPos[varIdx]
				if ti >= startIdx && (uipTrailPos < 0 || ti > uipTrailPos) {
					uipTrailPos = ti
					uipVar = varIdx
				}
			}
		}

		// Clear trailPos for the current-level trail slice
		for ti := startIdx; ti < len(s.trail); ti++ {
			s.trailPos[s.trail[ti]] = 0
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
	return currentCount
}

// uipFallbackLogCap bounds per-solve diagnostic output from logUIPFallback.
// Fallback is rare (<0.01% of conflicts), but uncapped logging on a long run
// could still produce excessive output.
const uipFallbackLogCap = 32

// logUIPFallback dumps diagnostic state when 1-UIP resolution fails to
// converge. Output goes to stderr regardless of s.verbose (fallback is rare
// enough that the noise is bounded by uipFallbackLogCap per solve, but turning
// on full -verbose to catch it is impractical at 100K+ conflicts).
// startIdx is the trail index where the current decision level begins
// (trailHead[s.level]); trailPos must already be populated for
// [startIdx, len(trail)). decisionsAtCurrentLevel/propagationsAtCurrentLevel
// are precomputed by the caller.
func (s *CDCLSolver) logUIPFallback(decisionsAtCurrentLevel, propagationsAtCurrentLevel, startIdx int) {
	if s.uipFallbackLogged >= uipFallbackLogCap {
		return
	}
	s.uipFallbackLogged++
	fmt.Fprintf(os.Stderr, "c [uip-fallback] conflict=%d level=%d trailLen=%d trailHead=%d decisions=%d propagations=%d\n",
		s.conflicts, s.level, len(s.trail), startIdx, decisionsAtCurrentLevel, propagationsAtCurrentLevel)
	for _, v := range s.tmpTouchedVars {
		if !s.tmpLiteralInClause[v] || s.assignments[v].Level != int32(s.level) {
			continue
		}
		reasonIdx := s.assignments[v].Reason
		vDimacs := int32(v) + 1
		if s.tmpLiteralIsNegated[v] {
			vDimacs = -vDimacs
		}
		if reasonIdx == -1 {
			fmt.Fprintf(os.Stderr, "c   [residual] lit=%d trailPos=%d reason=DECISION resolved=%v\n",
				vDimacs, s.trailPos[v], s.tmpResolved[v])
			continue
		}
		reasonLits := s.getReasonLitsForVar(v)
		if reasonLits == nil {
			fmt.Fprintf(os.Stderr, "c   [residual] lit=%d trailPos=%d reason=DANGLING(%d) resolved=%v\n",
				vDimacs, s.trailPos[v], reasonIdx, s.tmpResolved[v])
			continue
		}
		fmt.Fprintf(os.Stderr, "c   [residual] lit=%d trailPos=%d reason=%d resolved=%v assign(L%d,R%d,V%v) reasonLits=",
			vDimacs, s.trailPos[v], reasonIdx, s.tmpResolved[v],
			s.assignments[v].Level, s.assignments[v].Reason, s.assignments[v].Value)
		for _, rl := range reasonLits {
			rlVar := rl.Var()
			rlLevel := -1
			if int(rlVar) < len(s.assignments) {
				rlLevel = int(s.assignments[rlVar].Level)
			}
			fmt.Fprintf(os.Stderr, "%d@%d ", rl.ToDimacs(), rlLevel)
		}
		fmt.Fprintf(os.Stderr, "\n")
		// Full-trail scan to find the ACTUAL trail position of v (or -1 if absent).
		// O(trailLen) per residual, but only runs in the rare fallback path (capped).
		actualPos := -1
		for ti, tv := range s.trail {
			if tv == v {
				actualPos = ti
				break
			}
		}
		fmt.Fprintf(os.Stderr, "c             actualTrailPos=%d", actualPos)
		if actualPos >= 0 {
			lvlAtPos := -1
			if actualPos < len(s.trail) {
				lvlAtPos = int(s.assignments[s.trail[actualPos]].Level)
			}
			fmt.Fprintf(os.Stderr, " levelAtActualPos=%d", lvlAtPos)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
}

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

	currentCount := s.runOneUIPResolution(conflictLits)

	// Bump variables touched during 1-UIP analysis. bumpAnalyze (minisat
	// analyze_toclear) bumps ALL touched variables including intermediate
	// resolved vars; bumpClause bumps only the conflict clause vars. bumpAnalyze
	// is gated to default-decay instances — under aggressive decay (0.30→0.60)
	// the fast varInc growth flattens the VSIDS signal when distributed across
	// many touched vars (30eb4ef4 regression). Must run before the currentCount==0
	// early return so degenerate conflicts still bump involved variables.
	if s.useBumpAnalyze {
		s.vsids.bumpAnalyze(s.tmpTouchedVars)
	} else {
		s.vsids.bumpClause(conflictLits)
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

	// Build learned clause — exclude level-0 literals (always-true root facts).
	// Level-0 literals are preprocessing assignments and root-level learned units.
	// They are always true during search, so including them in learned clauses
	// wastes storage and watch slots without adding constraint value. The old
	// propLevel=1 hack resolved root-level propagated units away via 1-UIP
	// (their reason was a unit clause, so resolution removed them and added
	// nothing); Level 0 skips them, so we filter here to produce equally short
	// clauses.
	s.tmpLearnedLits = s.tmpLearnedLits[:0]
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = false
			if s.assignments[varIdx].Level > 0 {
				s.tmpLearnedLits = append(s.tmpLearnedLits, cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx]))
			}
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
	// Glue clause (LBD ≤ 2) counter for runtime decay adaptation. See
	// maybeAdaptDecay: structured instances produce many glues (tight
	// implication chains → low LBD); random instances produce almost none.
	// The ratio is the canonical structured-vs-random behavioral signal.
	if lbd <= 2 {
		s.glueLearned++
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
		// Check if an opposite unit already exists among learned units.
		// unitLearnedList is maintained at store/delete/compact/vivify/restart,
		// so it's current here. This replaces the prior O(learnedCapacity)
		// scan over all learned clauses.
		for _, existingIdx := range s.unitLearnedList {
			existingLits := s.getLearnedClauseLiterals(existingIdx)
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

	if !s.storeLearnedClause(lbd) {
		return 0
	}

	return backjumpLevel
}

// storeLearnedClause stores the learned clause in the database, sets up watches,
// and bumps VSIDS. Returns false if a duplicate-literal soundness bug was
// detected (caller returns backjump level 0). Returns true on success or
// when there is nothing to store (empty tmpLearnedLits).
func (s *CDCLSolver) storeLearnedClause(lbd int) bool {
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
			return false
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
		s.recordLearnedClauseSize(len(s.tmpLearnedLits))
		// Fresh clauses start at Activity=0 (MiniSat convention). They only
		// gain activity when used as reasons during 1-UIP resolution. With
		// Activity=0, the sort's index tiebreak handles fresh-clause
		// protection (they have high indices, deleted last within Activity=0
		// tier = FIFO protection). When claActivityEnabled=false, Activity
		// stays 0 (pure FIFO fallback).
		s.learnedMetadata = append(s.learnedMetadata, cnf.ClauseMetadata{
			LBD: int32(lbd),
		})
		s.learnedSearchHint = append(s.learnedSearchHint, 0) // Fresh clause: no hint yet
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
				if len(literals) == 2 {
					clauseIdx0 |= int32(watchBinaryBit)
					clauseIdx1 |= int32(watchBinaryBit)
				}
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
	return true
}

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
		if s.assignments[v].Reason == -1 {
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
		if s.bigAdjOff != nil && !(s.structureScore < 0.7 && s.binaryRatio > 0.4) { // GATE: disable BIG for mixed-binary sub-0.7
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
		if s.assignments[rv].Reason == -1 {
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

// markProtectedClauses returns a bitmap over [0, learnedCapacity) flagging
// learned clauses currently used as reasons (implication sources). Such
// clauses must not be vivified, subsumed, or deleted while in use. The bitmap
// is backed by s.tmpClauseUsedAsReason and is zeroed before marking.
func (s *CDCLSolver) markProtectedClauses() []bool {
	if cap(s.tmpClauseUsedAsReason) < s.learnedCapacity {
		s.tmpClauseUsedAsReason = make([]bool, s.learnedCapacity)
	}
	protected := s.tmpClauseUsedAsReason[:s.learnedCapacity]
	for i := range protected {
		protected[i] = false
	}
	for _, asg := range s.assignments {
		impIdx := asg.Reason
		if impIdx <= -5 {
			learnedIdx := -impIdx - 5
			if int(learnedIdx) < s.learnedCapacity {
				protected[learnedIdx] = true
			}
		}
	}
	return protected
}

// sortDeletionCandidates sorts learned-clause deletion candidates in place by
// ascending activity (lowest quality first), with ascending clause index as the
// FIFO tiebreak (oldest learned first). See deleteLearnedClauses.
func (s *CDCLSolver) sortDeletionCandidates(candidates []int) {
	sort.Slice(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		aiAct := s.learnedMetadata[ai].Activity
		ajAct := s.learnedMetadata[aj].Activity
		if aiAct != ajAct {
			return aiAct < ajAct
		}
		return ai < aj
	})
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
// Deletion trigger: when learnedActiveCount > deletionTriggerRatio × dynamicLimit
// (dynamicLimit = maxLearned + conflicts/dbGrowthDivisor). Target after deletion: dynamicLimit.
func (s *CDCLSolver) deleteLearnedClauses() {
	// LAZY LBD-BASED DELETION (Glucose-style)
	// Key insight: LBD is the best predictor of clause usefulness
	// - Keep all "glue" clauses (LBD ≤ 2) permanently
	// - Delete clauses with high LBD when database grows too large

	dynamicLimit := s.maxLearned + s.conflicts/s.dbGrowthDivisor
	targetCount := dynamicLimit

	currentActive := s.learnedActiveCount

	toDelete := currentActive - targetCount
	if toDelete <= 0 {
		return // Nothing to delete
	}

	// Mark protected clauses (used as implications) using bitmap
	// This avoids O(n*m) scanning
	protected := s.markProtectedClauses()

	// Use tmp buffer for deletion marks
	if cap(s.tmpDeleted) < s.learnedCapacity {
		s.tmpDeleted = make([]bool, s.learnedCapacity)
	}
	deleted := s.tmpDeleted[:s.learnedCapacity]
	for i := range deleted {
		deleted[i] = false
	}

	deletedCount := 0

	// Two-pass LBD-tiered deletion. Within each tier, candidates are sorted by
	// Activity ascending (lowest = delete first) with FIFO index tiebreak, so
	// the lowest-activity clauses are deleted first. Glue clauses (LBD ≤ 2)
	// are never deleted. When claActivityEnabled=false all Activity fields
	// are 0 and the index tieback reduces the sort to pure ascending index
	// order = FIFO (identical to pre-activity behavior).
	candidates := s.tmpDeletionCandidates[:0]

	// Pass 1: collect LBD > tier1Threshold candidates (lowest quality), delete lowest-activity first
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size == 0 || protected[i] {
			continue
		}
		if int(s.learnedMetadata[i].LBD) > s.lbdTier1Threshold {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) > 1 {
		s.sortDeletionCandidates(candidates)
	}
	for _, idx := range candidates {
		if deletedCount >= toDelete {
			break
		}
		deleted[idx] = true
		deletedCount++
	}

	// Pass 2: lower threshold to LBD > tier2Threshold (glue clauses are LBD ≤ tier2Threshold, never deleted)
	if deletedCount < toDelete {
		candidates = candidates[:0]
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedLoc[i].Size == 0 || protected[i] || deleted[i] {
				continue
			}
			if int(s.learnedMetadata[i].LBD) > s.lbdTier2Threshold {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) > 1 {
			s.sortDeletionCandidates(candidates)
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
		s.learnedMetadata[writeIdx] = s.learnedMetadata[readIdx]
		s.learnedSearchHint[writeIdx] = s.learnedSearchHint[readIdx]
		s.learnedWatchIdx0[writeIdx] = s.learnedWatchIdx0[readIdx]
		s.learnedWatchIdx1[writeIdx] = s.learnedWatchIdx1[readIdx]

		// Copy literals
		copy(s.learnedLiterals[newStart:newStart+oldSize], s.learnedLiterals[oldStart:oldStart+oldSize])

		writeIdx++
		nextOffset += oldSize
	}

	// Update Reason fields using the mapping
	for varIdx := range s.assignments {
		if s.assignments[varIdx].Reason <= -5 {
			learnedIdx := -s.assignments[varIdx].Reason - 5
			if int(learnedIdx) < len(clauseIndexMap) && clauseIndexMap[learnedIdx] >= 0 {
				s.assignments[varIdx].Reason = int32(-clauseIndexMap[learnedIdx] - 5)
			} else if int(learnedIdx) < len(clauseIndexMap) {
				// Clause was deleted - reset to decision
				s.assignments[varIdx].Reason = -1
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
		if len(literals) == 2 {
			clauseIdx0 |= int32(watchBinaryBit)
			clauseIdx1 |= int32(watchBinaryBit)
		}

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
		s.learnedSearchHint[i] = 0
	}

	// Mark watches as initialized
	s.watchInitialized = true

	// Truncate arrays to new capacity
	s.learnedLiterals = s.learnedLiterals[:nextOffset]
	s.learnedLoc = s.learnedLoc[:writeIdx]
	s.learnedMetadata = s.learnedMetadata[:writeIdx]
	s.learnedSearchHint = s.learnedSearchHint[:writeIdx]
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
		s.unassignVar(varIdx)
		s.vsids.onUnassign(varIdx)
	}
	s.trail = s.trail[:decisionPoint]
	s.qhead = decisionPoint
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel

	return true
}
