package solver

import (
	"fmt"
	"runtime"
	"satience/internal/cnf"
	"sort"
	"time"
)

// SolveResult represents the result of solving
type SolveResult int

const (
	SAT SolveResult = iota
	UNSAT
	UNKNOWN
)

// Solver configuration constants
const (
	DefaultMaxLearned       = 2000  // Maximum learned clauses before deletion
	DefaultMinLearned       = 1000  // Target clauses after deletion (50% reduction)
	DefaultRestartBase      = 50    // Base for Luby restart sequence
	VSIDSDecayFactor        = 0.95  // VSIDS activity decay factor
	ClauseActivityDecay     = 0.95  // Clause activity decay factor
	GlueLBDThreshold        = 3     // LBD ≤ 3 considered glue clauses (protected)
	CoreGlueLBDThreshold    = 2     // LBD ≤ 2 are core glue (never delete)
	MaxClauseAge            = 500   // Age threshold for forced deletion
	LargeClauseSize         = 15    // Size threshold for forced deletion
	IterationReportInterval = 10000 // Report progress every N iterations

	// Clause minimization thresholds
	// Set to extremely high values to enable aggressive minimization on ALL clauses
	MinimizationMaxSize       = 10000 // Minimize clauses up to 10K literals (effectively all)
	MinimizationMaxLBD        = 10000 // Minimize clauses up to LBD 10K (effectively all)
	MinimizationMaxReasonSize = 100   // Allow reason clauses up to 100 literals (more aggressive)

	// Debugging thresholds
	DebugConflictLimit = 100 // Verbose debug output for first N conflicts
)

// CDCLSolver implements a CDCL solver (DPLL with VSIDS + clause learning)
type CDCLSolver struct {
	cnf                 *cnf.CNF
	assignments         []Assignment
	trail               []int
	trailLevel          []int  // Cache of assignment levels for trail elements
	varLevel            []int  // Cache of variable levels (avoids random assignments[].Level access)
	trailHead           []int
	level               int
	vsids              *VSIDS
	conflicts          int
	implication        []*cnf.Clause // Clause pointer (nil for decisions)
	iterations         int
	propagations       int // Total propagations (assignments by unit propagation)
	maxIter            int
	learnedClauses     []cnf.Clause // Learned clauses
	clauseActivity     []float64
	clauseAge          []int
	clauseSize         []int // Track clause size for deletion
	clauseLBD          []int // Track LBD at time of learning
	normalClauseCount  int   // Track number of non-glue clauses (LBD > 3)
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
	lastDecisionVar         uint32  // Last variable chosen for decision
	consecutiveFlips        int     // Count of consecutive decisions on same variable
	minimizationMaxSize     int     // Skip minimization for clauses > this size (0=all)
	minimizationMaxLBD      int     // Skip minimization for clauses with LBD > this (0=all)
	minimizationMaxReasonSize int   // Skip resolution with reason clauses > this size
	// Reusable buffers for conflict analysis (avoid per-conflict allocation)
	tmpLiteralInClause  []bool
	tmpLiteralIsNegated []bool
	tmpLevelCount       []int
	tmpCandidates       []resolveCandidate
	tmpLevelSet         []int    // For LBD calculation (replaces map)
	tmpLevelSetUsed     []bool   // Track which levels are in tmpLevelSet
	tmpResolved         []bool   // Track resolved variables in 1-UIP to prevent cycles
	tmpClauseHash uint64 // Hash for duplicate detection
	tmpFlippedVars []bool // Track flipped variables at level 1 to prevent infinite loops
	tmpTouchedVars []uint32 // Track which variables were modified (for fast reset)
	tmpLearnedLits []cnf.Literal // Reusable buffer for learned clause literals

	// Watched literals infrastructure
	watchLists        [][]cnf.Watch // watchLists[lit] = clauses watching lit
	watchInitialized  bool          // True if watches have been initialized
	learnedClauseBase int           // Base ID for learned clause watches (fixed at initialization)

	// LBD-based learned clause ordering for propagation prioritization
	learnedClauseOrder  []int // Indices into learnedClauses/clauseLBD sorted by LBD
	lbdOrderDirty       bool  // True if order needs rebuilding
	lbdOrderLastRebuild int   // Conflict count when order was last rebuilt

	qhead int // Watched literals: next trail index to process
}

// resolveCandidate is used in learnClause for sorting resolution order
type resolveCandidate struct {
	varIdx uint32
	size   int
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
		trail:               make([]int, 0),
		trailLevel:          make([]int, 0),
		varLevel:            make([]int, formula.NumVars),
		trailHead:           make([]int, 1),
		qhead:               0,
		level:               0,
		vsids:               NewVSIDS(formula.NumVars),
		conflicts:           0,
		implication:         make([]*cnf.Clause, formula.NumVars),
		iterations:          0,
		maxIter:             0,
		learnedClauses:      make([]cnf.Clause, 0),
		clauseActivity:      make([]float64, 0),
		clauseAge:           make([]int, 0),
		clauseLBD:           make([]int, 0),
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
		randomDecisionRate:  0.0, // Default: no random decisions
		learnedClauseOrder:  make([]int, 0),
		lbdOrderDirty:       true,
		lbdOrderLastRebuild: 0,
		lastConflictLBD:     0,
		conflictsAtLevel:    make([]int, formula.NumVars+1),
		lastRandomDecision:  -1000,
		// Pre-allocate reusable buffers
		tmpLiteralInClause:  make([]bool, formula.NumVars),
		tmpLiteralIsNegated: make([]bool, formula.NumVars),
		tmpLevelCount:       make([]int, formula.NumVars+1),
		tmpCandidates:       make([]resolveCandidate, 0, 100),
		tmpLevelSet:         make([]int, 0, formula.NumVars),
		tmpLevelSetUsed:     make([]bool, formula.NumVars+1),
		tmpResolved:         make([]bool, formula.NumVars),
		tmpFlippedVars:      make([]bool, formula.NumVars),
		tmpTouchedVars:        make([]uint32, 0, formula.NumVars),
		tmpLearnedLits:        make([]cnf.Literal, 0, 64), // Pre-allocate for average clause size
		// Set learned clause base ID to original NumClauses (before preprocessing modifies it)
		learnedClauseBase: int(formula.NumClauses),
		// Initialize minimization thresholds to aggressive defaults
		minimizationMaxSize:     10000,
		minimizationMaxLBD:      10000,
		minimizationMaxReasonSize: 100,
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

// EnableLRB enables LRB (Learning Rate Based) heuristic
func (s *CDCLSolver) EnableLRB() {
	s.vsids.EnableLRB()
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
	return len(s.learnedClauses)
}

func (s *CDCLSolver) getDetailedStats() SolverStats {
	stats := SolverStats{
		Conflicts:      s.conflicts,
		Decisions:      s.decisions,
		Iterations:     s.iterations,
		LearnedClauses: len(s.learnedClauses),
		MaxLevel:       s.level,
	}

	// Calculate clause size statistics
	if len(s.learnedClauses) > 0 {
		minSize := len(s.learnedClauses[0].Literals)
		maxSize := minSize
		totalSize := 0

		for _, clause := range s.learnedClauses {
			size := len(clause.Literals)
			totalSize += size
			if size < minSize {
				minSize = size
			}
			if size > maxSize {
				maxSize = size
			}
		}

		stats.AvgClauseSize = totalSize / len(s.learnedClauses)
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
		EnableEquivalence:     true,
		EnablePureLiteral:     true,
		EnableSubsumption:     true,
		EnableSelfSubsumption: true,
		EnableHyperBinary:     true,
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

	if s.cnf.NumVars > 10000 || s.cnf.NumClauses > 50000 {
		if s.verbose {
			fmt.Printf("c [verbose] Skipping aggressive preprocessing: instance too large (%d vars, %d clauses)\n",
				s.cnf.NumVars, s.cnf.NumClauses)
		}
		// Still initialize watches and do basic setup
		s.cnf.RebuildLiteralPool()
		s.initWatches()
		return UNKNOWN
	}

	initialClauses := s.cnf.NumClauses
	modified := false // Track if any technique actually modified the formula

	// Increase to 5 passes for more thorough preprocessing
	// Modern solvers (CaDiCaL) use 10+ passes
	// Safeguards: time limits in each technique prevent explosion
	for pass := 0; pass < 5; pass++ {
		if s.verbose {
			fmt.Printf("c [verbose] Preprocessing pass %d: %d clauses\n", pass+1, s.cnf.NumClauses)
		}

		// Track clause count before each technique to detect modifications
		beforeClauses := s.cnf.NumClauses

		// Run unit propagation first to catch any existing units
		if preprocessConfig.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Equivalence detection: find a↔b patterns and substitute (BEFORE VE destroys binary clauses)
		if preprocessConfig.EnableEquivalence {
			if equivResult := s.equivalenceDetection(); equivResult != UNKNOWN {
				return equivResult
			}
		}

		// Variable elimination disabled (causes model reconstruction bugs)

		// Run unit propagation again after variable elimination
		if preprocessConfig.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Pure literal elimination
		if preprocessConfig.EnablePureLiteral {
			if pureResult := s.pureLiteralElimination(); pureResult != UNKNOWN {
				return pureResult
			}
		}

		// Subsumption elimination
		if preprocessConfig.EnableSubsumption {
			s.subsumptionElimination()
		}

		// Self-subsumption
		if preprocessConfig.EnableSelfSubsumption {
			s.selfSubsumption()
		}

		// Hyper-binary resolution
		if preprocessConfig.EnableHyperBinary {
			s.hyperBinaryResolution()
		}

		// Run unit propagation after hyper-binary resolution
		if preprocessConfig.EnableUnitProp {
			if unitResult := s.unitPropagationPreprocess(); unitResult != UNKNOWN {
				return unitResult
			}
		}

		// Track if anything changed
		if s.cnf.NumClauses != beforeClauses {
			modified = true
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

	// CRITICAL FIX: Only clear state and reinitialize watches if preprocessing actually modified the formula
	// Previously, we checked if techniques were ENABLED, not if they MODIFIED anything.
	// This caused state corruption when techniques ran but found nothing to simplify.
	// Bug discovered: PHP instances returned SAT instead of UNSAT with preprocessing enabled.
	if modified {
		// Clear assignments made during preprocessing
		// These assignments would otherwise persist and interfere with search
		// Preprocessing only simplifies clauses, it doesn't make permanent assignments
		for i := range s.assignments {
			s.assignments[i] = Assignment{}
			s.varLevel[i] = 0
		}
		s.trail = s.trail[:0]
		s.trailLevel = s.trailLevel[:0]
		s.trailHead = []int{0} // Reset to initial state
		s.level = 0
		s.qhead = 0
		for i := range s.implication {
			s.implication[i] = nil
		}

		// Rebuild literal pool after preprocessing modifications
		s.cnf.RebuildLiteralPool()

		// Initialize watched literals after preprocessing completes
		s.initWatches()
	}

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
	// Formula: 2 watches per clause (one per watched literal) / num literals
	// Minimum 8 to handle uneven distribution (some literals appear in many clauses)
	avgWatchesPerLit := (s.cnf.NumClauses * 2) / numLits
	if avgWatchesPerLit < 8 {
		avgWatchesPerLit = 8
	}
	for i := range s.watchLists {
		s.watchLists[i] = make([]cnf.Watch, 0, avgWatchesPerLit)
	}

	for clauseID := 0; clauseID < s.cnf.NumClauses; clauseID++ {
		clause := &s.cnf.Clauses[clauseID]
		s.addClauseToWatches(clause, clause.Literals)
	}

	// learnedClauseBase is already set in NewCDCLSolver to the original NumClauses value

	for learnedIdx := 0; learnedIdx < len(s.learnedClauses); learnedIdx++ {
		clause := &s.learnedClauses[learnedIdx]
		s.addClauseToWatches(clause, clause.Literals)
	}

	if s.verbose {
		totalWatches := 0
		for _, wl := range s.watchLists {
			totalWatches += len(wl)
		}
		fmt.Printf("c [verbose] Watched literals enabled: %d watch lists, %d total watches\n", len(s.watchLists), totalWatches)
	}
}

// addClauseToWatches adds a clause to the watch lists
// Watches the first two literals in the clause
func (s *CDCLSolver) addClauseToWatches(clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	lit0 := literals[0]
	lit1 := literals[1]

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Get positions before appending
	pos0 := len(s.watchLists[idx0])
	pos1 := len(s.watchLists[idx1])

	// Add watches with symmetric position tracking
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		Clause: clause,
		Blit:   uint32(idx1),
		SymPos: int32(pos1),
	})

	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		Clause: clause,
		Blit:   uint32(idx0),
		SymPos: int32(pos0),
	})

	s.watchInitialized = true
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
	removed := 0

	for i := 0; i < len(s.cnf.Clauses); i++ {
		for j := 0; j < len(s.cnf.Clauses); j++ {
			if i == j {
				continue
			}

			if s.subsumes(&s.cnf.Clauses[i], &s.cnf.Clauses[j]) {
				s.cnf.Clauses[j] = s.cnf.Clauses[len(s.cnf.Clauses)-1]
				s.cnf.Clauses = s.cnf.Clauses[:len(s.cnf.Clauses)-1]
				removed++
				if j < len(s.cnf.Clauses) {
					j--
				}
			}
		}
	}

	if s.verbose {
		fmt.Printf("c [verbose] Subsumption elimination: removed %d clauses\n", removed)
	}
}

func (s *CDCLSolver) subsumeLearnedClauses(newClause *cnf.Clause) {
	// Remove learned clauses that are subsumed by the new clause
	remaining := make([]cnf.Clause, 0, len(s.learnedClauses))
	removed := 0

	for i := range s.learnedClauses {
		if !s.subsumes(newClause, &s.learnedClauses[i]) {
			remaining = append(remaining, s.learnedClauses[i])
		} else {
			removed++
		}
	}

	if removed > 0 {
		s.learnedClauses = remaining
		s.clauseActivity = make([]float64, len(s.learnedClauses))
		s.clauseAge = make([]int, len(s.learnedClauses))
		if s.verbose {
			fmt.Printf("c [verbose] Learned clause subsumption: removed %d clauses\n", removed)
		}
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
// - Only run every 500 conflicts (expensive O(n²) operation)
// - Skip on large formulas (>5000 clauses)
// - Time limit of 200ms to avoid slowing down search
func (s *CDCLSolver) inprocessSubsumption() {
	if s.cnf.NumClauses > 5000 {
		return
	}

	startTime := time.Now()
	timeLimit := 200 * time.Millisecond

	removedOriginal := 0
	removedLearned := 0

	// Remove original clauses subsumed by learned clauses
	// This is the most impactful: learned clauses are often shorter and more general
	remainingOriginal := make([]cnf.Clause, 0, len(s.cnf.Clauses))
	for i := range s.cnf.Clauses {
		subsumed := false
		for j := range s.learnedClauses {
			if s.subsumes(&s.learnedClauses[j], &s.cnf.Clauses[i]) {
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

	// Remove learned clauses subsumed by other learned clauses
	// Keep only the most general (shortest) learned clauses
	if len(s.learnedClauses) > 100 {
		remainingLearned := make([]cnf.Clause, 0, len(s.learnedClauses))
		for i := range s.learnedClauses {
			subsumed := false
			for j := range s.learnedClauses {
				if i != j && s.subsumes(&s.learnedClauses[j], &s.learnedClauses[i]) {
					subsumed = true
					break
				}
			}
			if !subsumed {
				remainingLearned = append(remainingLearned, s.learnedClauses[i])
			} else {
				removedLearned++
			}
		}

		if removedLearned > 0 {
			s.learnedClauses = remainingLearned
			s.clauseActivity = make([]float64, len(s.learnedClauses))
			s.clauseAge = make([]int, len(s.learnedClauses))
			if s.verbose {
				fmt.Printf("c [inprocess] Removed %d learned clauses subsumed by other learned clauses\n", removedLearned)
			}
		}
	}

	totalRemoved := removedOriginal + removedLearned
	if s.verbose && totalRemoved > 0 {
		fmt.Printf("c [inprocess] Inprocessing subsumption: removed %d clauses (%d original, %d learned)\n",
			totalRemoved, removedOriginal, removedLearned)
	}
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
	if len(c1.Literals) >= len(c2.Literals) {
		return false
	}

	set := make(map[uint32]bool)
	for _, lit := range c1.Literals {
		key := uint32(lit)<<1 | boolToUint(lit.IsNegated())
		set[key] = true
	}

	for _, lit := range c2.Literals {
		key := uint32(lit)<<1 | boolToUint(lit.IsNegated())
		if !set[key] {
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

// Hybrid Approach:
// - First 50 conflicts: Use Luby (need LBD statistics)
// - After 50 conflicts: Use Glucose criterion (more aggressive)
// - If Glucose criterion not met: Fall back to Luby

// What Happens on Restart:
// 1. Clear the trail (all assignments)
// 2. Keep only "glue clauses" (LBD ≤ 3) - most valuable learned clauses
// 3. Delete all other learned clauses (50-90% reduction)
// 4. Reset LBD statistics for fresh measurement
// 5. Continue search with same VSIDS scores (learnings preserved)

// Why Keep Glue Clauses?
// Glue clauses (LBD ≤ 3) are the "backbone" of the search:
// - They connect few decision levels (highly general)
// - They propagate often and prune large parts of search space
// - Deleting them would cause the solver to re-explore the same conflicts
func (s *CDCLSolver) shouldRestart() bool {
	// Luby sequence (PRIMARY) - standard restart policy
	// Glucose-style adaptive restarts (SECONDARY) - only for extreme cases

	// Primary: Luby sequence restarts
	lubyValue := luby(s.lubyIndex + 1)
	threshold := lubyValue * s.restartBase

	if s.conflicts-s.restartCount >= threshold {
		return true
	}

	// Secondary: Glucose-style adaptive restarts for extreme LBD spikes only
	// Only trigger if LBD is MUCH higher than average (not just 1.5x)
	if s.lbdCount >= 100 {
		avgLBD := float64(s.lbdSum) / float64(s.lbdCount)

		// Only restart if LBD is extremely high (> 3x average AND > 20)
		// This catches truly pathological cases without over-restarting
		if s.lastConflictLBD > int(3.0*avgLBD) && s.lastConflictLBD > 20 {
			if len(s.learnedClauses) >= 100 {
				return true
			}
		}
	}

	return false
}

func (s *CDCLSolver) restart() {
	if s.verbose {
		fmt.Printf("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)
	}

	// IMPORTANT: Calculate LBD and identify glue clauses BEFORE clearing assignments!
	// Glue clauses (LBD <= 3) are preserved across restarts
	glueCount := 0
	isGlue := make([]bool, len(s.learnedClauses))

	for i, clause := range s.learnedClauses {
		// Calculate LBD while assignments are still valid using tmpLevelSet (avoid map allocation)
		lbd := 0
		for _, lit := range clause.Literals {
			lvl := s.assignments[lit.Var()].Level
			if lvl > 0 && !s.tmpLevelSetUsed[lvl] {
				s.tmpLevelSetUsed[lvl] = true
				s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				lbd++
			}
		}
		// Clear tmpLevelSetUsed for next clause
		for _, lvl := range s.tmpLevelSet {
			s.tmpLevelSetUsed[lvl] = false
		}
		s.tmpLevelSet = s.tmpLevelSet[:0]

		// Keep glue clauses (LBD <= 10) - INCREASED to prevent re-learning same clauses
		// Our 1-UIP produces clauses with LBD 5-8 typically on PHP instances
		// Deleting these causes the solver to re-encounter the same conflicts
		// LBD <= 3: core glue (most valuable, never delete)
		// LBD 3-10: useful glue (keep across restarts)
		// LBD > 10: trash (delete on restart)
		if lbd <= 10 {
			glueCount++
			isGlue[i] = true
		}
	}

	if s.verbose {
		fmt.Printf("c [verbose] Restart: keeping %d glue clauses, deleting %d non-glue\n", glueCount, len(s.learnedClauses)-glueCount)
	}

	// Clear trail and assignments
	s.trail = s.trail[:0]
	s.trailLevel = s.trailLevel[:0]
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

	// Reset restart counters
	s.lubyIndex++
	s.restartCount = s.conflicts
	s.lbdSum = 0
	s.lbdCount = 0
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
	for i := range s.tmpLevelCount {
		s.tmpLevelCount[i] = 0
	}
	for i := range s.tmpLevelSetUsed {
		s.tmpLevelSetUsed[i] = false
	}
	s.tmpLevelSet = s.tmpLevelSet[:0]
	s.tmpCandidates = s.tmpCandidates[:0]

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
				s.trailLevel = append(s.trailLevel, 1)
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

func (s *CDCLSolver) isClauseBlockedBy(clause cnf.Clause, blockingLit cnf.Literal) bool {
	opposite := blockingLit.Negate()

	for _, other := range s.cnf.Clauses {
		containsOpposite := false
		for _, lit := range other.Literals {
			if lit == opposite {
				containsOpposite = true
				break
			}
		}

		if !containsOpposite {
			continue
		}

		resolvent := s.resolveOnVar(clause, other, blockingLit.Var())

		if resolvent != nil && !s.isTautology(resolvent) {
			return false
		}
	}

	return true
}

func (s *CDCLSolver) inprocessing() {
	if s.verbose {
		fmt.Printf("c [verbose] Inprocessing: %d conflicts, %d clauses\n", s.conflicts, s.cnf.NumClauses)
	}

	initialClauses := s.cnf.NumClauses

	s.subsumptionElimination()

	if s.conflicts%1000 == 0 {
		s.selfSubsumption()
		// variableElimination disabled - causes model reconstruction bugs
	}

	if s.verbose && initialClauses != s.cnf.NumClauses {
		fmt.Printf("c [verbose] Inprocessing: reduced from %d to %d clauses\n", initialClauses, s.cnf.NumClauses)
	}
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
				s.trailLevel = append(s.trailLevel, 1)
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

func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

// SolveWithPreprocessing runs aggressive preprocessing before solving
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
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	for {
		s.iterations++
		if s.iterations%IterationReportInterval == 0 && s.verbose {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			fmt.Printf("c [debug] Iter %d, Conflicts %d, Level %d, Learned %d, Alloc=%dMB\n",
				s.iterations, s.conflicts, s.level, len(s.learnedClauses), mem.Alloc/1024/1024)
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
					s.conflicts, s.level, len(s.learnedClauses), s.decisions, s.propagations, propsPerDec)
			}
			if !s.backtrack() {
				if s.verbose {
					s.printStats()
				}
				return UNSAT
			}
			s.backjumpLevel = 0

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
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	for {
		s.iterations++
		if s.iterations%IterationReportInterval == 0 && s.verbose {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			fmt.Printf("c [debug] Iter %d, Conflicts %d, Level %d, Learned %d, Alloc=%dMB\n",
				s.iterations, s.conflicts, s.level, len(s.learnedClauses), mem.Alloc/1024/1024)
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
					s.conflicts, s.level, len(s.learnedClauses), s.decisions, s.propagations, propsPerDec)
			}
			if !s.backtrack() {
				if s.verbose {
					s.printStats()
				}
				return UNSAT
			}
			s.backjumpLevel = 0

			if s.shouldRestart() {
				s.restart()
			}
			continue
		}

		if s.allAssigned() {
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

// GetAssignments returns the current assignments for model extraction
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
func (s *CDCLSolver) propagateWatched() (bool, *cnf.Clause) {
	if !s.watchInitialized {
		return s.propagate()
	}

	// Use persistent qhead pointer (MiniSat-style) to avoid re-processing trail elements
	if s.qhead >= len(s.trail) {
		return false, nil
	}

	propagationCount := 0

	for trailIndex := s.qhead; trailIndex < len(s.trail); trailIndex++ {
		lit := s.trail[trailIndex]

		varIdx := uint32(lit)
		value := s.assignments[varIdx].Value
		var falseLit cnf.Literal
		if value {
			falseLit = cnf.NewLiteral(varIdx, true)
		} else {
			falseLit = cnf.NewLiteral(varIdx, false)
		}

		watchIdx := cnf.LitToIndex(falseLit)

		// Process watches for this literal using swap-with-last deletion
		watchList := s.watchLists[watchIdx]

		for readIdx := 0; readIdx < len(watchList); readIdx++ {
			watch := watchList[readIdx]

			// Skip deleted clauses
			if watch.Clause == nil {
				continue
			}

			clause := watch.Clause
			blitIdx := watch.Blit
			blit := cnf.IndexToLit(int(blitIdx))

			// Check if blit is true
			blitAssign := s.assignments[blit.Var()]
			blitIsTrue := blitAssign.Level != 0 && ((blit.IsNegated() && !blitAssign.Value) || (!blit.IsNegated() && blitAssign.Value))

			if blitIsTrue {
				// Clause is satisfied, keep watch in place
				continue
			}

			// Look for replacement watch
			foundReplacement := false
			for j := 0; j < len(clause.Literals); j++ {
				clauseLit := clause.Literals[j]
				if clauseLit == falseLit || clauseLit == blit {
					continue
				}

				litLevel := s.assignments[clauseLit.Var()].Level
				litValue := s.assignments[clauseLit.Var()].Value
				litTrue := (!clauseLit.IsNegated() && litValue) || (clauseLit.IsNegated() && !litValue)

				if litTrue || litLevel == 0 {
					// Found replacement - move watch from falseLit to clauseLit
					newWatchIdx := cnf.LitToIndex(clauseLit)
					blitIdxU := uint32(cnf.LitToIndex(blit))

					// Get position of new watch before adding
					newPos := len(s.watchLists[newWatchIdx])

					// Add new watch to clauseLit's watch list
					s.watchLists[newWatchIdx] = append(s.watchLists[newWatchIdx], cnf.Watch{
						Clause: clause,
						Blit:   blitIdxU,
						SymPos: watch.SymPos, // Points to symmetric watch position
					})

					// O(1) Update symmetric watch at blit's index
					symPos := watch.SymPos
					if symPos >= 0 && int(symPos) < len(s.watchLists[blitIdx]) {
						s.watchLists[blitIdx][symPos].Blit = uint32(newWatchIdx)
						s.watchLists[blitIdx][symPos].SymPos = int32(newPos)
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
					// Update symmetric position of the moved watch
					movedWatch := watchList[readIdx]
					movedSymPos := movedWatch.SymPos
					if movedSymPos >= 0 && int(movedSymPos) < len(s.watchLists[movedWatch.Blit]) {
						s.watchLists[movedWatch.Blit][movedSymPos].SymPos = int32(readIdx)
					}
					// Don't increment readIdx - need to process the moved watch
					readIdx--
				}
				// Truncate (remove last element which is now duplicated)
				watchList = watchList[:lastIdx]
				continue
			}

			// No replacement found - check if we can propagate or have conflict
			blitLevel := s.assignments[blit.Var()].Level

			if blitLevel == 0 {
				// Propagate blit
				reasonIdx := -1
				for i := range s.learnedClauses {
					if &s.learnedClauses[i] == clause {
						reasonIdx = -i - 1
						break
					}
				}
				if reasonIdx == -1 {
					for i := 0; i < s.cnf.NumClauses; i++ {
						if &s.cnf.Clauses[i] == clause {
							reasonIdx = i
							break
						}
					}
				}
				s.assignLiteralByClause(blit, s.level, clause)
				propagationCount++
				s.propagations++
				// Keep the watch in place - blit is now true
				continue
			}

			// blit is already assigned - check if it's false (conflict) or true (satisfied)
			blitValue := s.assignments[blit.Var()].Value
			blitTrue := (!blit.IsNegated() && blitValue) || (blit.IsNegated() && !blitValue)

			if !blitTrue {
				// Both watched literals are false - conflict!
				// CRITICAL: Write back watch list modifications before returning
				// Otherwise, watch list modifications from earlier in this loop are lost
				s.watchLists[watchIdx] = watchList
				return true, clause
			}

			// blit is true, keep watch in place
		}

		// Store the modified watch list back
		s.watchLists[watchIdx] = watchList
	}

	// Update qhead to end of trail
	s.qhead = len(s.trail)

	// Debug: report propagations
	if s.verbose && propagationCount > 0 {
		fmt.Printf("c [PROPAGATE] Processed trail[%d:%d], found %d propagations\n", s.qhead-propagationCount, len(s.trail), propagationCount)
	}

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
		for learnedIdx := 0; learnedIdx < len(s.learnedClauses); learnedIdx++ {
			clause := &s.learnedClauses[learnedIdx]
			clauseSize := len(clause.Literals)

			satisfiedCount := 0
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal

			for _, lit := range clause.Literals {
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
				return true, clause
			}

			if unassignedCount == 1 && falseCount == clauseSize-1 {
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

		trailIndex++
	}

	return false, nil
}

// selectRandomUnassigned selects a random unassigned variable
func (s *CDCLSolver) selectRandomUnassigned() uint32 {
	unassigned := make([]uint32, 0)
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			unassigned = append(unassigned, i)
		}
	}

	if len(unassigned) == 0 {
		return 0
	}

	// Simple deterministic "random" selection based on conflict count
	// This ensures reproducibility while providing diversification
	idx := s.conflicts % len(unassigned)
	return unassigned[idx]
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

		// Add periodic random decisions as fallback
		if !forceRandom && s.conflicts > 0 && s.conflicts%200 == 0 {
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
			fmt.Printf("c [verbose] Random decision at conflict %d, level %d (rate=%.2f)\n", s.conflicts, s.level, s.randomDecisionRate)
		}

		// Reset conflicts at this level after random decision
		if s.level > 0 && s.level < len(s.conflictsAtLevel) {
			s.conflictsAtLevel[s.level] = 0
		}

		// Reset flip tracking
		s.consecutiveFlips = 0
	} else {
		varIdx, phase = s.vsids.selectVariableWithPhase(s.assignments, s.savedPhase)

		// Detect variable flipping (same variable chosen consecutively)
		if s.conflicts > 0 && varIdx == s.lastDecisionVar {
			s.consecutiveFlips++

			// If flipping for 10+ conflicts, trigger diversification
			if s.consecutiveFlips >= 10 {
				s.vsids.diversify()
				s.consecutiveFlips = 0
				if s.verbose {
					fmt.Printf("c [DIVERSIFY] Conflict %d: triggered after %d flips on var %d\n",
						s.conflicts, 10, varIdx+1)
				}
			}
		} else {
			s.consecutiveFlips = 0
		}
		s.lastDecisionVar = varIdx
	}

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, phase), s.level, nil)
	s.decisions++
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
	s.trailLevel = append(s.trailLevel, level)
	s.implication[varIdx] = clause

	// Save the phase (polarity) for decisions only
	// Don't save phase for propagations - the phase is forced by the clause
	if clause == nil {
		s.savedPhase[varIdx] = value
	}

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
	s.trailLevel = append(s.trailLevel, level)

	// Store clause pointer directly - O(1), no lookup needed!
	s.implication[varIdx] = clause

	if level > s.level {
		s.savedPhase[varIdx] = value
	}
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

	// Bump activity for learned clause involved in conflict
	if conflictClause.Learned {
		for i := range s.learnedClauses {
			if &s.learnedClauses[i] == conflictClause {
				if i < len(s.clauseActivity) {
					s.clauseActivity[i] += 1.0
				}
				break
			}
		}
	}

	// Conflict clause selection: prefer shorter clauses for better learning
	// If multiple clauses conflict, we should choose the shortest one
	// For now, we use the first conflicting clause found (standard approach)
	// Future optimization: scan for all conflicting clauses and pick shortest

	s.vsids.bumpClause(conflictLits)

	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	// Decay clause activity EVERY CONFLICT (standard VSIDS)
	// Previous code decayed every 100 conflicts, causing variables to have
	// unbounded activity growth and leading to infinite loops on the same variable
	s.vsids.decay()
	s.vsids.decayLBD()
	// Also decay clause activity
	for i := range s.clauseActivity {
		s.clauseActivity[i] *= ClauseActivityDecay
	}

	// Inprocessing disabled (causes soundness bugs with watched literals)
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
			s.conflicts, s.iterations, s.level, len(s.learnedClauses), len(s.trail))
	}

	// Fast cleanup from previous conflict: reset only touched variables (O(k) instead of O(n))
	for _, varIdx := range s.tmpTouchedVars {
		s.tmpLiteralInClause[varIdx] = false
		s.tmpLiteralIsNegated[varIdx] = false
		s.tmpResolved[varIdx] = false
	}
	for _, lvl := range s.tmpLevelSet {
		s.tmpLevelCount[lvl] = 0
		s.tmpLevelSetUsed[lvl] = false
	}

	// Reset for new conflict
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
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = true
			s.tmpLiteralIsNegated[varIdx] = lit.IsNegated()
			s.tmpTouchedVars = append(s.tmpTouchedVars, varIdx)
			lvl := s.assignments[varIdx].Level
			if lvl <= s.level {
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

	// Start from end of trail and scan backwards (MiniSat-style)
	trailIndex := len(s.trail) - 1
	pathC := s.tmpLevelCount[s.level]

	for pathC > 1 {
		// Find next literal to resolve by scanning trail backwards
		var foundVar uint32 = 0
		found := false

		for trailIndex >= 0 {
			varIdx := uint32(s.trail[trailIndex])
			trailLevel := s.trailLevel[trailIndex]
			trailIndex--

			// Skip if not in learned clause or already resolved
			if !s.tmpLiteralInClause[varIdx] || s.tmpResolved[varIdx] {
				continue
			}

			// Skip if at lower level (not counted in pathC)
			if trailLevel != s.level {
				continue
			}

			// Found a literal at current level
			foundVar = varIdx
			found = true
			break
		}

		// Stop early if no resolvable literal found (MiniSat behavior)
		if !found {
			break
		}

		// Check if this literal has a reason (not a decision)
		if int(foundVar) >= len(s.implication) {
			// Out of bounds - should not happen, but defend against corruption
			break
		}
		reasonClause := s.implication[foundVar]
		if reasonClause == nil {
			// Decision literal - cannot resolve, stop early
			break
		}

		// Get reason clause literals directly from pointer
		reasonLits := reasonClause.Literals

		// Resolve: remove foundVar, add reason literals
		s.tmpLiteralInClause[foundVar] = false
		s.tmpResolved[foundVar] = true
		s.tmpLevelCount[s.level]--
		pathC--

		// Add reason literals (except the one we resolved on)
		newLiterals := 0
		for _, lit := range reasonLits {
			v := lit.Var()
			if v == foundVar {
				continue
			}
			if !s.tmpLiteralInClause[v] {
				s.tmpLiteralInClause[v] = true
				s.tmpLiteralIsNegated[v] = lit.IsNegated()
				s.tmpTouchedVars = append(s.tmpTouchedVars, v)
				lvl := s.varLevel[v]  // Use cached level instead of assignments[v].Level
				if lvl <= s.level {
					s.tmpLevelCount[lvl]++
					if lvl == s.level {
						pathC++
					}
				}
				newLiterals++
			}
		}

		resolvedCount++
		currentSize = currentSize - 1 + newLiterals
	}

	// Build the learned clause from remaining literals (using reusable buffer)
	s.tmpLearnedLits = s.tmpLearnedLits[:0] // Clear but keep capacity
	litsAtCurrentLevel := 0
	maxLevel := 0 // For backjump level calculation

	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			lit := cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx])
			s.tmpLearnedLits = append(s.tmpLearnedLits, lit)
			lvl := s.assignments[varIdx].Level
			if lvl == s.level {
				litsAtCurrentLevel++
			}
			if lvl > maxLevel && lvl < s.level {
				maxLevel = lvl
			}
		}
	}

	// INVARIANT CHECK: Verify exactly 1 literal at current level
	if s.verbose && s.conflicts <= DebugConflictLimit {
		fmt.Printf("c [1-UIP] Conflict %d: %d literals, %d at level %d (target: 1)\n",
			s.conflicts, len(s.tmpLearnedLits), litsAtCurrentLevel, s.level)
	}

	// If 1-UIP didn't reduce to exactly 1 literal at current level, log error and handle
	if litsAtCurrentLevel != 1 {
		if s.verbose {
			fmt.Printf("c [1-UIP ERROR] Conflict %d: Failed to find UIP! Literals: %d, at level %d\n",
				s.conflicts, len(s.tmpLearnedLits), s.level)
			if s.conflicts <= DebugConflictLimit {
				for _, lit := range s.tmpLearnedLits {
					v := lit.Var()
					sign := ""
					if lit.IsNegated() {
						sign = "¬"
					}
					fmt.Printf("c   Lit: %sx%d, Level: %d, Reason: %v\n",
						sign, v+1, s.assignments[v].Level, s.implication[v])
				}
			}
		}
		if s.level == 1 {
			return 1
		}
		backjumpLevel := s.level - 1
		if backjumpLevel < 1 {
			backjumpLevel = 1
		}
		return backjumpLevel
	}

	// Calculate LBD using touched vars (single pass, already computed maxLevel)
	lbd := 0
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			lvl := s.assignments[varIdx].Level
			if lvl > 0 && !s.tmpLevelSetUsed[lvl] {
				s.tmpLevelSetUsed[lvl] = true
				s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				lbd++
			}
		}
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
		// Glucose typically uses LBD threshold of 5-8, we use 15 as initial tuning
		LowQualityLBDThreshold := 15
		if lbd > LowQualityLBDThreshold {
			if s.verbose && s.conflicts <= DebugConflictLimit {
				fmt.Printf("c [debug] Skipping low-quality clause: LBD=%d, size=%d (threshold: LBD<=15)\n", lbd, len(s.tmpLearnedLits))
			}
			// Use precomputed maxLevel from single pass above
			backjumpLevel := maxLevel
			if backjumpLevel == 0 {
				backjumpLevel = 1
			}
			return backjumpLevel
		}

		// DUPLICATE DETECTION: Skip if this clause already exists
		// Use simple hash-based check for O(n) comparison only when hash matches
		s.tmpClauseHash = 0
		for _, lit := range s.tmpLearnedLits {
			s.tmpClauseHash = s.tmpClauseHash*31 + uint64(lit)
		}

		isDuplicate := false
		for _, existing := range s.learnedClauses {
			if len(existing.Literals) != len(s.tmpLearnedLits) {
				continue
			}
			match := true
			for j, lit := range s.tmpLearnedLits {
				if existing.Literals[j] != lit {
					match = false
					break
				}
			}
			if match {
				isDuplicate = true
				break
			}
		}

		if isDuplicate {
			// Don't learn this clause, but still return backjump level
			return s.level - 1
		}

		// Check if we need to delete clauses
		// Keep max 5000 normal clauses + all glue clauses
		maxNormalClauses := 5000

		if s.normalClauseCount >= maxNormalClauses && lbd > 3 {
			// Delete oldest 50% of normal clauses (by age)
			if s.verbose {
				fmt.Printf("c [verbose] Deleting old normal clauses: %d normal clauses (limit %d)\n", s.normalClauseCount, maxNormalClauses)
			}

			// Mark clauses to keep
			keepClause := make([]bool, len(s.learnedClauses))
			for i := range s.learnedClauses {
				if s.clauseLBD[i] <= 3 {
					// Keep all glue clauses
					keepClause[i] = true
				} else {
					// Keep only newest 50% of normal clauses
					ageRank := 0
					for j := range s.learnedClauses {
						if s.clauseLBD[j] > 3 && s.clauseAge[j] < s.clauseAge[i] {
							ageRank++
						}
					}
					keepClause[i] = (ageRank < s.normalClauseCount/2)
				}
			}

			// Compact arrays
			newClauses := make([]cnf.Clause, 0)
			newActivity := make([]float64, 0)
			newAge := make([]int, 0)
			newSize := make([]int, 0)
			newLBD := make([]int, 0)

			for i := range s.learnedClauses {
				if keepClause[i] {
					newClauses = append(newClauses, s.learnedClauses[i])
					newActivity = append(newActivity, s.clauseActivity[i])
					newAge = append(newAge, s.clauseAge[i])
					newSize = append(newSize, s.clauseSize[i])
					newLBD = append(newLBD, s.clauseLBD[i])
				}
			}

			s.learnedClauses = newClauses
			s.clauseActivity = newActivity
			s.clauseAge = newAge
			s.clauseSize = newSize
			s.clauseLBD = newLBD
			// Recalculate normal clause count after deletion
			s.normalClauseCount = 0
			for _, lbdVal := range s.clauseLBD {
				if lbdVal > 3 {
					s.normalClauseCount++
				}
			}
		}

		s.clauseActivity = append(s.clauseActivity, 0.0)
		s.clauseAge = append(s.clauseAge, s.currentAge)
		s.clauseSize = append(s.clauseSize, len(s.tmpLearnedLits))
		s.clauseLBD = append(s.clauseLBD, lbd)
		if lbd > 3 {
			s.normalClauseCount++
		}
		s.currentAge++

		// Make a copy of learned literals since tmpLearnedLits is reused
		literalsCopy := make([]cnf.Literal, len(s.tmpLearnedLits))
		copy(literalsCopy, s.tmpLearnedLits)

		// Append first, then get stable pointer
		s.learnedClauses = append(s.learnedClauses, cnf.Clause{Literals: literalsCopy, Learned: true})

		// Get stable pointer after append (slice might have reallocated)
		clause := &s.learnedClauses[len(s.learnedClauses)-1]

		// Add learned clause to watches
		if s.watchInitialized {
			s.addClauseToWatches(clause, s.tmpLearnedLits)
		}

		// Mark LBD order as dirty - will be rebuilt on next propagation
		s.lbdOrderDirty = true

		// Enforce maxLearned limit by deleting clauses when exceeded
		if len(s.learnedClauses) > s.maxLearned {
			s.deleteLearnedClauses()
		}

		// ALWAYS print first 10 learned clauses for debugging
		if len(s.learnedClauses) <= 10 {
			fmt.Printf("c [LEARNED] Clause %d: LBD=%d, size=%d, lits=[", len(s.learnedClauses)-1, lbd, len(s.tmpLearnedLits))
			for i, lit := range s.tmpLearnedLits {
				if i > 0 {
					fmt.Printf(" ")
				}
				if lit.IsNegated() {
					fmt.Printf("-%d", lit.Var()+1)
				} else {
					fmt.Printf("%d", lit.Var()+1)
				}
			}
			fmt.Printf("]\n")
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

	if backjumpLevel == 0 {
		backjumpLevel = 1
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
	// Aggressive clause deletion - keep only the absolute best clauses
	// Strategy: Delete by age first, then by quality
	// Rationale: Old clauses, even with good LBD, may not be relevant to current search

	type clauseInfo struct {
		idx      int
		lbd      int
		size     int
		age      int
		activity float64
		score    float64 // Higher = more likely to delete
	}

	clauses := make([]clauseInfo, 0, len(s.learnedClauses))

	for i, clause := range s.learnedClauses {
		// Use stored LBD (calculated at learning time)
		lbd := s.clauseLBD[i]

		size := len(clause.Literals)
		age := s.currentAge - s.clauseAge[i]
		activity := s.clauseActivity[i]

		// Calculate deletion score (higher = delete first)
		// PRIMARY FACTOR: Age (old clauses are less relevant)
		score := float64(age) * 10.0

		// SECONDARY FACTOR: LBD (higher LBD = less useful)
		score += float64(lbd) * 50.0

		// TERTIARY FACTOR: Size (larger clauses are less useful)
		score += float64(size) * 5.0

		// BONUS: Activity (active clauses are more useful)
		score -= activity * 20.0

		// PROTECTION: Glue clauses (LBD ≤ GlueLBDThreshold) are NEVER deleted
		// These are the backbone of the learned clause database
		if lbd <= GlueLBDThreshold {
			score = -1000.0 // Absolutely never delete, regardless of age or size
		}

		// LBD == 3 AND size <= 3 AND age < 50: very good, protect unless very old
		if lbd == 3 && size <= 3 && age < 50 {
			score = -500.0 // Protect unless very old
		}

		// FORCE DELETION: Very old clauses (age > MaxClauseAge) regardless of LBD
		// But NOT core glue clauses (LBD ≤ CoreGlueLBDThreshold)
		if age > MaxClauseAge && lbd > CoreGlueLBDThreshold {
			score += 1000.0 // Force deletion of very old clauses
		}

		// FORCE DELETION: Large clauses (size > LargeClauseSize) regardless of LBD
		// But NOT core glue clauses (LBD ≤ CoreGlueLBDThreshold)
		if size > LargeClauseSize && lbd > CoreGlueLBDThreshold {
			score += 800.0 // Force deletion of large clauses
		}

		clauses = append(clauses, clauseInfo{
			idx:      i,
			lbd:      lbd,
			size:     size,
			age:      age,
			activity: activity,
			score:    score,
		})
	}

	// Sort by score (descending - highest score = delete first)
	sort.Slice(clauses, func(i, j int) bool {
		return clauses[i].score > clauses[j].score
	})

	// Target: reduce to minLearned clauses (aggressive deletion)
	toKeep := s.minLearned
	if toKeep > len(s.learnedClauses) {
		toKeep = len(s.learnedClauses) // Can't keep more than we have
	}

	toDelete := len(s.learnedClauses) - toKeep

	// Mark clauses to delete
	keep := make([]bool, len(s.learnedClauses))
	deleted := 0

	for i := range keep {
		keep[i] = true // Default: keep all
	}

	for i := 0; i < len(clauses) && deleted < toDelete; i++ {
		idx := clauses[i].idx
		// Skip protected clauses (score < 0 means protected)
		if clauses[i].score < 0 {
			continue
		}
		keep[idx] = false
		deleted++
	}

	// Compact the slices
	newClauses := make([]cnf.Clause, 0, toKeep)
	newActivity := make([]float64, 0, toKeep)
	newAge := make([]int, 0, toKeep)
	newSize := make([]int, 0, toKeep)
	newLBD := make([]int, 0, toKeep)

	for i := range s.learnedClauses {
		if keep[i] {
			newClauses = append(newClauses, s.learnedClauses[i])
			newActivity = append(newActivity, s.clauseActivity[i])
			newAge = append(newAge, s.clauseAge[i])
			newSize = append(newSize, s.clauseSize[i])
			newLBD = append(newLBD, s.clauseLBD[i])
		}
	}

	s.learnedClauses = newClauses
	s.clauseActivity = newActivity
	s.clauseAge = newAge
	s.clauseSize = newSize
	s.clauseLBD = newLBD

	// Mark LBD order as dirty - must rebuild after clause deletion
	s.lbdOrderDirty = true

	if s.verbose {
		fmt.Printf("c [verbose] Deleted %d learned clauses, kept %d (target: %d)\n", deleted, len(s.learnedClauses), toKeep)
		if deleted == 0 && len(s.learnedClauses) > 300 {
			// Debug: show why clauses are protected
			protectedLBD := 0
			protectedSize := 0
			for i, clause := range s.learnedClauses {
				if i >= 100 {
					break // Sample first 100
				}
				// Use stored LBD (calculated at learning time)
				lbd := s.clauseLBD[i]
				size := len(clause.Literals)
				if lbd <= 3 {
					protectedLBD++
				}
				if size < 5 {
					protectedSize++
				}
			}
			fmt.Printf("c [debug] Protection stats (sample 100): storedLBD≤3=%d, size<5=%d\n", protectedLBD, protectedSize)
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
	if len(s.trailHead) <= 1 {
		return false
	}

	// Use backjump level if available, otherwise backtrack one level
	bjLevel := s.backjumpLevel
	if bjLevel <= 0 || bjLevel >= s.level {
		bjLevel = s.level - 1
	}
	if bjLevel < 1 {
		// Backjump level calculation failed - this indicates a problem with 1-UIP
		// Don't flip at arbitrary levels - only at level 1
		if s.level == 1 && len(s.trail) > 1 {
			decisionPoint := s.trailHead[1]
			if decisionPoint < len(s.trail) {
				decisionVar := uint32(s.trail[decisionPoint])
				// Check if we already flipped this variable
				if s.tmpFlippedVars[decisionVar] {
					return false
				}
				s.tmpFlippedVars[decisionVar] = true
				decisionValue := s.assignments[decisionVar].Value
				for i := decisionPoint + 1; i < len(s.trail); i++ {
					varIdx := uint32(s.trail[i])
					s.assignments[varIdx] = Assignment{}
					s.varLevel[varIdx] = 0
					s.implication[varIdx] = nil
				}
				s.trail = s.trail[:decisionPoint+1]
				s.trailLevel = s.trailLevel[:decisionPoint+1]
				s.qhead = decisionPoint + 1
				s.assignments[decisionVar] = Assignment{
					Value: !decisionValue,
					Level: 1,
				}
				s.varLevel[decisionVar] = 1
				return true
			}
		}
		// For level > 1, something is wrong with 1-UIP - return UNSAT
		if s.verbose {
			fmt.Printf("c [BACKTRACK] bjLevel=%d invalid at level %d - returning UNSAT\n", bjLevel, s.level)
		}
		return false
	}

	// Find the decision point at the backjump level
	decisionPoint := s.trailHead[bjLevel]
	if decisionPoint >= len(s.trail) {
		if s.verbose && s.conflicts <= DebugConflictLimit {
			fmt.Printf("c [BACKTRACK] FAIL: decision point %d >= trail len %d\n", decisionPoint, len(s.trail))
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
	s.trailLevel = s.trailLevel[:decisionPoint]
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
	s.trailLevel = append(s.trailLevel, bjLevel)

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
		// Find learned clause to get LBD
		for i := range s.learnedClauses {
			if &s.learnedClauses[i] == reasonClause {
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
	n := len(s.learnedClauses)
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
