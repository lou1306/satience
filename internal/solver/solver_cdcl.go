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
	//
	// myPos is packed ONLY for general (size>=3) clauses. Binary (size==2) watches
	// live in a structurally separate region (watchListsBinary) and their slow path
	// never reads myPos, so it is not packed for them. There is no binary bit: the
	// region a clause belongs to is immutable and is carried by which watch list
	// holds it, not by a per-watch flag.
	watchLearnedBit uint32 = 0x80000000
	watchMyPosBit   uint32 = 0x40000000
	watchIdxMask    uint32 = 0x1FFFFFFF
	watchMyPosMask  uint32 = 0x9FFFFFFF // bits 0-28 + bit 31 (clears myPos for identity comparison)
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
	// Ceiling on the learned-clause database. 100K+ learned clauses
	// make the general (long-clause) watch tier so large that propagation
	// rescans billions of literals (8202af80: 5.28B, de2b584ee: 2.08B) and
	// dominates wall time. Capping at 30000 shrinks the general tier and lets
	// watch propagation stay O(1); the asserting clause is still always added
	// each conflict so search never stalls. Measured on the 72-instance suite:
	// PAR2 4.35s→3.17s, solve 94.4%→97.2%, TMO 4→2 (8202af80 100s→~17s,
	// de2b584ee ~32s→~15s), with no regression in any previously-solved
	// instance.
	if baseLimit > 30000 {
		baseLimit = 30000
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
	cnf              *cnf.CNF
	assignments      []Assignment
	trail            []uint32
	trailHead        []int
	trailPos         []int // trailPos[varIdx] = position in trail (-1 if not on trail); O(1) lookup for 1-UIP fallback
	level            int
	vsids            *VSIDS
	numUnassigned    int           // Count of unassigned variables (O(1) allAssigned/hasUnassigned)
	watchLists       [][]cnf.Watch // watchLists[lit] = general (size>=3) clauses watching lit
	watchListsBinary [][]cnf.Watch // watchListsBinary[lit] = binary (size==2) clauses watching lit
	qhead            int           // Watched literals: next trail index to process
	litTrue          []bool        // Cached assigned-and-true bitmap (varIdx*2 + negated); blit fast path reads this instead of decoding literal + loading assignments[]

	// ===== WARM FIELDS (per-conflict / per-restart) =====
	preprocessTrail []uint32 // Permanent preprocessing assignments (Level 0, never cleared/backtracked)
	probeTrail      []int    // Reusable trail for FLP non-watch BCP (literal indices)
	conflicts       int
	iterations      int
	propagations    int    // Total propagations (assignments by unit propagation)
	numWatchMoves   uint64 // Instrumentation: count of watch replacement moves in propagateWatched (diagnostic/witness)
	// Tier counters for the propagation fast/slow split (diagnostic). Count how
	// many watch visits /entries each tier handles per solve, to attribute wall
	// time and measure the value of the split without profiling:
	//   blitFastHits  - watch skipped because blocking literal is already true (fast path)
	//   binarySlow    - non-satisfied size-2 watch handled without clause-data load
	//   generalSlow   - non-satisfied non-binary watch (replacement scan / conflict)
	blitFastHits uint64
	binarySlow   uint64
	generalSlow  uint64
	generalMoves uint64 // of generalSlow, the count that end in a replacement-move (vs propagate/conflict)
	// F-measure diagnostics: attribute the move-dominated generalSlow tier.
	//   moveReallocCast   - moves whose destination watch list hit cap (append grew it)
	//   moveHintMiss      - moves where the cached search hint missed and a full scan ran
	//   moveScanLits      - total literals scanned across all post-miss replacement scans
	moveReallocCast uint64
	moveHintMiss    uint64
	moveScanLits    uint64
	// preferTrueCap bounds how far the replacement scan hunts for a currently-TRUE
	// literal before falling back to the first unassigned one, restricted to
	// LEARNED clauses (original clauses keep the first-non-false policy). A true
	// watch is "parked" (satisfied, stable until backtrack) so preferring it
	// reduces watch-move churn; the cap prevents the prefer-true hunt from
	// scanning a long clause end-to-end. Default 0 = old behavior (first
	// non-false, no prefer-true on any clause): a hard-rail DEV sweep
	// (randkcnf150/200, rand4 100, tseitin grid17/gnd80, parity off) showed no
	// net win at any cap (cap=8 even adds a new TMO), so it is off by default and
	// only enabled via the -prefer-true-cap CLI flag.
	preferTrueCap      int
	originalSearchHint []int32 // Per-original-clause search hint for replacement scan (0=no hint)
	learnedSearchHint  []int32 // Per-learned-clause search hint for replacement scan (0=no hint) — hot path
	maxIter            int
	decisions          int
	uipFallbackCount   int  // Number of times 1-UIP resolution didn't converge (diagnostic)
	debugCC            bool // SAT_DEBUG_CC: assert unit-clause completeness on every propagate
	uipFallbackLogged  int  // Counter for capping per-solve fallback diagnostic output (diagnostic)
	backjumpLevel      int
	maxLearned         int
	restartCount       int
	lubyIndex          int
	// Restart-reason attribution counters (instrumentation). Which mechanism
	// triggered each restart: Luby fallback, Glucose LBD restarts, props/dec
	// bounded, level-capped, or MiniSat geometric. Used to diagnose where
	// search time goes (flat-high-LBD instances never fire Glucose, so the
	// Luby fallback dominates).
	restartReasons      [5]int // [0]=glucose, [1]=luby, [2]=propsdec, [3]=levelcap, [4]=geometric
	restartSegStartConf int    // conflicts at the start of the current restart segment
	restartSegStartDec  int    // decisions at the start of the current restart segment
	restartSegGlueCount int    // glue clauses learned in the current segment
	restartSegStartGlue int    // glueLearned at the start of the current segment
	// Branch-quality telemetry (instrumentation): which VSIDS bump scheme and
	// init mode were actually in effect. 1=analyze_toclear (bump all touched),
	// 0=bumpClause only; initMode 1=clause/occurrence-weighted, 0=zero-init.
	branchBumpScheme int
	branchInitMode   int
	lbdSum           int
	lbdCount         int
	emaLBD           float64 // Exponential moving average of LBD (smooth restart signal)
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
	bigAdjOff  []int64
	// bigBfsBudget bounds the per-call transitive-reduction BFS node budget in
	// bigReachableInClause. Larger budgets find more multi-hop binary reductions
	// at higher per-minimize cost. Exposed for A/B (see -big-bfs).
	bigBfsBudget int
	// Adaptive BIG hit-rate gate (P1a): random k-SAT instances invoke BIG's
	// per-conflict transitive-reduction BFS millions of times (e.g. 1.09M on
	// r3_250) but never remove a literal (0 hits) — pure per-conflict cost. Once
	// bigMinimizeCalls passes bigHitWindow with bigMinimizeHits==0, we disable
	// BIG for the rest of the solve. Because a 0-hit BIG removes no literal, no
	// learned clause changes, so the disabled search is trajectory-identical to
	// the enabled one — this is a pure per-conflict-cost win, distinct from the
	// (retired) watch trajectory thread.
	bigDisabled   bool    // adaptive gate fired: skip BIG minimize + learned-binary maintenance
	bigHitWindow  int64   // sliding-window length (ring size): 0 = never disable
	bigMinHitRate float64 // disable BIG when recent (sliding-window) hit rate falls below this (fraction)
	// Sliding-window ring tracks the outcome of the most recent bigHitWindow BIG
	// attempts so the gate observes hit RATE rather than the historical cumulative
	// 0-hit signal. Scattered hits no longer keep BIG alive forever on a low-yield
	// instance (old gate: only disabled when TOTAL hits == 0, so a 0.5% hit rate
	// never fired and BIG ran at ~99.5% BFS overhead for the whole solve).
	bigWinRing []bool // outcome of each recent BIG attempt (nil until first use)
	bigWinPtr  int    // next ring write position
	bigWinHits int    // #hits currently present in the ring
	bigWinFull bool   // ring has been fully populated at least once

	// bigLearnAdj augments the static original-binary BIG with implication
	// edges from LEARNED binary clauses. Learned clauses are permanent logical
	// consequences of the formula, so edges are never removed after addition
	// (even if the clause is later deleted) — resolution over them is always
	// sound. Per-literal growable slices; bigLearnEdgeCount/cap bound memory.
	bigLearnAdj       [][]int32
	bigLearnEdgeCount int
	bigLearnCap       int
	bigLearnEnabled   bool
	bigLearnFlag      bool   // off by default: enabling shifts search trajectory (net wall-time regression on the suite)
	bigLearnAdded     uint64 // diagnostic: learned-binary edges added
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

	// Reusable buffer for vivification results (avoid per-round allocation)
	tmpVivifyResults []vivifyResult
	// Reusable buffer for the clause being vivified (defensive copy so trial
	// propagation's watch-swaps on the learned-clause pool cannot corrupt the
	// vivify iteration). Avoids a per-clause allocation on the cold path.
	tmpVivifySrc []cnf.Literal

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
	explicitFlags      map[string]bool
	// Clause DB deletion thresholds (Tier 1 tunables). These control which
	// learned clauses are deleted and when. Sweeping them matters because the
	// level-0 literal filter changed clause DB composition.
	lbdTier1Threshold          int     // Pass 1 deletion: delete LBD > threshold (default 5)
	lbdTier2Threshold          int     // Pass 2 deletion: delete LBD > threshold (default 2; glue ≤ threshold never deleted)
	dbGrowthDivisor            int     // dynamicLimit = maxLearned + conflicts/divisor (default 50)
	deletionTriggerRatio       float64 // Trigger deletion when activeCount > ratio × dynamicLimit (default 1.5)
	dbShrinkThreshold          int     // Shrink maxLearned when avgLBD > threshold (default 10)
	dbShrinkFloorMultiplier    int     // Shrink floor = numVars × multiplier (default 3)
	dbMaxLen                   int     // reduceDB: clauses strictly longer than this get eviction priority (0 = disabled)
	restartBase                int     // Luby restart base (default 200; classifier may override)
	restartPropsDecLimit       int     // Props/dec threshold for restart (0=disabled, default 100)
	adaptPropDecLimit          int     // Tier-2 low props/dec threshold for deep-search escape restart
	adaptPropDecDeepGate       int     // Tier-2 gate: deep-search escape fires when conflict level exceeds this (0=disabled)
	adaptivePhaseFlipRate      float64 // Phase flip rate when props/dec is high (0=disabled, default 0.1)
	propsDecRestartGap         int     // Min conflicts between props/dec-bounded restarts (default 100)
	restartLevelCap            int     // Force restart when conflict level exceeds this on long-clause instances (0=disabled)
	levelRestartGap            int     // Min conflicts between level-capped restarts (anti-thrashing)
	minimizeMaxDepth           int     // Max recursion depth for recursive clause minimization (default 0=unlimited)
	minimizeLBDGate            int     // Skip BIG+recursive minimization for learned clauses with pre-minimize LBD above this (0=always minimize)
	unitPropBudget             int     // Max literal visits for unit propagation preprocess (0=unlimited)
	dbCapFactor                float64 // Multiplier on the learned-DB deletion target (1.0 = baseline/indexical)
	veBudget                   int     // Max resolvents for variable elimination (0=unlimited)
	subsumptionBudget          int     // Max clause-pair comparisons in subsumptionPass (0=unlimited)
	vivifyPeriod               int     // Master vivification switch (>0 enabled; cadence is conflict-based)
	vivifyMinConflictGap       int     // Conflict-based vivify cadence: min conflicts between rounds. Default high so vivify fires rarely (recovering pre-decouple protective behavior)
	conflictsAtLastVivify      int     // conflict count at last vivify round (for conflict-cadence gate)
	vivifyEnabled              bool    // Whether vivification is enabled (adaptive: structured instances only)
	subsumptionPeriod          int     // Run subsumption every Nth restart (0=disabled, default 100)
	subsumptionPeriodSet       bool    // True if SetSubsumptionPeriod was called (skip adaptive override)
	subsumptionMinConflictGap  int     // Min conflicts between subsumption rounds (0=restart-based only)
	conflictsAtLastSubsumption int     // conflict count at last subsumption round (for gap gate)
	inprocessPeriod            int     // Master in-processing switch + initial conflict gap (0=off); cadence is adaptive
	inprocessBudget            int     // BVE resolvent budget per in-processing round (0 = unlimited)
	inprocessMinYield          int     // Per-round yield (subsumed+strengthened+eliminated) below which the adaptive cadence backs off
	inprocessMinUnits          int     // Min NEW root-level units since the last round required to justify a fire (trigger A)
	inprocessGapCur            int     // Current adaptive conflict cadence (initialize = inprocessPeriod)
	inprocessGapMin            int     // Adaptive floor for inprocessGapCur (high-yield rounds tighten toward this)
	inprocessGapMax            int     // Adaptive ceiling (low-yield rounds back off to this, effectively stopping)
	rootUnitsLearned           int     // Monotonic counter: size-1 learned clauses stored (root facts discovered)
	unitsAtLastInprocess       int     // rootUnitsLearned snapshot at the last in-processing round
	inprocessExcluded          bool    // True when a static classifier (dense-binary) says in-processing is destructive here
	inprocessRoundsRun         uint64  // diagnostic: in-processing rounds actually executed
	conflictsAtLastInprocess   int     // conflict count at last in-processing round (for gap gate)
	skipSubsumption            bool
	skipBVE                    bool
	skipPolarityPhase          bool
	occurrenceWeight           float64
	skipVSIDSInit              bool
	geometricRestarts          bool
	geometricRestartThreshold  float64 // Cached threshold for geometricRestarts (= restartBase × 1.5^lubyIndex)
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
	structuredDensityGate      float64 // density >= this forces structured preprocessing even if score<0.7 & binRatio<=0.5 (0=disabled)
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

	// uniformDefaults neutralizes every per-instance classifier bifurcation to
	// a single fixed value (see setUniformDefaults / classifyInstance). A/B
	// test infrastructure only: when false, behavior is byte-identical to the
	// pre-override classified stack. Lets us measure whether the per-instance
	// classifier gates are net-positive across the heldout distribution.
	uniformDefaults bool

	// glueLearned counts learned clauses with LBD ≤ 2 (glue clauses) since
	// search start. Consumed by the behavioral governor.
	// Placed at struct end with other cold fields to avoid shifting hot/warm
	// cache lines (69d72f81 regressed from 9s to TMO when these were mid-struct).
	glueLearned uint64

	// Unified search governor: runtime self-correction compensating for
	// classifier misclassification WITHOUT static per-instance gates. Evaluated
	// at restart boundaries (cold path) using rolling-window deltas of live
	// search signals. See governor.go / maybeAdaptSearch.
	govStartConf    uint64 // cumulative conflicts at last window snapshot
	govStartLbd     uint64 // cumulative LBD sum at last snapshot
	govStartLbdC    uint64 // cumulative LBD-clause count at last snapshot
	govNextConflict int    // next conflict count at which to evaluate the governor
	govStagFired    bool   // Detector 4 fired: restart base already lowered on LBD stagnation

	// Detector 4 LBD-stagnation history (last govStagWin window avgLBDs).
	govLbdHist    [8]float64
	govLbdHistN   int
	govLbdHistIdx int

	// XOR / parity preprocessing (-parity; ON by default). Add-only
	// Gaussian elimination over detected parity families (see parity.go).
	parityEnabled   bool // gate: enable parity detection + GF(2) derivation
	parityMaxArity  int  // max support size (clause length) for a parity family (>=3)
	parityBudget    int  // hard cap on derived binary clauses appended (<=0 = off)
	paritySizeGate  int  // skip the (O(clauses)) parity scan above this many vars
	parityRowsFound int  // diagnostic: parity families detected
	parityUnits     int  // diagnostic: derived unit clauses
	parityBinaries  int  // diagnostic: derived binary clauses appended
	parityRounds    int  // diagnostic: in-loop parity re-derivations that fired (P4 fixpoint)

	// Governor tuning knobs (CLI-exposed for sweeps; defaults match the
	// empirically-tuned governor). See governor.go.
	govWindow   int     // window scope in conflicts per governor eval (default 20000)
	govStagLBD  float64 // Det4: window avgLBD threshold to qualify as high (default 12)
	govStagGlue float64 // Det4: max window glue ratio to qualify as stagnation (default 0.05)
	govStagWin  int     // Det4: consecutive windows with no LBD improvement to confirm (default 3)
	govStagBase int     // Det4: target restartBase when stagnation fires (default 20)
	// Det6 (geometric-spiral -> Luby mechanism flip): fires once when running
	// geometric (geometric-restarts) restarts on a STRUCTURED unguided deep-spiral
	// signature (chain/ordering-principle-like: weak phase guidance, high flat
	// LBD, no glue, deep propagation cascade) and switches the restart MECHANISM
	// to Luby/Glucose, where the base-lowering detectors (Det1/Det4) and recurring
	// short segments can actually rescue the spiral. Geometric deepens unboundedly
	// (threshold = base·1.5^i) so it can never cut the spiral short.
	geoFlipFired        bool       // Det6 fired: restart mechanism already flipped to Luby
	govSpiralPDec       float64    // Det6: min props/dec to qualify as a deep unguided cascade (default 8)
	govSpiralHist       [8]float64 // Det6: ring buffer of recent window avgLBD values
	govSpiralIdx        int        // Det6: ring cursor
	govSpiralN          int        // Det6: number of windows accumulated
	govSpiralNext       int        // Det6: next conflict at which to evaluate (window cadence)
	govSpiralStartConf  uint64     // Det6: window-delta start (conflicts)
	govSpiralStartDec   uint64     // Det6: window-delta start (decisions)
	govSpiralStartProps uint64     // Det6: window-delta start (propagations)
	govSpiralStartLbd   uint64     // Det6: window-delta start (LBD sum)
	govSpiralStartLbdC  uint64     // Det6: window-delta start (LBD count)
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
	varIdx uint32
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
		debugCC:              os.Getenv("SAT_DEBUG_CC") != "",
		vsids:                NewVSIDS(formula.NumVars),
		conflicts:            0,
		iterations:           0,
		maxIter:              0,
		// P0: Pre-allocate learned clause arrays with generous capacity to avoid growth
		// learnedLiterals: 8 literals per clause average (covers most learned clauses)
		learnedLiterals:    make([]cnf.Literal, 0, maxLearned*8),
		learnedLoc:         make([]LearnedClauseLoc, 0, maxLearned),
		learnedMetadata:    make([]cnf.ClauseMetadata, 0, maxLearned), // Packed metadata
		learnedWatchIdx0:   make([]int, 0, maxLearned),                // Watched literal indices
		learnedWatchIdx1:   make([]int, 0, maxLearned),
		learnedSearchHint:  make([]int32, 0, maxLearned), // Hot-path search hints
		learnedActiveCount: 0,
		preferTrueCap:      0, // Disabled by default (CLI -prefer-true-cap overrides)
		learnedCapacity:    0,
		unitLearnedList:    make([]int, 0, 64), // Pre-allocate for unit clause tracking
		verbose:            false,
		decisions:          0,
		backjumpLevel:      0,
		maxLearned:         maxLearned,
		restartBase:        restartBase,
		restartCount:       0,
		lubyIndex:          0,
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
		bigBfsBudget:        16, bigHitWindow: 50000,
		bigMinHitRate:     0.05,
		tmpLevelCount:     make([]int, formula.NumVars+1),
		tmpLevelCountUsed: make([]bool, formula.NumVars+1),
		tmpCandidates:     make([]resolveCandidate, 0, 200), // Increased from 100
		tmpLevelSet:       make([]int, 0, formula.NumVars),
		tmpLevelSetUsed:   make([]bool, formula.NumVars+1),
		tmpResolved:       make([]bool, formula.NumVars),
		tmpResolvedVars:   make([]uint32, 0, formula.NumVars),
		tmpTouchedVars:    make([]uint32, 0, formula.NumVars),
		// P1: Increased buffer capacity from 64 to 256 to handle larger learned clauses
		tmpLearnedLits:   make([]cnf.Literal, 0, 256),
		tmpMinSeenVars:   make([]uint32, 0, 256),
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
		// LBD gate for BIG+recursive minimization: skip clauses with pre-minimize
		// LBD > 16 (reduceDB eviction targets => mostly wasted minimization). 0 disables.
		minimizeLBDGate: 16,
		// Unit propagation budget: 0 = unlimited (small instances use fixpoint cap).
		// Large instances set this to bound preprocessing time.
		unitPropBudget: 0,
		// Variable elimination budget: 0 = unlimited (small instances).
		// Large instances set this to bound resolvent generation.
		veBudget: 5000000, // 5M resolvents default; large instances override to 2M
		// Subsumption budget: 0 = unlimited. Sized adaptively before preprocessing
		// (in preprocessAggressive) to bound the O(n²)-ish clause-pair scans.
		subsumptionBudget: 0,
		// Vivification: run every 200 restarts (configurable via CLI). Default
		// raised from 100 after the resweep: vivify=200 is a 0-TMO operating
		// point with lower PAR2 (1.37 vs 1.42) and flat median on the 72-suite —
		// it trades a big win on the long-clause 274099073 (23.3->6.7s) for a
		// moderate loss on the phase-transition 30eb4ef44 (11.7->20.7s).
		vivifyPeriod:  200,
		vivifyEnabled: true,
		// Min conflicts between vivify rounds. Without this gate, small/fast-restart
		// instances fire vivify every ~50 restarts = every few hundred conflicts,
		// thrashing the solver with low-yield rounds. 20000 ensures vivify only
		// fires on genuinely hard instances (those exceeding ~20K conflicts); easy
		// instances solve before vivify ever triggers.
		vivifyMinConflictGap: 600000,
		// Learned-clause subsumption: same gating cadence as vivification.
		// Self-gating via early-return on no binary learned clauses means it
		// is effectively free on random instances, so it is always enabled.
		subsumptionPeriod:         100,
		subsumptionMinConflictGap: 20000,
		inprocessBudget:           2000000,
		inprocessMinYield:         100,
		inprocessMinUnits:         16,
		inprocessGapMin:           5000,
		inprocessGapMax:           200000,
		// Runtime decay adaptation: first check after 500-conflict warmup.
		govNextConflict:      500,
		randomPhaseRate:      0,
		restartPhaseFlipRate: 0,
		dbCapFactor:          1.0,
		// Parity preprocessing (-parity; ON by default after the distributional
		// held-out gate passed on all validation families — no new TMO, no
		// median regression, ~5700x median-PAR2 win on Tseitin families).
		parityEnabled:  true,
		parityMaxArity: 6,
		parityBudget:   2000,
		// skip the parity scan on huge industrial encodings: detectParityRows is
		// O(total clause length) and yields nothing on such instances (net ~4%
		// suite PAR2 win across the 72-suite, daf59d -21%, 262ba -25%). The
		// held-out gate's parity benefit lives on small Tseitin/structured
		// families (<=9x9 grids, <=50 vars), safely below this gate.
		paritySizeGate: 50000,
		// Configurable parameters with defaults
		preprocessingMaxVars:    50000,
		preprocessingMaxClauses: 500000,
		// Density rescue: score<0.7 & binRatio<=0.5 can still be a structured
		// industrial encoding (high clause/variable ratio); random k-SAT density
		// is ~<=7. densityScore caps at /5 so dense formulas stay low-scored
		// while being unmistakably structured. 0 disables.
		structuredDensityGate: 15.0,
		govWindow:             20000,
		govStagLBD:            12.0,
		govStagGlue:           0.05,
		govStagWin:            3,
		govStagBase:           20,
		govSpiralPDec:         8.0, // Det6: deep unguided cascade threshold for geo->Luby flip
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
		lbdTier1Threshold:          5,
		lbdTier2Threshold:          2,
		dbGrowthDivisor:            50,
		deletionTriggerRatio:       1.5,
		dbShrinkThreshold:          10,
		dbShrinkFloorMultiplier:    3,
		dbMaxLen:                   25, // reduceDB length gate: evict >25-lit clauses first on binary-heavy formulas
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

// SetGovernorWindow sets the conflict-window scope (in conflicts) for the
// runtime search-governor evaluation cadence (Detector 4 / Detector 6).
func (s *CDCLSolver) SetGovernorWindow(window int) {
	if window > 0 {
		s.govWindow = window
	}
}

// SetGovernorDet4Params overrides the Detector 4 (LBD-stagnation) tuning knobs.
func (s *CDCLSolver) SetGovernorDet4Params(stagLBD, stagGlue float64, stagWin, stagBase int) {
	if stagLBD > 0 {
		s.govStagLBD = stagLBD
	}
	if stagGlue >= 0 {
		s.govStagGlue = stagGlue
	}
	if stagWin >= 1 {
		s.govStagWin = stagWin
	}
	if stagBase >= 1 {
		s.govStagBase = stagBase
	}
}

// SetGovernorSpiralPDec sets the Det6 (geometric-spiral -> Luby flip) minimum
// props/dec qualifying as a deep unguided cascade.
func (s *CDCLSolver) SetGovernorSpiralPDec(pdec float64) {
	if pdec > 0 {
		s.govSpiralPDec = pdec
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

// SetExplicitFlags records which CLI flags were explicitly set by the user.
// The behavioral governor respects user-set restart/Glucose flags: it skips
// overriding any parameter whose flag appears in this set. The map keys are
// the CLI flag names (e.g. "restart-base", "restart-glucose-min").
func (s *CDCLSolver) SetExplicitFlags(flags map[string]bool) {
	s.explicitFlags = flags
}

// flagSet reports whether the user explicitly passed the named CLI flag.
// Returns false if SetExplicitFlags was never called (no flags recorded).
func (s *CDCLSolver) flagSet(name string) bool {
	return s.explicitFlags != nil && s.explicitFlags[name]
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

// SetDBMaxLen sets the reduceDB length gate: learned clauses strictly longer
// than dbMaxLen become top-priority eviction candidates. 0 (default) disables
// the length gate entirely. The gate is applied ONLY at reduceDB time — the
// asserting clause is always still learned after a conflict, so search always
// progresses regardless of this setting.
func (s *CDCLSolver) SetDBMaxLen(dbMaxLen int) {
	s.dbMaxLen = dbMaxLen
}

func (s *CDCLSolver) SetOccurrenceWeight(w float64) {
	s.occurrenceWeight = w
}

func (s *CDCLSolver) SetSkipVSIDSInit(skip bool) {
	s.skipVSIDSInit = skip
}

func (s *CDCLSolver) SetGeometricRestarts(enabled bool) {
	s.geometricRestarts = enabled
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

// uniformLBDScale is the fixed LBD-bonus scale used by the -uniform A/B
// baseline. Matches the vsids internal default / the adaptive formula's floor:
// a purely-VSIDS signal with negligible LBD activity guidance (the most
// conventional, MiniSat-like configuration).
const uniformLBDScale = 10.0

// SetUniformDefaults enables the A/B "uniform baseline" mode. It forces every
// per-instance classifier bifurcation in classifyInstance to a single fixed
// value, leaving only the global defaults (geometric, minisatBumps, governors,
// DB shrink, etc.) active. Diagnostic/test infrastructure only for measuring
// whether the per-instance classifier gates are net-positive across the
// heldout distribution. When false (default) behavior is unchanged.
func (s *CDCLSolver) SetUniformDefaults(enabled bool) {
	s.uniformDefaults = enabled
	if enabled {
		// Neutralize the skip-* hardening gates and the useBumpAnalyze gating:
		// everything runs the un-gated path (always attempt BVE/polarity/
		// subsumption; always bump only the conflict clause, never analyze_toclear).
		s.skipBVE = false
		s.skipPolarityPhase = false
		s.skipSubsumption = false
		s.inprocessExcluded = false
		s.useBumpAnalyze = false
		s.useBumpAnalyzeOverride = true
		// Fixed subsumption period (no <500-var special case).
		s.subsumptionPeriod = 100
		s.subsumptionPeriodSet = true
		// Fixed LBD bonus scale (no adaptive binaryRatio/numVars formula).
		s.vsids.SetLBDBonusScale(uniformLBDScale)
		s.lbdScaleOverride = true
	}
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

// SetMinimizeLBDGate skips BIG+recursive minimization for learned clauses whose
// pre-minimize LBD exceeds n. High-LBD clauses are reduceDB's first eviction
// targets, so minimizing them is largely wasted cost; gating them preserves the
// cost savings without weakening the low-LBD clauses that survive and drive the
// search (so instances that depend on strong clauses are unaffected). 0 always
// minimizes.
func (s *CDCLSolver) SetMinimizeLBDGate(n int) {
	if n < 0 {
		n = 0
	}
	s.minimizeLBDGate = n
}

// SetVivifyPeriod sets how often vivification runs (every Nth restart).
// 0 disables vivification entirely.
func (s *CDCLSolver) SetVivifyPeriod(p int) {
	s.vivifyPeriod = p
}

// SetInprocess enables (period>0) in-processing: re-simplify the original
// clause DB (subsumption + bounded VE) at level 0. period is both the master
// switch and the INITIAL conflict cadence; the cadence then adapts to measured
// yield (productive rounds tighten, unproductive rounds back off). budget is
// the per-round BVE resolvent cap (0=unlimited).
func (s *CDCLSolver) SetInprocess(period, budget int) {
	s.inprocessPeriod = period
	s.inprocessGapCur = period
	if budget > 0 {
		s.inprocessBudget = budget
	}
}

// SetInprocessMinYield sets the per-round yield (subsumed+strengthened+
// eliminated) threshold: below it the adaptive cadence backs off (lengthens
// the gap), at/above it the cadence tightens. 0 = disable the yield gate.
func (s *CDCLSolver) SetInprocessMinYield(y int) {
	s.inprocessMinYield = y
}

// SetInprocessMinUnits sets the minimum number of NEW root-level units since the
// last round required to justify firing an in-processing round (trigger A).
func (s *CDCLSolver) SetInprocessMinUnits(n int) {
	if n > 0 {
		s.inprocessMinUnits = n
	}
}

// SetInprocessGapRange sets the adaptive cadence floor/ceiling (min/max
// conflicts between rounds). Setting min==max disables adaptation.
func (s *CDCLSolver) SetInprocessGapRange(minV, maxV int) {
	if minV > 0 {
		s.inprocessGapMin = minV
	}
	if maxV >= minV && maxV > 0 {
		s.inprocessGapMax = maxV
	}
	if s.inprocessGapCur > 0 && s.inprocessGapCur < s.inprocessGapMin {
		s.inprocessGapCur = s.inprocessGapMin
	}
}

// SetDBCapFactor manually scales the learned-DB deletion target (1.0 = baseline).
func (s *CDCLSolver) SetDBCapFactor(f float64) {
	if f <= 0 {
		f = 1.0
	}
	s.dbCapFactor = f
}

// SetParityParams gates XOR/parity preprocessing (-parity, default ON). maxArity
// is the max support size (>=3); budget caps derived binary clauses (<=0 = off).
func (s *CDCLSolver) SetParityParams(on bool, maxArity, budget int) {
	s.parityEnabled = on
	if maxArity >= 3 {
		s.parityMaxArity = maxArity
	}
	if s.parityMaxArity > parityMaxArityMax {
		s.parityMaxArity = parityMaxArityMax
	}
	if budget >= 0 {
		s.parityBudget = budget
	}
}

// SetParitySizeGate sets the variable-count threshold above which the parity
// scan is skipped (large industrial encodings get no parity benefit but pay
// an O(total clause length) detectParityRows cost). <=0 keeps parity always on.
func (s *CDCLSolver) SetParitySizeGate(n int) {
	s.paritySizeGate = n
}

// SetVivifyMinConflictGap sets the minimum number of conflicts that must occur
// between two vivification rounds. 0 disables the gap gate (restart-based only).
func (s *CDCLSolver) SetVivifyMinConflictGap(g int) {
	s.vivifyMinConflictGap = g
}

// SetBigBfsBudget sets the per-call BFS node budget for BIG transitive
// minimization (default 16). Larger budgets find more reductions at higher cost.
func (s *CDCLSolver) SetBigBfsBudget(n int) {
	if n > 0 {
		s.bigBfsBudget = n
	}
}

// SetBigHitWindow sets the adaptive BIG hit-rate gate observation window: if
// BIG accumulates bigHitWindow per-conflict attempts with zero literal-removal
// hits, BIG is disabled for the rest of the solve (trajectory-neutral — a 0-hit
// BIG changes no learned clause). 0 disables the gate (never auto-disable).
func (s *CDCLSolver) SetBigHitWindow(n int64) {
	s.bigHitWindow = n
}

// SetBigMinHitRate sets the sliding-window hit-rate threshold: BIG is disabled
// once the most recent bigHitWindow attempts fall below this fraction of hits.
// 0 keeps the historical behavior (disable only on a fully 0-hit window).
func (s *CDCLSolver) SetBigMinHitRate(rate float64) {
	if rate < 0 {
		rate = 0
	}
	s.bigMinHitRate = rate
}

// recordBigOutcome feeds a BIG minimization attempt outcome into the
// sliding-window ring and fires the rate gate. window<=0 disables the gate;
// bigMinHitRate<=0 restores the historical "disable only on a fully 0-hit
// window" behavior (a window with any hit never disables BIG).
func (s *CDCLSolver) recordBigOutcome(hit bool) {
	w := int(s.bigHitWindow)
	if w <= 0 || s.bigDisabled {
		return
	}
	if s.bigWinRing == nil {
		s.bigWinRing = make([]bool, w)
	}
	if s.bigWinFull {
		if s.bigWinRing[s.bigWinPtr] {
			s.bigWinHits--
		}
	} else if s.bigWinPtr == w-1 {
		s.bigWinFull = true
	}
	s.bigWinRing[s.bigWinPtr] = hit
	if hit {
		s.bigWinHits++
	}
	s.bigWinPtr++
	if s.bigWinPtr == w {
		s.bigWinPtr = 0
	}
	if s.bigMinHitRate > 0 && s.bigWinFull &&
		float64(s.bigWinHits) < float64(w)*s.bigMinHitRate {
		s.bigDisabled = true
		s.Log("c [BIG] sliding-window gate: last %d attempts hit-rate %.2f%% < %.1f%% -> disabling BIG\n",
			w, 100*float64(s.bigWinHits)/float64(w), 100*s.bigMinHitRate)
	}
}

// SetBigLearn enables the learned-binary BIG augmentation for transitive
// minimization. Stricter minimization (BIG hits +) but shifts search trajectory
// with net wall-time regression on the suite — off by default (A/B only).
func (s *CDCLSolver) SetBigLearn(on bool) {
	s.bigLearnFlag = on
}

// SetStructuredDensityGate sets the density at/below which a score<0.7 &
// binRatio<=0.5 instance is rescued onto structured preprocessing (0 disables).
func (s *CDCLSolver) SetStructuredDensityGate(d float64) {
	if d < 0 {
		d = 0
	}
	s.structuredDensityGate = d
}

// SetPreferTrueCap sets how far the replacement scan hunts for a currently-TRUE
// literal before falling back to the first unassigned one, restricted to learned
// clauses. 0 = old behavior (first non-false, no prefer-true).
func (s *CDCLSolver) SetPreferTrueCap(n int) {
	if n < 0 {
		n = 0
	}
	s.preferTrueCap = n
}

// SetSubsumptionPeriod sets how often learned-clause subsumption runs (every
// Nth restart). 0 disables subsumption entirely.
func (s *CDCLSolver) SetSubsumptionPeriod(p int) {
	s.subsumptionPeriod = p
	s.subsumptionPeriodSet = true
}

// SetSubsumptionMinConflictGap sets the minimum number of conflicts that must
// occur between two subsumption rounds. 0 disables the gap gate (restart-based
// only). Used by tests to force subsumption cadence; no CLI flag wires it.
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

// assertTrueUnitComplete is a SAT_DEBUG_CC canary for the two-watched-literal
// completeness invariant: after a conflict-free propagation, no clause may be a
// true unit (exactly one unassigned literal, all others false) that was not
// propagated. If one is, a watch was dropped/missed upstream -> search can stall
// or become unsound. Scans original + learned clauses; debugging only.
func (s *CDCLSolver) assertTrueUnitComplete() {
	for ci := 0; ci < s.cnf.NumOriginalClauses(); ci++ {
		off, sz := s.cnf.GetOriginalClauseInfo(ci)
		lits := s.cnf.GetLiteralPool()[off : off+sz]
		un := int(-1)
		nun := 0
		hasTrue := false
		for _, lit := range lits {
			a := s.assignments[lit.Var()]
			if a.Level < 0 {
				un = int(lit.Var())
				nun++
				if nun > 1 {
					break
				}
			} else if lit.IsNegated() != a.Value {
				hasTrue = true
			}
		}
		if nun == 1 && !hasTrue {
			fmt.Fprintf(os.Stderr, "CC-MISS: original clause %d is a UNIT on var %d not propagated; lits=%v\n", ci, un, lits)
			panic("completeness: missed unit in original clause")
		}
	}
	for cid := 0; cid < len(s.learnedLoc); cid++ {
		if s.learnedLoc[cid].Size == 0 {
			continue
		}
		lits := s.getLearnedClauseLiterals(cid)
		un := int(-1)
		nun := 0
		hasTrue := false
		for _, lit := range lits {
			a := s.assignments[lit.Var()]
			if a.Level < 0 {
				un = int(lit.Var())
				nun++
				if nun > 1 {
					break
				}
			} else if lit.IsNegated() != a.Value {
				hasTrue = true
			}
		}
		if nun == 1 && !hasTrue {
			fmt.Fprintf(os.Stderr, "CC-MISS: learned clause %d is a UNIT on var %d not propagated; lits=%v\n", cid, un, lits)
			panic("completeness: missed unit in learned clause")
		}
	}
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
	s.Log("c Restarts:      %s (last segment %d glue)\n",
		s.restartReasonSummary(), s.restartSegGlueCount)
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
	liveHist := [6]int{}
	liveLong := 0
	for i := 0; i < s.learnedCapacity; i++ {
		sz := int(s.learnedLoc[i].Size)
		if sz > 0 {
			switch {
			case sz <= 2:
				liveHist[0]++
			case sz <= 5:
				liveHist[1]++
			case sz <= 10:
				liveHist[2]++
			case sz <= 20:
				liveHist[3]++
			case sz <= 50:
				liveHist[4]++
			default:
				liveHist[5]++
			}
			if sz > 10 {
				liveLong++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "c [final] t=%.2fs conflicts=%d decisions=%d props=%d props/dec=%.1f moves=%d learned=%d/%d emaLBD=%.1f avgLBD=%.1f totAvgLBD=%.1f%s | br=bump=%d/init=%d | min: rate=%.1f%% | BIG: calls=%d hits=%d score=%.2f brat=%.2f | parity: rows=%d units=%d bins=%d pfr=%d 	 | vivify: rounds=%d | subsump: rounds=%d sub=%d str=%d | inprocess: rounds=%d | uip-fallback=%d | tiers: blitF=%d bin=%d gen=%d(mov=%d) | move: recast=%d hintMiss=%d scanLits=%d | hist=[%d %d %d %d %d %d] live=[%d %d %d %d %d %d] live>10=%d\n",
		s.elapsedSec(), s.conflicts, s.decisions, s.propagations, propsPerDec, s.numWatchMoves,
		s.learnedActiveCount, s.maxLearned, s.emaLBD, avgLBD, totalAvgLBD, shrunk,
		s.branchBumpScheme, s.branchInitMode,
		minRate,
		s.bigMinimizeCalls, s.bigMinimizeHits, s.structureScore, s.binaryRatio,
		s.parityRowsFound, s.parityUnits, s.parityBinaries, s.parityRounds,
		s.vivifyRoundsRun,
		s.subsumptionRoundsRun, s.subsumptionClausesSubsumed, s.subsumptionClausesStrengthened,
		s.inprocessRoundsRun,
		s.uipFallbackCount,
		s.blitFastHits, s.binarySlow, s.generalSlow, s.generalMoves,
		s.moveReallocCast, s.moveHintMiss, s.moveScanLits,
		s.learnedLenHist[0], s.learnedLenHist[1], s.learnedLenHist[2],
		s.learnedLenHist[3], s.learnedLenHist[4], s.learnedLenHist[5],
		liveHist[0], liveHist[1], liveHist[2], liveHist[3], liveHist[4], liveHist[5], liveLong)
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
	fmt.Fprintf(os.Stderr, "c [stats] t=%.2fs conflicts=%d level=%d decisions=%d props=%d props/dec=%.1f learned=%d emaLBD=%.1f avgLBD=%.1f totAvgLBD=%.1f glue=%.3f%s | br=bump=%d/init=%d | min: rate=%.1f%% | BIG: hits=%d/%d | restarts[%s] seg-glue=%d\n",
		s.elapsedSec(), s.conflicts, s.level, s.decisions, s.propagations, propsPerDec,
		s.learnedActiveCount, s.emaLBD, avgLBD, totalAvgLBD, glueRatio, shrunk,
		s.branchBumpScheme, s.branchInitMode,
		minRate,
		s.bigMinimizeHits, s.bigMinimizeCalls,
		s.restartReasonSummary(), s.restartSegGlueCount)
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
	// RegularOccurrence is true when every appearing variable occurs the same
	// number of times (within a small relative tolerance). Holds throughout for
	// k-regular structured formulas (Tseitin on a regular graph, regular graph
	// coloring), whose variables all have identical occurrence counts. Random
	// k-SAT essentially never satisfies it. It distinguishes provably-regular
	// uniform-long structured families from random k-SAT so the uniform-long
	// random penalty (longSizeVaried) does not misroute them to random handling.
	RegularOccurrence bool
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

	// Regular-occurrence detection. In a k-regular structured formula every
	// appearing variable occurs exactly the same number of times (identical
	// node degree structure). Random k-SAT (Poisson-distributed occurrences)
	// never has near-equal per-variable counts. This is a high-precision,
	// low-recall signal: it only exempts provably-regular instances, so random
	// families are never touched by the exemption.
	minOcc := -1
	maxOcc := -1
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		n := posCount[i] + negCount[i]
		if n == 0 {
			continue
		}
		if minOcc < 0 {
			minOcc, maxOcc = n, n
		} else {
			if n < minOcc {
				minOcc = n
			}
			if n > maxOcc {
				maxOcc = n
			}
		}
	}
	if minOcc > 0 {
		meanOcc := float64(minOcc+maxOcc) / 2.0
		if float64(maxOcc-minOcc)/meanOcc < 0.02 {
			structure.RegularOccurrence = true
		}
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
	// EXEMPTION: provably-regular uniform-long families (e.g. Tseitin on a
	// regular graph — all clauses same size AND every variable occurs equally
	// often) are structured parity/theory instances, not random k-SAT. Without
	// the exemption they are misclassified as "random k-SAT k≥4" (wrong decay,
	// preprocessing disabled). RegularOccurrence is high-precision (random
	// instances never satisfy it), so routing these to the structured path is
	// safe and does not reintroduce the rand4sat misclassification.
	if longCount > 0 && !longSizeVaried && !structure.RegularOccurrence {
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

	// Cache classifier output for downstream consumers (initVSIDSOccurrenceBonus
	// gates the polarity-based initial phase on these metrics; the level-capped
	// restart gate reads longClauseRatio; the governor gates on binaryRatio and
	// structureScore).
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
	// (35) separates the two regimes. Under -uniform (uniformDefaults) these
	// per-instance gates are disabled: everything runs the un-gated path.
	if !s.uniformDefaults {
		s.skipPolarityPhase = structure.BinaryRatio > 0.9 && structure.Density > 35.0

	// BVE is pure overhead on dense binary instances: it hits the resolvent
	// budget eliminating only 7-14% of variables while spending 4-15s on
	// resolvent generation + post-BVE rebuild. The highly-connected BIG is
	// navigated in <2s by watch-based propagation alone. 8202af80 (density
	// 24.5, 99.8% binary): 16.7s→1.4s. bb34f22f (density 3.12, 67% binary) is
	// NOT gated (density ≤ 10) — it's the instance VE was tuned for.
	s.skipBVE = structure.BinaryRatio > 0.95 && structure.Density > 10.0
	// In-processing classifier: in-process BVE has HIGH yield on dense-binary
	// instances yet is destructive to search there (de2b: +2246 eliminations
	// but TMO; same class as skipBVE). Exclude the dense-binary/BVE-hostile
	// class outright so even an enabled in-processing never runs on them.
	s.inprocessExcluded = s.skipBVE
	// Subsumption is O(clauses × occurrences × clause-length). On very dense
	// instances (density > 60) the occurrence lists are huge, making each pass
	// take seconds while the instance often solves in <0.1s without it.
	// ramlb_6_6 (density 149): subsumption 5s, search 0.03s. kcliquebin_6
	// (density 298): subsumption 2s, search 0.01s. ramlb_5_5 (density 74):
	// subsumption 0.4s, search 0.01s. The threshold of 60 is above the
	// highest-density suite instance (32baec6a, density 49).
	s.skipSubsumption = structure.Density > 60.0
	} // end uniform-disabled classifier gates

	// Size-adaptive subsumption period. Small instances (numVars < 500)
	// benefit from period=50 (subsumption fires at restart 50 vs 100); op_18
	// regresses +1.6s and rand3sat_200 +1.4s with period=100. Large instances
	// keep period=100. Skipped if the caller explicitly set the period via
	// SetSubsumptionPeriod (CLI or tests).
	if !s.subsumptionPeriodSet && s.cnf.NumVars < 500 {
		s.subsumptionPeriod = 50
	}

	// Single principled search signal: the analyze_toclear variable-bumping
	// gate (useBumpAnalyze). VSIDS decay, restartBase, and Glucose restarts are
	// global (constructor/CLI); the ONLY per-instance decision is whether to
	// bump all touched vars during conflict analysis (MiniSat analyze_toclear):
	//   ON  for random / low-structure families (the dominant hard case) —
	//       brings hard random k-SAT from TMO to ~0.8s on the dev rail.
	//   OFF for binary-heavy and dense/long-clause structured instances,
	//       where VSIDS is the sole guidance signal and diluting it across
	//       all touched vars degrades the search.
	if !s.useBumpAnalyzeOverride {
		s.useBumpAnalyze = structure.BinaryRatio <= 0.5 &&
			(structure.Density < 10.0 || structure.PolarityImbalance > 0.4)
	}

	// Classifier telemetry: emit every structural gate decision on stderr so the
	// audit (and future regression checks) can read all decisions at once,
	// independent of -verbose / -stats. Must stay read-only on s.cnf (as the rest
	// of classifyInstance) and not perturb the search.
	preproc := "on"
	if s.structureScore < 0.7 && s.binaryRatio <= 0.5 &&
		!(s.structuredDensityGate > 0 && s.cnf.NumVars > 0 && float64(s.cnf.NumClauses)/float64(s.cnf.NumVars) >= s.structuredDensityGate) &&
		int(s.cnf.NumVars) <= s.preprocessingMaxVars {
		preproc = "off"
	}
	flp := "off"
	if s.structureScore < 0.7 && s.binaryRatio <= 0.5 {
		flp = "on"
	}
	big := true
	if s.structureScore < 0.7 && s.binaryRatio > 0.4 {
		big = false
	}
	lbdScale := 200000.0 * s.binaryRatio / float64(s.cnf.NumVars)
	if lbdScale < 10.0 {
		lbdScale = 10.0
	}
	fmt.Fprintf(os.Stderr,
		"c classify: score=%.3f binRatio=%.3f ternary=%.3f long=%.3f density=%.1f imb=%.2f regOcc=%t | preproc=%s bumpAnalyze=%t skipBVE=%t skipPolarity=%t skipSubsumption=%t FLP=%s BIG=%t subperiod=%d lbdScale=%.0f\n",
		s.structureScore, s.binaryRatio, structure.TernaryRatio, s.longClauseRatio,
		structure.Density, s.polarityImbalance, structure.RegularOccurrence,
		preproc, s.useBumpAnalyze, s.skipBVE, s.skipPolarityPhase, s.skipSubsumption,
		flp, big, s.subsumptionPeriod, lbdScale)
}

// getAdaptivePreprocessingConfig returns preprocessing config based on the
// cached structureScore (set by classifyInstance, which must have run first).
func (s *CDCLSolver) getAdaptivePreprocessingConfig() PreprocessingConfig {
	// Under -uniform (uniformDefaults) always run the standard unit-prop +
	// single-pass config, ignoring the random-like disable and the density/50K
	// recovery rescues. Neutralizes the classifier's preprocessing decision.
	if s.uniformDefaults {
		return PreprocessingConfig{
			EnableUnitProp: true,
			MaxPasses:      1,
		}
	}
	// Very large instances are industrial / reduction-friendly regardless of the
	// borderline score: structureScore is tuned on small instances and
	// under-weights huge regular encodings. E.g. course-timetabling instances at
	// 105-222K vars score only 0.65-0.70 yet BVE eliminates ~27% of variables,
	// so the score-based "random-like" early-out must not exclude them. A corpus
	// scan shows the ONLY >50K-var instances with score<0.7 AND binaryRatio<=0.5
	// are exactly those timetabling instances; true random instances are small
	// (a few hundred vars) so the size gate never misroutes them into aggressive
	// preprocessing. Dense-binary large instances are separately protected by
	// skipBVE.
	if int(s.cnf.NumVars) > s.preprocessingMaxVars {
		s.Log("c [preprocessing] Large instance (%d vars) -> enabling aggressive preprocessing\n", s.cnf.NumVars)
		return PreprocessingConfig{
			EnableUnitProp: true,
			MaxPasses:      1,
		}
	}
	// Density rescue: a score<0.7 & binRatio<=0.5 formula with very high
	// clause/variable density is a structured industrial encoding, not random
	// k-SAT (random density stays ~<=7, and the uniform-long penalty forces
	// rand4+/rand3 dense formulas low too). Route it onto preprocessing like a
	// structured instance. Subsumption/BVE stay gated by their own density/
	// ratio skip flags inside preprocessAggressive.
	if s.structuredDensityGate > 0 && s.cnf.NumVars > 0 && float64(s.cnf.NumClauses)/float64(s.cnf.NumVars) >= s.structuredDensityGate {
		s.Log("c [preprocessing] Density rescue (%.1f >= %.1f) -> structured preprocessing\n",
			float64(s.cnf.NumClauses)/float64(s.cnf.NumVars), s.structuredDensityGate)
		return PreprocessingConfig{
			EnableUnitProp: true,
			MaxPasses:      1,
		}
	}
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

	// XOR/parity precondition (sound, add-only; -parity, gated). Detect parity
	// families and derive units/binary equivalences via GF(2) Gaussian, appended
	// to the DB as consequences; unit-propagation below then assigns them. Only
	// for structured instances (EnableUnitProp path). Runs after equiv so binary
	// equivalences are already factored out.
	added := false
	if parityResult := s.analyzeParity(); parityResult != UNKNOWN {
		s.printStats()
		return parityResult
	}
	if s.parityEnabled && (s.parityBinaries > 0 || s.parityUnits > 0) {
		s.cnf.RebuildLiteralPool()
		added = true
	}
	_ = added

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
			veResult := s.boundedVarElimination(false)
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

		// P4 — parity as part of the preprocessing fixpoint. After BVE/subsumption/
		// unit-prop change the clause DB, previously-incomplete parity families can
		// become COMPLETE (root units assign variables, BVE removes variables),
		// exposing XOR structure that yields NEW units/binaries. Re-derive each
		// pass. Add-only and sound (see analyzeParity); bounded by the loop's pass
		// cap and the global -parity-budget. Gated -parity (default off).
		if s.parityEnabled && s.parityMaxArity >= 3 && s.parityBudget > 0 {
			beforeParity := s.parityBinaries + s.parityUnits
			if pr := s.analyzeParity(); pr != UNKNOWN {
				s.printStats()
				return pr
			}
			if s.parityBinaries+s.parityUnits > beforeParity {
				s.cnf.RebuildLiteralPool()
				passChanged = true
				s.parityRounds++
			}
		}
		ppMark("parity re-derivation pass")

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
	s.watchListsBinary = make([][]cnf.Watch, numLits)

	// Pre-allocate each watch list to its actual original-clause load rather
	// than a uniform estimate. At init (post-preprocessing) most literals are
	// unassigned, so chooseWatchPositions picks each clause's first two literals
	// (positions 0 and 1); count those per literal. Capacity is purely a memory/
	// reallocation concern and never affects search order (append handles any
	// under-count, e.g. clauses containing level-0-assigned literals whose watch
	// position differs). Headroom covers learned-clause growth during the solve.
	occ := make([]int, numLits)
	occBin := make([]int, numLits)
	for clauseID := 0; clauseID < s.cnf.NumClauses; clauseID++ {
		lits := s.cnf.Clauses[clauseID].Literals
		if len(lits) < 2 {
			continue
		}
		if len(lits) == 2 {
			occBin[cnf.LitToIndex(lits[0])]++
			occBin[cnf.LitToIndex(lits[1])]++
		} else {
			occ[cnf.LitToIndex(lits[0])]++
			occ[cnf.LitToIndex(lits[1])]++
		}
	}
	for i := range s.watchLists {
		capEst := occ[i]*2 + 16
		if capEst < 8 {
			capEst = 8
		}
		if capEst > 1024 {
			capEst = 1024
		}
		s.watchLists[i] = make([]cnf.Watch, 0, capEst)

		capEst = occBin[i]*2 + 16
		if capEst < 8 {
			capEst = 8
		}
		if capEst > 1024 {
			capEst = 1024
		}
		s.watchListsBinary[i] = make([]cnf.Watch, 0, capEst)
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
		for _, wl := range s.watchListsBinary {
			totalWatches += len(wl)
		}
		avgWatches := float64(totalWatches) / float64(numLits)
		s.Log("c [verbose] Watched literals enabled: %d watch lists, %d total watches, %.1f avg per lit\n",
			len(s.watchLists)+len(s.watchListsBinary), totalWatches, avgWatches)
	}
}

// chooseWatchPositions selects two literal positions (indices into literals)
// to watch, preferring non-false (unassigned or true) literals so the
// watched-literal invariant (at most one watched literal is false) holds
// after setup. Returns positions (-1 if fewer than two candidates found).
// Shared by original/learned clause watch setup and compaction rebuild.
// chooseWatchPositions picks two literal positions to watch, preferring
// unassigned then true over false literals. When every literal is already
// assigned false it returns two false watches — valid for LEARNED clauses
// added mid-conflict (one watch turns true after backjump and the WB scheme
// self-corrects on the next scan), and impossible for original clauses, which
// are only watched at level 0 where all literals are unassigned.
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

// appendWatch literally appends a watch to lit's general (size>=3) or binary

// appendWatch literally appends a watch to lit's general (size>=3) or binary
// (size==2) watch list by clause size. The region a clause belongs to is
// immutable — a clause never changes size across its lifetime — so appends
// always target a stable region (never crossing between lists). This is what
// the write-pointer compaction in propagateWatched relies on: surviving
// watches keep their in-list relative order, and a clause is always in exactly
// one of the two per-literal lists.
func (s *CDCLSolver) appendWatch(litIdx int, w cnf.Watch, isBinary bool) {
	if isBinary {
		s.watchListsBinary[litIdx] = append(s.watchListsBinary[litIdx], w)
	} else {
		s.watchLists[litIdx] = append(s.watchLists[litIdx], w)
	}
}

// watchListOf returns the general or binary watch list for a literal.
func (s *CDCLSolver) watchListOf(litIdx int, isBinary bool) []cnf.Watch {
	if isBinary {
		return s.watchListsBinary[litIdx]
	}
	return s.watchLists[litIdx]
}

// setWatchList writes back a (possibly re-sliced/swapped) watch list.
func (s *CDCLSolver) setWatchList(litIdx int, isBinary bool, wl []cnf.Watch) {
	if isBinary {
		s.watchListsBinary[litIdx] = wl
	} else {
		s.watchLists[litIdx] = wl
	}
}

// addOriginalClauseToWatches adds an original clause to the watch lists.
// Original clauses are watched during initWatches at level 0, where every
// variable is unassigned, so both chosen watches are always unassigned
// (never false). The generic chooseWatchPositions prioritizes unassigned/true
// over false but, for LEARNED-clause adds, may legitimately return two false
// watches pre-backjump (one becomes true post-backjump; WB self-corrects).
// That two-false state does not occur here because all literals are unassigned.
func (s *CDCLSolver) addOriginalClauseToWatches(clauseIdx int, clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	watch0, watch1 := s.chooseWatchPositions(literals)
	if watch0 < 0 || watch1 < 0 {
		return
	}
	verifyOriginalWatchNotBothFalse(s, literals, watch0, watch1)

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

	// Pack myPos into ClauseIdx bit 30 for GENERAL (size>=3) clauses: the watch
	// on idx0 (lit0 at position 0) has myPos=0, the watch on idx1 (lit1 at
	// position 1) has myPos=1. Binary (size==2) watches live in their own
	// structural region and never decode myPos, so neither watch packs it.
	// Bit layout: 31=learned(0 here), 30=myPos, bits 0-28=clause index.
	if clauseIdx < 0 || clauseIdx >= int(watchIdxMask) {
		panic(fmt.Sprintf("satience: original clause index %d exceeds the %d-entry watch packing budget (bits 0-28); watch layout cannot represent more clauses", clauseIdx, int(watchIdxMask)))
	}
	if len(literals) == 2 {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: int32(clauseIdx),
			Blit:      litToBlit(lit1),
		}, true)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: int32(clauseIdx),
			Blit:      litToBlit(lit0),
		}, true)
	} else {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: int32(clauseIdx),
			Blit:      litToBlit(lit1),
		}, false)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: int32(clauseIdx) | int32(watchMyPosBit),
			Blit:      litToBlit(lit0),
		}, false)
	}
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

	// Pack learned flag (bit 31) + myPos (bit 30) into ClauseIdx. myPos is packed
	// only for GENERAL (size>=3) watches; binary watches live in their own region
	// and never decode it.
	// Bit layout: 31=learned, 30=myPos, bits 0-28=clause index.
	if learnedIdx < 0 || learnedIdx >= int(watchIdxMask) {
		panic(fmt.Sprintf("satience: learned clause index %d exceeds the %d-entry watch packing budget (bits 0-28); watch layout cannot represent more clauses", learnedIdx, int(watchIdxMask)))
	}
	clauseIdx0 := int32(watchLearnedBit | uint32(learnedIdx)) // myPos=0
	clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)           // myPos=1

	if len(literals) == 2 {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit1),
		}, true)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit0),
		}, true)
	} else {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit1),
		}, false)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: clauseIdx1,
			Blit:      litToBlit(lit0),
		}, false)
	}

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

	if size <= 2 {
		return false
	}

	// Copy literals to a private buffer — trial propagation may swap literals
	// in the clause's shared pool storage (watch replacement on the watched
	// positions), which would otherwise corrupt our iteration by overwriting a
	// not-yet-read literal of this same clause in s.learnedLiterals.
	src := s.tmpVivifySrc
	if cap(src) < size {
		src = make([]cnf.Literal, size)
	}
	literals := src[:size]
	copy(literals, s.learnedLiterals[offset:offset+size])
	s.tmpVivifySrc = literals
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

	// Region is immutable: a size-2 learned clause lives in watchListsBinary
	// for both its watched literals, a size>=3 clause in watchLists.
	isBinary := s.learnedLoc[learnedIdx].Size == 2

	// Clause identity for comparison: mask out myPos bit (bit 30) and binary bit
	// (bit 29) since the stored watches may have either myPos value.
	clauseID := uint32(watchLearnedBit | uint32(learnedIdx))

	idx0 := s.learnedWatchIdx0[learnedIdx]
	idx1 := s.learnedWatchIdx1[learnedIdx]

	if idx0 < 0 || idx1 < 0 || idx0 >= len(s.watchLists) || idx1 >= len(s.watchLists) {
		return
	}

	// Remove watch from lit0's watch list (swap-remove, no Blit update needed)
	wl0 := s.watchListOf(idx0, isBinary)
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
	s.setWatchList(idx0, isBinary, wl0)

	// Remove watch from lit1's watch list
	wl1 := s.watchListOf(idx1, isBinary)
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
	s.setWatchList(idx1, isBinary, wl1)
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
	if s.geometricRestarts {
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

		// Fall back to Luby sequence (configurable base). The raw Luby sequence
		// oscillates back to short segments (1,1,2,1,1,2,4,...) forever.
		lubyValue := luby(s.lubyIndex + 1)
		threshold := float64(lubyValue * s.restartBase)

		if float64(s.conflicts-s.restartCount) >= threshold {
			s.restartReasons[1]++ // luby
			return true
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

// simplifyOriginalDB re-runs original-clause simplification (subsumption +
// bounded VE) at a level-0 restart boundary, then rebuilds the original-clause
// SoA (literal pool + locs), rebuilds watches, and re-propagates unit clauses.
// In-processing makes the (otherwise preprocessing-only) reduction available to
// instances whose formula changes as search discovers unit clauses. Returns
// true if the formula became UNSAT.
func (s *CDCLSolver) simplifyOriginalDB() bool {
	if s.cnf.NumClauses == 0 {
		return false
	}
	// Materialize the original clause DB into slice form (it is nil during
	// search; parser + pre-processing consumed it into the SoA pool/locs).
	pool := s.cnf.GetLiteralPool()
	locs := s.cnf.GetOriginalClauseLocs()
	clauses := make([]cnf.Clause, 0, len(locs))
	for i := range locs {
		off := int(locs[i].Offset)
		sz := int(locs[i].Size)
		if sz == 0 {
			continue
		}
		lits := make([]cnf.Literal, sz)
		copy(lits, pool[off:off+sz])
		clauses = append(clauses, cnf.Clause{Literals: lits})
	}
	s.cnf.Clauses = clauses
	s.cnf.NumClauses = len(clauses)

	saveBudget := s.veBudget
	if s.inprocessBudget > 0 {
		s.veBudget = s.inprocessBudget
	}
	defer func() { s.veBudget = saveBudget; s.cnf.Clauses = nil }()

	subSubsumed, subStrengthened := s.subsumptionPass()
	if s.hasEmptyClause() {
		return true
	}
	elim := 0
	if vr := s.boundedVarElimination(true); vr < 0 {
		return true
	} else if vr > 0 {
		elim = vr
	}

	// Yield-based adaptive cadence (C): adjust the conflict gap for the NEXT
	// round from this round's yield (subsumed + strengthened + eliminated,
	// i.e. actual formula reduction). A productive round tightens the cadence
	// (fire again sooner), a low-yield round backs it off toward inprocessGapMax
	// (effectively stopping) without a hard latch, so a formula that becomes
	// unit-rich again can resume. Yield reuses roundYield (subsumed+
	// strengthened+eliminated).
	roundYield := subSubsumed + subStrengthened + elim
	if s.inprocessGapCur > 0 {
		if roundYield >= s.inprocessMinYield {
			if s.inprocessGapCur/2 < s.inprocessGapMin {
				s.inprocessGapCur = s.inprocessGapMin
			} else {
				s.inprocessGapCur /= 2
			}
		} else if s.inprocessGapMin > 0 && s.inprocessGapMin != s.inprocessGapMax {
			if s.inprocessGapCur*2 > s.inprocessGapMax {
				s.inprocessGapCur = s.inprocessGapMax
			} else {
				s.inprocessGapCur *= 2
			}
		}
	}

	s.cnf.RebuildLiteralPool()
	s.originalUnitClauses = precomputeOriginalUnitClauses(s.cnf)

	// Rebuild watches (original + learned) and re-propagate level-0 units so
	// the reduced formula is fully consistent before search resumes.
	s.watchInitialized = false
	s.initWatches()
	if s.propagateOriginalUnitsAndActivateWatches() {
		return true
	}
	return false
}

// inprocessDue reports whether an in-processing round is due at the current
// point. Combines trigger A (enough NEW root-level units discovered since the
// last round — the causal condition: formula changes unlock reductions) with
// the adaptive conflict gap C (inprocessGapCur, driven by yield), and the
// static dense-binary exclusion. It is deliberately not a bare conflict-period
// check: firing the expensive materialize+scan+rebuild only on real reductions
// amortizes the cost.
func (s *CDCLSolver) inprocessDue() bool {
	return s.inprocessPeriod > 0 && !s.inprocessExcluded && s.inprocessGapCur > 0 &&
		s.conflicts >= s.conflictsAtLastInprocess+s.inprocessGapCur &&
		s.rootUnitsLearned-s.unitsAtLastInprocess >= s.inprocessMinUnits
}

// runInprocess executes one in-processing round. Caller must already be at
// level 0 (either a scheduled restart — co-schedule D — or an on-demand
// cancelUntil(0) from the loop fallback). Returns true if the formula became
// UNSAT. Updates the last-round snapshot and the governor's segment counters
// (this level-0 reset wasn't a scheduled restart).
func (s *CDCLSolver) runInprocess() bool {
	s.inprocessRoundsRun++
	s.Log("c [inprocess] round %d: conflicts=%d newUnits=%d gap=%d\n",
		s.inprocessRoundsRun, s.conflicts, s.rootUnitsLearned-s.unitsAtLastInprocess, s.inprocessGapCur)
	if s.simplifyOriginalDB() {
		return true
	}
	s.conflictsAtLastInprocess = s.conflicts
	s.unitsAtLastInprocess = s.rootUnitsLearned
	s.restartSegStartConf = s.conflicts
	s.restartSegStartDec = s.decisions
	s.restartSegStartGlue = int(s.glueLearned)
	return false
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
	s.restartSegGlueCount = int(s.glueLearned) - s.restartSegStartGlue
	if s.verbose {
		s.Log("c [verbose] segment: %d conflicts / %d decisions, %d glue learned\n",
			s.conflicts-s.restartSegStartConf, segDec, s.restartSegGlueCount)
	}
	s.restartSegStartConf = s.conflicts
	s.restartSegStartDec = s.decisions
	s.restartSegStartGlue = int(s.glueLearned)

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
	if s.geometricRestarts {
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

	// In-processing co-schedule (D): we are at level 0 with no clause in use as
	// a reason, sharing this restart's already-paid reset/watch-rebuild. Firing
	// here (rather than only forcing a level-0 cancelUntil in the loop) amortizes
	// the materialize+simplify cost over the restart's existing level-0 work. The
	// loop fallback still handles long restart-free stretches.
	if s.inprocessDue() {
		if s.runInprocess() {
			return true // UNSAT detected
		}
	}

	// Vivification cadence, decoupled from the restart index. Under the old
	// schedule (lubyIndex % vivifyPeriod == 0 AND the conflict gap), vivify was
	// keyed to a flat restart counter that grows ~logarithmically with conflicts
	// under geometric restarts (threshold = restartBase * 1.5^k with base 200),
	// so it silently almost never fired on geometric-default instances and only
	// engaged after the Det6 geometric->Luby flip. Vivify can only run at the
	// level-0 restart boundary, so a true mode-independent cadence should be
	// conflict-based: fire when vivifyMinConflictGap conflicts have elapsed since
	// the last round. vivifyPeriod>0 is retained purely as the on/off switch.
	// The default gap is deliberately high (600000) so vivify fires rarely,
	// recovering the protective (near-dormant) firing rate of the old gate. A
	// low/moderate gap re-opens a regression on high-conflict vivify-hostile
	// instances (30eb, ~483k conflicts): every firing gap that helps the
	// 274099073/de2b class also fires on it, because it has the highest conflict
	// count, so no single conflict-cadence value keeps those wins without it.
	if s.vivifyEnabled && s.vivifyPeriod > 0 &&
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

	// Unified search governor: runtime self-correction compensating for
	// classifier misclassification. Same cadence as decay adaptation (every
	// adaptRestartPeriod restarts, cold path); internally window-gated on
	// govNextConflict (govWindowConfScope conflicts), so it is effectively a
	// compare+return plus a cheap window eval every 20K conflicts.
	const adaptRestartPeriod = 10
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
	// Offset/cursor arrays are int64: total binary-implication edges
	// (= 2 x #binary clauses) can exceed MaxInt32 on very large binary-heavy
	// instances, and the prefix-sum/offsets would silently overflow int32.
	// Adjacency targets stay int32 (literal indices < numLits always fit).
	off := make([]int64, numLits+1)
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
	data := make([]int32, int(off[numLits]))
	cur := make([]int64, numLits)
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
		data[int(cur[a^1])] = int32(b)
		cur[a^1]++
		data[int(cur[b^1])] = int32(a)
		cur[b^1]++
	}
	s.bigAdjData = data
	s.bigAdjOff = off
	// Learned-binary adjacency: per-literal growable slices supplementing the
	// static CSR. Heuristically cap total edges to bound memory on grinders;
	// exceeding the cap only reduces fast-path effectiveness (never soundness).
	s.bigLearnAdj = nil
	if s.bigLearnFlag {
		s.bigLearnAdj = make([][]int32, numLits)
	}
	s.bigLearnEdgeCount = 0
	s.bigLearnEnabled = s.bigLearnFlag
	s.bigLearnAdded = 0
	if !s.bigLearnFlag {
		s.bigLearnCap = 0
		return
	}
	cap := 4_000_000
	if edgeCap := 32 * numLits; edgeCap > cap {
		cap = edgeCap
	}
	if cap > 16_000_000 {
		cap = 16_000_000
	}
	s.bigLearnCap = cap
}

// addLearnedBinaryToBIG registers the implication edges of a newly-learned
// binary clause (l0 ∨ l1): ¬l0 → l1 and ¬l1 → l0. Sound to retain forever even
// after the clause is deleted because learned clauses are permanent logical
// consequences of the formula; resolution over them stays valid.
func (s *CDCLSolver) addLearnedBinaryToBIG(l0, l1 cnf.Literal) {
	if !s.bigLearnEnabled || s.bigLearnAdj == nil {
		return
	}
	a := cnf.LitToIndex(l0)
	b := cnf.LitToIndex(l1)
	if a == b || a == b^1 {
		return
	}
	if s.bigLearnEdgeCount >= s.bigLearnCap {
		return // cap reached — stop growing (effectiveness only)
	}
	s.bigLearnAdj[a^1] = append(s.bigLearnAdj[a^1], int32(b))
	s.bigLearnEdgeCount++
	s.bigLearnAdj[b^1] = append(s.bigLearnAdj[b^1], int32(a))
	s.bigLearnEdgeCount++
	s.bigLearnAdded++
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
	maxVisited := s.bigBfsBudget
	if maxVisited <= 0 {
		maxVisited = 16
	}
	visitedCount := 0
	bigLearnAdj := s.bigLearnAdj
	for head < len(q) && !found {
		cur := q[head]
		head++
		start := int(bigAdjOff[cur])
		end := int(bigAdjOff[cur+1])
		process := func(m int) bool {
			if visited[m] == ep {
				return false
			}
			visited[m] = ep
			visitedCount++
			mVar := uint32(m >> 1)
			if mVar != litVar && tmpInClause[mVar] && tmpIsNeg[mVar] == ((m&1) == 1) {
				return true
			}
			if visitedCount < maxVisited {
				q = append(q, m)
			}
			return false
		}
		for i := start; i < end && !found; i++ {
			found = process(int(bigAdjData[i]))
		}
		if !found && bigLearnAdj != nil && cur < len(bigLearnAdj) && len(bigLearnAdj[cur]) > 0 {
			for _, m := range bigLearnAdj[cur] {
				if found = process(int(m)); found {
					break
				}
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

		// In-processing fallback (D): the primary firing site is co-scheduled at
		// restart boundaries (see restart()), sharing the existing level-0 reset.
		// This loop fallback fires on a long RESTART-FREE stretch when the
		// trigger (A: enough new root units since last round) AND the adaptive
		// conflict gap (C) are both satisfied, forcing a level-0 reset on demand.
		// Level-0 is the true safety requirement (no clause in use as a reason).
		// Gated OFF by default (inprocessPeriod=0) until proven net-positive.
		if s.inprocessDue() {
			if s.level > 0 {
				s.cancelUntil(0)
			}
			if s.runInprocess() {
				s.printStats()
				return UNSAT
			}
			continue
		}

		// Det6: geometric->Luby rest-mechanism flip on conflict-window cadence
		// (independent of restart count, which geometric restarts starve).
		s.maybeGeoSpiral()

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

		if s.debugCC {
			s.assertTrueUnitComplete()
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

	// qhead is positioned by the trail-appenders, not reset here:
	//   - decide()  -> successful propagateWatched sets s.qhead = len(trail)
	//                 (:4792), which equals trailHead[level], covering the new
	//                 decision literal.
	//   - backtrack() -> s.qhead = decisionPoint (:6537), covering the asserting
	//                 literal and any unit-scan-appended elements.
	//   - cancelUntil() -> s.qhead = len(trail) (:3154) (restart/level-cap).
	//   - compactLearnedClauses() -> s.qhead = 0 (:3348), the watch-DB rebuild
	//                 safety net that forces full re-propagation.
	// Previously this function rewound qhead to trailHead[level] on entry, which
	// re-scanned the entire already-processed kept decision level after every
	// backjump (redundant re-propagation on every conflict). It was a no-op for
	// the decision path (where qhead already == trailHead[level]). Removing it
	// relies on the four setters above to cover all newly-appended trail
	// elements; correctness is asserted by the debugCC watch-completeness canary.
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
	watchListsBinary := s.watchListsBinary

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
	blitFast := s.blitFastHits
	binarySlow := s.binarySlow
	generalSlow := s.generalSlow
	generalMoves := s.generalMoves

	// Cache the trail slice header. trail grows via append below; the cached
	// header must be written back to s.trail at every return so subsequent
	// calls see the grown trail. Same aliasing rationale as assignments above.
	// Also cache s.level (constant for the whole call — no decisions/conflicts
	// occur inside this loop) and derive propLevel once. Root-level propagations
	// (s.level==0) get Level 0: 1-UIP correctly skips them as always-true.
	trail := s.trail
	level := s.level
	propLevel := level
	// Slow-path DB slice headers are cached once at function entry. Serving the
	// slow path (after the blit check fails) from these locals removes the per
	// slow-path-entry method calls (GetOriginalClauseLocs/GetLiteralPool) and
	// slice-header re-fetches. Empirically bit-identical (~5.5% PAR2 on the fast
	// suite) — the Go compiler keeps the fast-path registers live (litValueBase,
	// watchList, readIdx), so the extra locals do NOT spill them here (the old
	// all-at-unsafe-entry hoist was a different, regressing arrangement).
	origClauseLocs := s.cnf.GetOriginalClauseLocs()
	origLiteralPool := s.cnf.GetLiteralPool()
	numOriginalClauses := len(origClauseLocs)
	origSearchHint := s.originalSearchHint
	lrnLoc := s.learnedLoc
	lrnLiterals := s.learnedLiterals
	lrnSearchHint := s.learnedSearchHint

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

		// Process watches for this literal using write-pointer compaction: each
		// surviving watch is copied forward to wIdx as the scan advances; moved
		// watches are skipped. On a conflict-return the never-scanned suffix is
		// shifted down (never dropped) so no live watch is lost. Bounds-check-free
		// via pointer base: reads span [0,len), writes only to wIdx<=readIdx.
		//
		// Binary (size-2) and general (size>=3) clauses live in SEPARATE per-
		// literal lists (watchListsBinary vs watchLists). Region membership is
		// immutable (a clause never changes size), so a watch never crosses lists
		// and each region is scanned with a specialized body: the binary region
		// needs no clause-data load and no replacement search; the general region
		// needs no binary-bit test or size-2 handling.
		const wSize = 8 // cnf.Watch{ClauseIdx int32; Blit uint32}

		// ---- BINARY PASS ----
		binList := watchListsBinary[watchIdx]
		bLen := len(binList)
		bIdx := 0
		var binBase unsafe.Pointer
		if bLen > 0 {
			binBase = unsafe.Pointer(&binList[0])
		}

		for readIdx := 0; readIdx < bLen; readIdx++ {
			watch := *(*cnf.Watch)(unsafe.Add(binBase, readIdx*wSize))

			// FAST PATH: Blit stores the litTrue index directly (varIdx*2 + negated).
			// unsafe.Add skips the bounds check — the invariant Blit < len(litValue)
			// always holds (Blit = varIdx*2+negated, varIdx < NumVars).
			if *(*bool)(unsafe.Add(litValueBase, watch.Blit)) {
				blitFast++
				*(*cnf.Watch)(unsafe.Add(binBase, bIdx*wSize)) = watch
				bIdx++
				continue
			}

			// BINARY SLOW PATH: For a size-2 clause the Blit field already holds the
			// blocking literal. No replacement search is possible (only 2 literals)
			// and no clause-data load is needed except on the rare conflict branch.
			binarySlow++
			// Bit 31: learned flag, bit 30: myPos, bit 29: binary, bits 0-28: index.
			clauseIdxRaw := uint32(watch.ClauseIdx)
			isLearned := clauseIdxRaw&watchLearnedBit != 0
			clauseID := int(clauseIdxRaw & watchIdxMask)
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
				*(*cnf.Watch)(unsafe.Add(binBase, bIdx*wSize)) = watch
				bIdx++
				continue
			}

			// Assigned — check for conflict (blit is false since fast path already
			// filtered out the true case). Build conflict clause (rare path — clause
			// data load OK here).
			if level == 0 {
				s.emptyClauseFound = true
			}
			if !isLearned {
				originalClauseLocs := origClauseLocs
				originalLiteralPool := origLiteralPool
				loc := originalClauseLocs[clauseID]
				s.conflictClauseBuf.Literals = originalLiteralPool[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
				s.conflictClauseBuf.Learned = false
			} else {
				learnedLoc := s.learnedLoc
				learnedLiterals := s.learnedLiterals
				loc := learnedLoc[clauseID]
				s.conflictClauseBuf.Literals = learnedLiterals[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
				s.conflictClauseBuf.Learned = true
			}
			// The conflicting watch survives — write it into the compacted list.
			*(*cnf.Watch)(unsafe.Add(binBase, bIdx*wSize)) = watch
			bIdx++
			// Never-scanned suffix [readIdx+1, bLen) preserved on early return:
			// shift it down after the compacted survivors so no live watch drops.
			// copy() is a single memmove (dst starts at bIdx <= readIdx+1).
			n := bIdx + copy(binList[bIdx:], binList[readIdx+1:bLen])
			binList = binList[:n]
			watchListsBinary[watchIdx] = binList
			s.propagations = propagations
			s.numUnassigned = numUnassigned
			s.trail = trail
			s.blitFastHits = blitFast
			s.binarySlow = binarySlow
			s.generalSlow = generalSlow
			s.generalMoves = generalMoves
			return true, &s.conflictClauseBuf
		}
		// Normal completion: whole binary list scanned, compacted [0,bIdx) complete.
		watchListsBinary[watchIdx] = binList[:bIdx]

		// ---- GENERAL PASS ----
		watchList := watchLists[watchIdx]
		wlLen := len(watchList)
		wIdx := 0
		var wlBase unsafe.Pointer
		if wlLen > 0 {
			wlBase = unsafe.Pointer(&watchList[0])
		}

		for readIdx := 0; readIdx < wlLen; readIdx++ {
			watch := *(*cnf.Watch)(unsafe.Add(wlBase, readIdx*wSize))

			// FAST PATH: Blit stores the litTrue index directly (varIdx*2 + negated).
			// unsafe.Add skips the bounds check — the invariant Blit < len(litValue)
			// always holds (Blit = varIdx*2+negated, varIdx < NumVars).
			if *(*bool)(unsafe.Add(litValueBase, watch.Blit)) {
				blitFast++
				*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
				wIdx++
				continue
			}

			// SLOW PATH: Blocking literal is not true (or unassigned).
			// Access clause data for replacement search / conflict detection.
			//
			// A1: Slow-path-only slice headers are loaded lazily (see below), not
			// at function entry. This keeps the fast-path inner loop free of the
			// extra slice headers that would otherwise force litValueBase/watchList/
			// readIdx to spill to the stack.
			//
			// Decode ClauseIdx (no header loads — pure bit ops). This is a GENERAL
			// (size>=3) watch — binary clauses live in the separate watchListsBinary
			// pass above — so no binary-bit test and no size-2 propagation body.
			// Bit 31: learned flag, bit 30: myPos, bit 29: binary, bits 0-28: clause index.
			clauseIdxRaw := uint32(watch.ClauseIdx)
			isLearned := clauseIdxRaw&watchLearnedBit != 0
			clauseID := int(clauseIdxRaw & watchIdxMask)

			generalSlow++
			// NON-BINARY clause: all six slow-path headers are already cached in
			// function-entry locals (origClauseLocs, origLiteralPool, ...).
			originalClauseLocs := origClauseLocs
			originalLiteralPool := origLiteralPool
			originalSearchHint := origSearchHint
			learnedLoc := lrnLoc
			learnedLiterals := lrnLiterals
			learnedSearchHint := lrnSearchHint

			var clauseLits []cnf.Literal
			if !isLearned {
				if clauseID >= numOriginalClauses {
					*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
					wIdx++
					continue
				}
				loc := originalClauseLocs[clauseID]
				clauseLits = originalLiteralPool[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
			} else {
				loc := learnedLoc[clauseID]
				if int(loc.Size) == 0 {
					*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
					wIdx++
					continue
				}
				clauseLits = learnedLiterals[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
			}

			// Re-read the actual blocking literal from clause data (Blit may be stale).
			// The fast-path Blit check already filtered out the true case; here we
			// need the actual literal for the replacement guard, propagation, and
			// conflict detection. myPos/blitPos are derived only from clauseIdxRaw
			// and are consumed ONLY on this non-binary path, so decode them here
			// (not before the binary fast-path check above).
			myPos := int((clauseIdxRaw >> 30) & 1)
			blitPos := 1 - myPos
			blitLit := clauseLits[blitPos]
			blitVarIdx := int(blitLit.Var())
			blitNegated := blitLit.IsNegated()

			// Look for replacement watch
			foundReplacement := false
			var trueReplacementLit uint32 // 0 = none found; else litTrue index of true literal to cache as Blit
			foundJ := -1
			var newWatchIdx int

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
					foundJ = int(hint)
					newWatchIdx = clauseLitVar << 1
					if litNegated {
						newWatchIdx |= 1
					}
				} else {
					litTrue := litNegated != clauseAsg.Value
					if litTrue {
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
				s.moveHintMiss++
				// Resume the scan past the known-non-good frontier instead of
				// restarting at 2. searchHint records the frontier: everything in
				// [2, hint) was verified non-good when the last move happened, so
				// [hint, n) is scanned first, then wraps to cover [2, hint) for
				// completeness (slots 0/1 are the two watched positions and are
				// never part of the replacement domain).
				nLits := len(clauseLits)
				start := 2
				if int(hint) >= 2 && int(hint) < nLits {
					start = int(hint)
				}

				// Bounded prefer-true hunt, restricted to learned clauses: learned
				// clauses are short so the cap cost is trivially bounded (no long
				// original-clause scan bloat), and they are the propagation hot
				// ones where parking a satisfied watch pays. For original clauses
				// keep the classic first-non-false policy. Within preferTrueCap
				// positions (wrap order) prefer a currently-TRUE literal: a true
				// watch is "parked" (satisfied, stable until backtrack) so it is
				// not re-visited/re-scanned. Track the first unassigned literal
				// (the classic live unit candidate) as a fallback; it is used once
				// the hunt budget is spent.
				var firstUnassignedJ int
				firstSet := false
				searchN := nLits - 2
				limit := 0
				if s.preferTrueCap > 0 && isLearned {
					limit = searchN
					if s.preferTrueCap < limit {
						limit = s.preferTrueCap
					}
				}
				for k := 0; k < limit; k++ {
					j := start + k
					if j >= nLits {
						j -= searchN // wrap within [2, nLits)
					}
					s.moveScanLits++
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
						foundJ = j
						newWatchIdx = clauseLitVar << 1
						if litNegated {
							newWatchIdx |= 1
						}
						break
					}
					if !firstSet {
						firstSet = true
						firstUnassignedJ = j
					}
				}
				// Any unassigned seen (within or past the budget for short
				// clauses) is a valid replacement; prefer true was already
				// exhaustively checked above.
				if foundJ < 0 && firstSet {
					foundJ = firstUnassignedJ
					newWatchIdx = int(clauseLits[firstUnassignedJ].Var()) << 1
					if clauseLits[firstUnassignedJ].IsNegated() {
						newWatchIdx |= 1
					}
				}
				// Completeness: if the budget ran out before reaching any
				// unassigned/true literal (a long clause whose leading literals are
				// all false), resume the classic first-non-false full-domain scan so
				// a valid replacement is found wherever it lives, and so that a
				// clause with no non-false replacement still falls through to
				// unit/conflict handling below.
				if foundJ < 0 && !firstSet && limit < searchN {
					for j := start; j < nLits; j++ {
						s.moveScanLits++
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
					if foundJ < 0 {
						for j := 2; j < start; j++ {
							s.moveScanLits++
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

				dst := watchLists[newWatchIdx]
				if cap(dst) == len(dst) {
					s.moveReallocCast++
				}
				watchLists[newWatchIdx] = append(dst, cnf.Watch{
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
				s.numWatchMoves++
				generalMoves++
				// Watch moved to another literal's list — drop it here (slot skipped).
				continue
			}

			// No replacement found - check if we can propagate or have conflict
			blitAsg := assignments[blitVarIdx]

			if blitAsg.Level < 0 {
				// Unassigned blit - propagate it (inlined assignLiteralByClause)
				if s.debugCC {
					// Completeness canary: propagating var blitVarIdx via clauseID is
					// sound ONLY if every other literal is already false. If any is not
					// false (unassigned or true), a watch was missed upstream and the
					// reason clause would be inconsistent -> 1-UIP fails to converge.
					for j, lit := range clauseLits {
						if j == blitPos {
							continue
						}
						asg := assignments[lit.Var()]
						if asg.Level < 0 || (lit.IsNegated() != asg.Value) {
							fmt.Fprintf(os.Stderr, "CC-HOLE: prop var %d via clause %d (lrn=%v) but lit %v (var %d lvl %d val %v) not false; lits=%v myPos=%d blitPos=%d\n",
								blitVarIdx, clauseID, isLearned, lit, lit.Var(), asg.Level, asg.Value, clauseLits, myPos, blitPos)
							panic("completeness hole: unit-implied literal has a non-false antecedent")
						}
					}
				}
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
				*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
				wIdx++
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
					s.conflictClauseBuf.Literals = literals
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
				// The conflicting watch survives — write it into the compacted list.
				*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
				wIdx++
				// Never-scanned suffix [readIdx+1, wlLen) preserved on early return:
				// shift it down after the compacted survivors so no live watch drops.
				// copy() is a single memmove (dst starts at wIdx <= readIdx+1).
				n := wIdx + copy(watchList[wIdx:], watchList[readIdx+1:wlLen])
				watchList = watchList[:n]
				watchLists[watchIdx] = watchList
				s.propagations = propagations
				s.numUnassigned = numUnassigned
				s.trail = trail
				s.blitFastHits = blitFast
				s.binarySlow = binarySlow
				s.generalSlow = generalSlow
				s.generalMoves = generalMoves
				return true, conflictClause
			}
			// Actual blit literal evaluated TRUE (top-of-loop cached Blit was stale)
			// -> clause satisfied, this watch survives silently. Must be written or
			// loop-end truncation drops a live watch (missed unit).
			*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
			wIdx++
		}
		// Normal completion: whole list scanned, compacted [0,wIdx) is complete.
		watchLists[watchIdx] = watchList[:wIdx]
	}

	// Update qhead to end of trail
	s.qhead = len(trail)
	s.trail = trail
	s.propagations = propagations
	s.numUnassigned = numUnassigned
	s.blitFastHits = blitFast
	s.binarySlow = binarySlow
	s.generalSlow = generalSlow
	s.generalMoves = generalMoves

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

	// Resolve on candidates until 1 UIP remains. The O(current-level trail)
	// reverse-scan below feeds ONLY the resolution loop (which runs only when
	// currentCount > 1). When currentCount <= 1 the conflict is already
	// asserting/non-asserting and the candidate list is never read, so gate the
	// scan inside the branch to avoid the per-conflict waste on the dominant path.
	if currentCount > 1 {
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
	}
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

	// FIX: If 1-UIP didn't converge (currentCount > 1) after exhausting all
	// candidates, check if remaining literals are all decisions or if some are
	// propagations with buggy reasons. The decisions/propagations counts are
	// consumed ONLY by the fallback diagnostic (logUIPFallback), and the fallback
	// is empirically never entered (uipFallback==0 across the 72-suite and all
	// held-out cnfgen families; the held-out gate FAILs any family that fires it).
	// So the O(touched) counting scan is behavior-neutral waste on the dominant
	// path: gate it inside the fallback branch to eliminate the per-conflict cost.
	if currentCount > 1 {
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
		s.uipFallbackCount++
		// 1-UIP did not converge: there is more than one literal at the current
		// decision level still in the clause after resolution. In a correct CDCL
		// this never happens (each level has exactly one decision, so resolving
		// non-decision literals always reduces to that single decision = the UIP).
		// Non-convergence therefore indicates inconsistent reason clauses (e.g. a
		// propagated literal whose reason contains an unassigned literal, or more
		// than one decision at one level), which were skipped above.
		//
		// FAIL-SAFE: We do NOT try to force a 1-UIP by dropping the remaining
		// current-level literals. The dropped literals would not be logically
		// implied unless every reason clause is consistent — but non-convergence
		// is precisely the signature of *inconsistent* reason clauses, so pruning
		// them can produce an un-entailed learned clause that later rules out a
		// satisfying assignment and turns a satisfiable formula into a false
		// UNSAT. The safe action is to leave currentCount > 1 and let the caller
		// (learnClause) skip learning entirely and backjump conservatively.
		if s.verbose {
			s.Log("c [1-UIP] FALLBACK: %d decisions + %d propagations at level %d -> NO learn\n",
				decisionsAtCurrentLevel, propagationsAtCurrentLevel, s.level)
		}

		// Diagnostic: dump residual-literal state. Build trailPos for the
		// current-level trail slice (O(current-level trail)) for O(1) position
		// lookup, then clear it after the dump.
		startIdx := s.trailHead[s.level]
		for ti := startIdx; ti < len(s.trail); ti++ {
			s.trailPos[s.trail[ti]] = ti
		}
		s.logUIPFallback(decisionsAtCurrentLevel, propagationsAtCurrentLevel, startIdx)
		for ti := startIdx; ti < len(s.trail); ti++ {
			s.trailPos[s.trail[ti]] = 0
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
	// Must run before the currentCount != 1 early return so degenerate
	// conflicts still bump involved variables.
	if s.useBumpAnalyze {
		s.vsids.bumpAnalyze(s.tmpTouchedVars)
	} else {
		s.vsids.bumpClause(conflictLits)
	}

	// NOTE: currentCount != 1 means we do NOT have a genuine 1-UIP asserting
	// clause, so we skip learning and backjump conservatively:
	//   - currentCount == 0: resolution canceled all literal at the current
	//     decision level -> non-asserting, no literal to propagate. Skip.
	//   - currentCount > 1: resolution did not converge, i.e. reason clauses
	//     were inconsistent. Learning the partially-derived clause would risk
	//     an un-entailed clause (false UNSAT), so skip it (see runOneUIPResolution).
	// The produced clause (sound or not) would be non-asserting anyway, so
	// skipping it loses nothing real; backjump is always sound.
	if currentCount != 1 {
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
	// Build learned clause — exclude level-0 literals (always-true root facts).
	// Level-0 literals are preprocessing assignments and root-level learned units.
	// They are always true during search, so including them in learned clauses
	// wastes storage and watch slots without adding constraint value.
	//
	// LBD/maxLevel accumulation and learned-literal construction iterate the same
	// tmpLiteralInClause-filtered touched set and read the same assignments[].Level,
	// so fold them into a single pass (compute lvl once) instead of two O(touched)
	// scans per conflict.
	s.tmpLearnedLits = s.tmpLearnedLits[:0]
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			lvl := int(s.assignments[varIdx].Level)
			s.tmpLiteralInClause[varIdx] = false
			if lvl > 0 {
				if !s.tmpLevelSetUsed[lvl] {
					s.tmpLevelSetUsed[lvl] = true
					s.tmpLevelSet = append(s.tmpLevelSet, lvl)
					lbd++
				}
				s.tmpLearnedLits = append(s.tmpLearnedLits, cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx]))
			}
			if lvl > maxLevel && lvl < s.level {
				maxLevel = lvl
			}
		}
	}

	// NOTE: resolvedCount == 0 is normal when all conflict literals are decisions.
	// The learned clause IS useful - it prevents this exact combination of decisions.
	// Do NOT skip learning in this case - that would cripple the solver.

	// CRITICAL: Verify 1-UIP property to catch soundness bugs
	// If verification fails, the learned clause is invalid - skip learning (safer than wrong clause)
	// This point is reached only for a genuine 1-UIP (currentCount == 1).
	if !s.verifyLearnedClause(s.tmpLearnedLits, false) {
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
	// Disabled for clauses ≤2 literals (already minimal). originalSize and
	// originalLBD feed ONLY the verbose minimize log inside this branch, so
	// capture them here rather than per-learn unconditionally.
	//
	// A/B LBD gate (SetMinimizeLBDGate): skip the expensive BIG BFS + recursive
	// minimization for high-LBD clauses that reduceDB will evict anyway. Falling
	// through leaves the clause unminimized (higher LBD, deleted sooner) — always
	// sound, only trajectory/quality change. Low-LBD clauses are unaffected.
	// The gate must cover the whole block (including the post-minimize LBD
	// recompute) so lbd stays the unminimized (pre-minimize) value when skipped.
	if len(s.tmpLearnedLits) > 2 &&
		(s.minimizeLBDGate <= 0 || lbd <= s.minimizeLBDGate) {
		originalSize := len(s.tmpLearnedLits)
		originalLBD := lbd
		s.tmpLearnedLits = s.minimizeLearnedClause(s.tmpLearnedLits)

		// Recalculate LBD after minimization (CRITICAL - LBD may have decreased)
		// Must reset tmpLevelSetUsed since it was used for original LBD calculation.
		// Reset only the levels that were marked (tracked in tmpLevelSet), avoiding
		// an O(NumVars) sweep per conflict on the hot learnClause path.
		lbd = 0
		maxLevel = 0
		for _, lvl := range s.tmpLevelSet {
			s.tmpLevelSetUsed[lvl] = false
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
	// Glue clause (LBD ≤ 2) counter, consumed by the behavioral governor.
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
		// Register learned binary clauses into the dynamic BIG for stronger
		// transitive minimization (sound even after deletion — see
		// addLearnedBinaryToBIG).
		if len(literals) == 2 && !s.bigDisabled {
			s.addLearnedBinaryToBIG(literals[0], literals[1])
		}

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
					s.appendWatch(idx0, cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit1)}, true)
					s.appendWatch(idx1, cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit0)}, true)
				} else {
					s.appendWatch(idx0, cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit1)}, false)
					s.appendWatch(idx1, cnf.Watch{ClauseIdx: clauseIdx1, Blit: litToBlit(lit0)}, false)
				}
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
			s.rootUnitsLearned++ // trigger A: newly-discovered root fact
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
		if s.bigAdjOff != nil && !s.bigDisabled &&
			!(s.structureScore < 0.7 && s.binaryRatio > 0.4) { // GATE: disable BIG for mixed-binary sub-0.7
			s.bigMinimizeCalls++
			if s.bigReachableInClause(lit) {
				s.bigMinimizeHits++
				s.recordBigOutcome(true)
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
			// Sliding-window hit-rate gate: disable BIG once the recent hit rate
			// (measured over the last bigHitWindow attempts) falls below
			// bigMinHitRate. The old cumulative gate only fired at total-hits==0,
			// so a low-but-nonzero hit rate (e.g. 0.5% on large timetabling
			// encodings) kept the ~99.5%-overhead BFS alive for the whole solve.
			s.recordBigOutcome(false)
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
	if s.dbCapFactor > 0 && s.dbCapFactor != 1.0 {
		dynamicLimit = int(float64(dynamicLimit) * s.dbCapFactor)
	}
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

	// Pass 0 (length gate): if dbMaxLen>0, evict the longest clauses first.
	// The LBD passes below can never remove long near-glue clauses (LBD ≤ tier2
	// never deleted), so on long-clause instances the DB fills with them and
	// propagation gets slow. This pass prioritizes evicting any clause longer
	// than dbMaxLen. It is a deletion-time-only preference: the asserting learned
	// clause is still always added after a conflict, so search always progresses.
	//
	// Gated on binary-heavy formulas (binaryRatio > 0.5): these are formulas
	// (e.g., pigeonhole, parity) whose ORIGINAL clauses are small but whose
	// LEARNED DB bloats with long redundant near-glue clauses — pruning them is
	// pure win. Long-clause formulas (e.g. 274099073, ~99% long originals) need
	// their long learned clauses to drive search, so the gate must not touch
	// them. binaryRatio is cached by classifyInstance (runs before solving).
	if s.dbMaxLen > 0 && s.binaryRatio > 0.5 {
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedLoc[i].Size == 0 || protected[i] {
				continue
			}
			if int(s.learnedLoc[i].Size) > s.dbMaxLen {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) > 1 {
			sort.Slice(candidates, func(a, b int) bool {
				ia, ib := candidates[a], candidates[b]
				sa, sb := int(s.learnedLoc[ia].Size), int(s.learnedLoc[ib].Size)
				if sa != sb {
					return sa > sb // longest first
				}
				return ia < ib
			})
		}
		for _, idx := range candidates {
			if deletedCount >= toDelete {
				break
			}
			deleted[idx] = true
			deletedCount++
		}
		candidates = candidates[:0]
	}

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

		bl := s.watchListsBinary[litIdx]
		writeIdx = 0
		for _, watch := range bl {
			if watch.ClauseIdx >= 0 {
				bl[writeIdx] = watch
				writeIdx++
			}
		}
		s.watchListsBinary[litIdx] = bl[:writeIdx]
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
			s.appendWatch(idx0, cnf.Watch{
				ClauseIdx: clauseIdx0,
				Blit:      litToBlit(lit1),
			}, true)
			s.appendWatch(idx1, cnf.Watch{
				ClauseIdx: clauseIdx0,
				Blit:      litToBlit(lit0),
			}, true)
		} else {
			s.appendWatch(idx0, cnf.Watch{
				ClauseIdx: clauseIdx0,
				Blit:      litToBlit(lit1),
			}, false)
			s.appendWatch(idx1, cnf.Watch{
				ClauseIdx: clauseIdx1,
				Blit:      litToBlit(lit0),
			}, false)
		}

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
	// Only mark unitsDirty if we actually unassign a variable that is the target
	// of a learned unit clause (Reason encodes -learnedIdx-5 for a size-1 clause).
	// Reading Reason BEFORE unassignVar (which resets it to -1) is required.
	// This avoids an O(unitLearnedList) re-scan on every conflict when no learned
	// unit became unassigned (typically all units sit at root/level 0 and survive
	// a backjump unchanged).
	var unitVarUnassigned bool
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := s.trail[i]
		reason := s.assignments[varIdx].Reason
		if reason <= -5 {
			ui := int(-reason - 5)
			if ui < len(s.learnedLoc) && s.learnedLoc[ui].Size == 1 {
				unitVarUnassigned = true
			}
		}
		s.unassignVar(varIdx)
		s.vsids.onUnassign(varIdx)
	}
	if unitVarUnassigned {
		s.unitsDirty = true
	}
	s.trail = s.trail[:decisionPoint]
	s.qhead = decisionPoint
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel

	return true
}
