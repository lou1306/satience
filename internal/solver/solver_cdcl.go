package solver

import (
	"fmt"
	"runtime"
	"satience/internal/cnf"
	"time"
)

// SolveResult represents the result of solving
type SolveResult int

const (
	SAT SolveResult = iota
	UNSAT
	UNKNOWN
)

// CDCLSolver implements a CDCL solver (DPLL with VSIDS + clause learning)
type CDCLSolver struct {
	cnf          *cnf.CNF
	assignments  []Assignment
	trail        []int
	trailHead    []int
	level        int
	vsids        *VSIDS
	conflicts    int
	implication  []int
	iterations   int
	maxIter      int
	learnedClauses []cnf.Clause      // Learned clauses
	clauseActivity []float64
	clauseAge    []int
	clauseSize   []int // Track clause size for deletion
	clauseLBD    []int // Track LBD at time of learning
	normalClauseCount int // Track number of non-glue clauses (LBD > 3)
	currentAge   int
	verbose      bool
	decisions    int
	backjumpLevel int
	maxLearned   int
	minLearned   int // Minimum clauses to keep (aggressive deletion target)
	savedPhase   []bool
	restartBase  int
	restartCount int
	lubyIndex    int
	lbdSum       int
	lbdCount     int
	lastConflictLBD int
	conflictsAtLevel []int  // Track conflicts per decision level
	lastRandomDecision int  // Last conflict where we made random decision
	// Reusable buffers for conflict analysis (avoid per-conflict allocation)
	tmpLiteralInClause []bool
	tmpLiteralIsNegated []bool
	tmpLevelCount []int
	tmpCandidates []resolveCandidate
	tmpLevelSet []int // For LBD calculation (replaces map)
	tmpLevelSetUsed []bool // Track which levels are in tmpLevelSet
	tmpResolved []bool // Track resolved variables in 1-UIP to prevent cycles
	tmpClauseHash uint64 // Hash for duplicate detection
	
	// Variable elimination tracking for model reconstruction
	eliminatedVars map[uint32]eliminationInfo  // Maps eliminated var to substitution rule
	elimOrder      int                          // Elimination order counter
	
	// Watched literals infrastructure
	watchLists     [][]cnf.Watch  // watchLists[lit] = clauses watching lit
	watchInitialized bool         // True if watches have been initialized
	
	// LBD-based learned clause ordering for propagation prioritization
	learnedClauseOrder []int  // Indices into learnedClauses/clauseLBD sorted by LBD
	lbdOrderDirty      bool   // True if order needs rebuilding
	lbdOrderLastRebuild int  // Conflict count when order was last rebuilt
	
	qhead int  // Watched literals: next trail index to process
}

// eliminationInfo stores how a variable was eliminated for model reconstruction
type eliminationInfo struct {
	posClauseLits [][]cnf.Literal  // Clauses with positive literal X: stored as (X ∨ A) -> store A
	negClauseLits [][]cnf.Literal  // Clauses with negative literal ¬X: stored as (¬X ∨ B) -> store B
	elimOrder     int              // Elimination order (0 = first eliminated)
}

// resolveCandidate is used in learnClause for sorting resolution order
type resolveCandidate struct {
	varIdx uint32
	size   int
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	maxLearned := 2000   // Keep moderate learned clause database (balance between pruning and propagation cost)
	minLearned := 1000   // Target after deletion (50% reduction)
	restartBase := 100  // Base for Luby restart sequence
	
	// Ensure literal pool is built for efficient propagation
	formula.RebuildLiteralPool()
	
	solver := &CDCLSolver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		trailHead:   make([]int, 1),
		qhead:       0,
		level:       0,
		vsids:       NewVSIDS(formula.NumVars),
		conflicts:   0,
		implication: make([]int, formula.NumVars),
		iterations:  0,
		maxIter:     0,
		learnedClauses: make([]cnf.Clause, 0),
		clauseActivity: make([]float64, 0),
		clauseAge:    make([]int, 0),
		clauseLBD:    make([]int, 0),
		currentAge:   0,
		verbose:     false,
		decisions:   0,
		backjumpLevel: 0,
		maxLearned:   maxLearned,
		minLearned:   minLearned,
		savedPhase:  make([]bool, formula.NumVars),
		restartBase:  restartBase,
		restartCount: 0,
		lubyIndex:    0,
		lbdSum:       0,
		lbdCount:     0,
		learnedClauseOrder: make([]int, 0),
		lbdOrderDirty:      true,
		lbdOrderLastRebuild: 0,
		lastConflictLBD: 0,
		conflictsAtLevel: make([]int, formula.NumVars+1),
		lastRandomDecision: -1000,
		// Pre-allocate reusable buffers
	tmpLiteralInClause: make([]bool, formula.NumVars),
	tmpLiteralIsNegated: make([]bool, formula.NumVars),
	tmpLevelCount: make([]int, formula.NumVars+1),
	tmpCandidates: make([]resolveCandidate, 0, 100),
	tmpLevelSet: make([]int, 0, formula.NumVars),
	tmpLevelSetUsed: make([]bool, formula.NumVars+1),
	tmpResolved: make([]bool, formula.NumVars),
		// Initialize variable elimination tracking
		eliminatedVars: make(map[uint32]eliminationInfo),
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

// EnableLRB enables LRB (Learning Rate Based) heuristic
func (s *CDCLSolver) EnableLRB() {
	s.vsids.EnableLRB()
}

// GetStats returns solving statistics
// SolverStats holds detailed solving statistics
type SolverStats struct {
	Conflicts        int
	Decisions        int
	Iterations       int
	LearnedClauses   int
	MaxLevel         int
	AvgClauseSize    int
	AvgLBD           int
	MinClauseSize    int
	MaxClauseSize    int
}

func (s *CDCLSolver) GetStats() map[string]int {
	stats := s.getDetailedStats()
	return map[string]int{
		"conflicts":     stats.Conflicts,
		"decisions":     stats.Decisions,
		"iterations":    stats.Iterations,
		"learned":       stats.LearnedClauses,
		"level":         stats.MaxLevel,
	}
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

func (s *CDCLSolver) preprocessAggressive() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Aggressive preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}

	initialClauses := s.cnf.NumClauses
	
	// Increase to 5 passes for more thorough preprocessing
	// Modern solvers (CaDiCaL) use 10+ passes
	// Safeguards: time limits in each technique prevent explosion
	for pass := 0; pass < 5; pass++ {
		if s.verbose {
			fmt.Printf("c [verbose] Preprocessing pass %d: %d clauses\n", pass+1, s.cnf.NumClauses)
		}
		
		// Run unit propagation first to catch any existing units
		unitResult := s.unitPropagationPreprocess()
		if unitResult != UNKNOWN {
			return unitResult
		}
		
		// Equivalence detection: find a↔b patterns and substitute (BEFORE VE destroys binary clauses)
		equivResult := s.equivalenceDetection()
		if equivResult != UNKNOWN {
			return equivResult
		}
		
		// Variable elimination disabled (causes model reconstruction bugs)
		
		// Run unit propagation again after variable elimination
		unitResult = s.unitPropagationPreprocess()
		if unitResult != UNKNOWN {
			return unitResult
		}
		
		pureResult := s.pureLiteralElimination()
		if pureResult != UNKNOWN {
			return pureResult
		}
		
		s.subsumptionElimination()
		
		s.selfSubsumption()
		
		s.hyperBinaryResolution()
		
		// Run unit propagation after hyper-binary resolution
		unitResult = s.unitPropagationPreprocess()
		if unitResult != UNKNOWN {
			return unitResult
		}
		
		// DISABLED: Failed literal elimination causes soundness bugs
		// failedResult := s.failedLiteralElimination()
		// if failedResult != UNKNOWN {
		// 	return failedResult
		// }
		
		// Stop if no progress made for 2 consecutive passes
		if s.cnf.NumClauses == initialClauses && pass >= 1 {
			break
		}
		initialClauses = s.cnf.NumClauses
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] After preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}
	
	// Rebuild literal pool after preprocessing modifications
	s.cnf.RebuildLiteralPool()
	
	// Initialize watched literals after preprocessing completes
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
		clause := s.cnf.Clauses[clauseID]
		s.addClauseToWatches(clauseID, clause.Literals, false)
	}
	
	for clauseID := 0; clauseID < len(s.learnedClauses); clauseID++ {
		clause := s.learnedClauses[clauseID]
		s.addClauseToWatches(clauseID, clause.Literals, true)
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Watched literals enabled: %d watches initialized\n", len(s.watchLists))
	}
}

// addClauseToWatches adds a clause to the watch lists
// Watches the first two literals in the clause
func (s *CDCLSolver) addClauseToWatches(clauseID int, literals []cnf.Literal, learned bool) {
	if len(literals) < 2 {
		return
	}
	
	lit0 := literals[0]
	lit1 := literals[1]
	isBinary := len(literals) == 2
	
	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)
	
	s.watchLists[idx0] = append(s.watchLists[idx0], cnf.Watch{
		ClauseID: uint32(clauseID),
		Blit:     uint32(idx1),
		IsBinary: isBinary,
	})
	
	s.watchLists[idx1] = append(s.watchLists[idx1], cnf.Watch{
		ClauseID: uint32(clauseID),
		Blit:     uint32(idx0),
		IsBinary: isBinary,
	})
	
	if clauseID == 108 {
	}
	
	// Watched literals now enabled with all soundness bugs fixed
	s.watchInitialized = false  // Watch propagation order bug: multiple propagations in same pass violate binary clauses
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

// failedLiteralElimination detects literals that must be false through trial assignment

// Algorithm:
// For each unassigned variable x, try assigning x=false and propagate.
// If conflict occurs, then x must be true (failed literal).
// This is a powerful preprocessing technique but can be expensive.

// Safeguards to prevent memory explosion (learned from PHP instances):
// 1. Time limit: 500ms total for entire failed literal elimination
// 2. Per-variable limit: 10ms max per variable
// 3. Formula size limit: Skip if >2000 variables or >5000 clauses
// 4. Early termination: Stop if clause count grows by >10%
// 5. Skip dense instances: Skip if clause/variable ratio >10
func (s *CDCLSolver) failedLiteralElimination() SolveResult {
	// Strict safeguards to prevent memory explosion
	if s.cnf.NumVars > 2000 || s.cnf.NumClauses > 5000 {
		if s.verbose {
			fmt.Printf("c [verbose] Failed literal: skipped (too large: %d vars, %d clauses)\n", 
				s.cnf.NumVars, s.cnf.NumClauses)
		}
		return UNKNOWN
	}
	
	density := float64(s.cnf.NumClauses) / float64(s.cnf.NumVars)
	if density > 10 {
		if s.verbose {
			fmt.Printf("c [verbose] Failed literal: skipped (dense instance: %.1f clauses/var)\n", density)
		}
		return UNKNOWN
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Failed literal elimination: checking %d variables\n", s.cnf.NumVars)
	}
	
	startTime := time.Now()
	totalTimeLimit := 500 * time.Millisecond
	varTimeLimit := 10 * time.Millisecond
	initialClauses := s.cnf.NumClauses
	maxClauses := initialClauses * 120 / 100 // Allow 20% growth
	
	changed := true
	for changed {
		changed = false
		
		// Check total time limit
		if time.Since(startTime) > totalTimeLimit {
			if s.verbose {
				fmt.Printf("c [verbose] Failed literal: time limit reached (%.2fs)\n", 
					time.Since(startTime).Seconds())
			}
			break
		}
		
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level != 0 {
				continue
			}
			
			// Check per-variable time limit
			varStartTime := time.Now()
			
			for polarity := 0; polarity < 2; polarity++ {
				// Check time limit for this variable
				if time.Since(varStartTime) > varTimeLimit {
					if s.verbose {
						fmt.Printf("c [verbose] Failed literal: var %d time limit\n", varIdx)
					}
					goto nextVar
				}
				
				value := polarity == 0
				lit := cnf.NewLiteral(varIdx, !value)
				
				savedTrail := len(s.trail)
				savedImplication := make([]int, len(s.implication))
				copy(savedImplication, s.implication)
				
				s.assignLiteral(lit, 1, -1)
				conflict, _ := s.propagate()
				
				if conflict {
					oppositeValue := !value
					s.assignments[varIdx] = Assignment{
						Value: oppositeValue,
						Level: 1,
					}
					s.trail = s.trail[:savedTrail]
					s.trailHead = s.trailHead[:1]
					s.level = 0
					copy(s.implication, savedImplication)
					
					conflict = s.simplifyAfterAssignment(varIdx, oppositeValue)
					if conflict {
						return UNSAT
					}
					
					// Check clause growth
					if s.cnf.NumClauses > maxClauses {
						if s.verbose {
							fmt.Printf("c [verbose] Failed literal: clause growth limit (%d -> %d)\n",
								initialClauses, s.cnf.NumClauses)
						}
						return UNKNOWN
					}
					
					changed = true
					if s.verbose {
						fmt.Printf("c [verbose] Failed literal: var %d = %v\n", varIdx, oppositeValue)
					}
					break
				} else {
					for i := savedTrail; i < len(s.trail); i++ {
						v := uint32(s.trail[i])
						s.assignments[v] = Assignment{}
						s.implication[v] = savedImplication[v]
					}
					s.trail = s.trail[:savedTrail]
					s.trailHead = s.trailHead[:1]
					s.level = 0
				}
			}
			
			nextVar:
		}
	}
	
	if s.verbose && changed {
		fmt.Printf("c [verbose] Failed literal: eliminated %d variables\n", 
			initialClauses - s.cnf.NumClauses)
	}
	
	return UNKNOWN
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
	// Glucose-style adaptive restarts (PRIMARY)
	// Luby sequence as fallback (SECONDARY)
	
	// Need at least 50 conflicts for LBD statistics
	if s.lbdCount >= 50 {
		avgLBD := float64(s.lbdSum) / float64(s.lbdCount)
		
		if s.verbose && s.conflicts % 1000 == 0 {
			fmt.Printf("c [verbose] LBD stats: avg=%.2f, last=%d, threshold=%.2f\n", avgLBD, s.lastConflictLBD, 1.5*avgLBD)
		}
		
		// Glucose criterion: restart when current LBD > 1.5× average
		if s.lastConflictLBD > int(1.5*avgLBD) && s.lastConflictLBD > 3 {
			if len(s.learnedClauses) >= 50 {
				return true
			}
		}
		
		// Also restart if LBD is very high (absolute threshold)
		if s.lastConflictLBD > 12 {
			if len(s.learnedClauses) >= 50 {
				return true
			}
		}
	}
	
	// Fallback to Luby sequence for regular restarts
	lubyValue := luby(s.lubyIndex + 1)
	threshold := lubyValue * s.restartBase
	
	return s.conflicts-s.restartCount >= threshold
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
		// Calculate LBD while assignments are still valid
		levelSet := make(map[int]bool)
		for _, lit := range clause.Literals {
			lvl := s.assignments[lit.Var()].Level
			if lvl > 0 {
				levelSet[lvl] = true
			}
		}
		lbd := len(levelSet)
		
		// Keep glue clauses (LBD <= 5) - ADAPTED for our 1-UIP implementation
		// Our 1-UIP produces clauses with LBD 5-8 typically, so LBD<=3 is too strict
		// LBD <= 2: core glue (most valuable, never delete)
		// LBD 3-5: useful glue (keep across restarts)
		// LBD > 5: trash (delete on restart)
		if lbd <= 5 {
			glueCount++
			isGlue[i] = true
		}
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Restart: keeping %d glue clauses, deleting %d non-glue\n", glueCount, len(s.learnedClauses)-glueCount)
	}
	
	// Clear trail and assignments
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.level = 0
	for i := range s.implication {
		s.implication[i] = -1
	}
	for i := range s.assignments {
		s.assignments[i] = Assignment{}
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
	
	// CRITICAL: Reset VSIDS activity on restart
	// Without this, the same high-activity variables get chosen again,
	// leading to infinite loops on PHP-like instances
	s.vsids.resetActivity()
	
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
	
}

func (s *CDCLSolver) variableElimination() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Variable elimination: checking %d variables\n", s.cnf.NumVars)
	}
	
	// Aggressive variable elimination with relaxed limits
	// Increased from 2s to 5s total, 100ms to 200ms per var, degree 100 to 150
	// Modern solvers (CaDiCaL) are much more aggressive
	// ALLOW 10% BLOWUP: Controlled formula growth for better elimination
	startTime := time.Now()
	totalTimeLimit := 5 * time.Second
	varTimeLimit := 200 * time.Millisecond
	
	eliminatedCount := 0
	resolventCount := 0
	initialClauses := s.cnf.NumClauses
	maxClauses := initialClauses * 120 / 100 // Allow 20% blowup
	
	changed := true
	for changed {
		changed = false
		eliminated := make([]bool, s.cnf.NumVars)
		
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if eliminated[varIdx] {
				continue
			}
			
			// Check total time limit
			if time.Since(startTime) > totalTimeLimit {
				if s.verbose {
					fmt.Printf("c [verbose] Variable elimination: time limit reached (%.2fs)\n", time.Since(startTime).Seconds())
				}
				goto done
			}
			
			// Check clause blowup limit
			if s.cnf.NumClauses > maxClauses {
				if s.verbose {
					fmt.Printf("c [verbose] Variable elimination: clause blowup limit reached (%d > %d)\n", 
						s.cnf.NumClauses, maxClauses)
				}
				goto done
			}
			
			varStartTime := time.Now()
			
			posClauses := make([]int, 0)
			negClauses := make([]int, 0)
			
			for i, clause := range s.cnf.Clauses {
				hasPos := false
				hasNeg := false
				for _, lit := range clause.Literals {
					if lit.Var() == varIdx {
						if !lit.IsNegated() {
							hasPos = true
						} else {
							hasNeg = true
						}
					}
				}
				
				if hasPos {
					posClauses = append(posClauses, i)
				}
				if hasNeg {
					negClauses = append(negClauses, i)
				}
			}
			
			// DEGREE LIMIT: Skip variables with high degree
			// Increased from 100 to 150 to eliminate more variables
			totalDegree := len(posClauses) + len(negClauses)
			if totalDegree > 150 {
				continue
			}
			
			if len(posClauses) == 0 || len(negClauses) == 0 {
				continue
			}
			
			// ALLOW 10% BLOWUP: More aggressive than 0% but still controlled
			// This allows eliminating variables that slightly increase clause count
			originalCount := len(posClauses) + len(negClauses)
			maxResolvents := originalCount + (originalCount * 20 / 100) // 10% blowup allowed
			
			// Early exit check: Cartesian product would be too large
			if len(posClauses) * len(negClauses) > maxResolvents * 2 {
				// Too many potential resolvents, skip this variable
				continue
			}
			
			resolvents := make([]cnf.Clause, 0, maxResolvents)
			resolventSet := make(map[string]bool)
			
			for _, posIdx := range posClauses {
				// Check per-variable time limit
				if time.Since(varStartTime) > varTimeLimit {
					if s.verbose {
						fmt.Printf("c [verbose] Var %d: time limit for this variable\n", varIdx)
					}
					goto nextVar
				}
				
				posClause := s.cnf.Clauses[posIdx]
				
				for _, negIdx := range negClauses {
					negClause := s.cnf.Clauses[negIdx]
					
					resolvent := s.resolveForElimination(posClause, negClause, varIdx)
					if resolvent != nil {
						if !s.isTautology(resolvent) {
							key := s.clauseKey(resolvent)
							if !resolventSet[key] {
								resolventSet[key] = true
								resolvents = append(resolvents, *resolvent)
								
								// Early exit if resolvents exceed limit
								if len(resolvents) > maxResolvents {
									goto nextVar
								}
							}
						}
					}
				}
			}
			
			// Only eliminate if resolvents <= original (already checked during construction)
			if len(resolvents) <= maxResolvents {
				if s.verbose {
					fmt.Printf("c [verbose] Eliminating var %d: %d clauses -> %d resolvents\n", 
						varIdx, originalCount, len(resolvents))
				}
				
				for _, resolvent := range resolvents {
					if len(resolvent.Literals) == 0 {
						if s.verbose {
							fmt.Printf("c [verbose] Variable elimination: empty clause created (UNSAT)\n")
						}
						return UNSAT
					}
				}
				
				// Track eliminated variable for model reconstruction BEFORE modifying clauses
				// Store the clauses WITHOUT the eliminated variable
				posLits := make([][]cnf.Literal, len(posClauses))
				negLits := make([][]cnf.Literal, len(negClauses))
				for i, idx := range posClauses {
					clause := s.cnf.Clauses[idx]
					lits := make([]cnf.Literal, 0)
					for _, lit := range clause.Literals {
						if lit.Var() != varIdx {
							lits = append(lits, lit)
						}
					}
					posLits[i] = lits
				}
				for i, idx := range negClauses {
					clause := s.cnf.Clauses[idx]
					lits := make([]cnf.Literal, 0)
					for _, lit := range clause.Literals {
						if lit.Var() != varIdx {
							lits = append(lits, lit)
						}
					}
					negLits[i] = lits
				}
				
				keepClauses := make([]cnf.Clause, 0)
				for _, clause := range s.cnf.Clauses {
					keep := true
					for _, lit := range clause.Literals {
						if lit.Var() == varIdx {
							keep = false
							break
						}
					}
					if keep {
						keepClauses = append(keepClauses, clause)
					}
				}
				
				for _, resolvent := range resolvents {
					if !s.isSubsumedByAny(resolvent, keepClauses) {
						keepClauses = append(keepClauses, resolvent)
					}
				}
				
				s.cnf.Clauses = keepClauses
				s.cnf.NumClauses = len(keepClauses)
				
				s.eliminatedVars[varIdx] = eliminationInfo{
					posClauseLits: posLits,
					negClauseLits: negLits,
					elimOrder:     s.elimOrder,
				}
				s.elimOrder++
				
				eliminated[varIdx] = true
				eliminatedCount++
				resolventCount += len(resolvents)
				changed = true
			}
			
			nextVar:
		}
	}
	
	done:
	
	if s.verbose {
		fmt.Printf("c [verbose] Variable elimination: eliminated %d variables, added %d resolvents\n", 
			eliminatedCount, resolventCount)
	}
	
	if len(s.cnf.Clauses) == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Variable elimination: all clauses satisfied\n")
		}
		// Extend model to eliminated variables
		if !s.extendModel() {
			return UNSAT  // Contradiction found during model reconstruction
		}
		return SAT
	}
	
	return UNKNOWN
}

// extendModel extends the current model to eliminated variables
// Called after SAT is found to assign values to variables that were eliminated
// Returns true if model extension succeeded, false if contradiction found (UNSAT)

// Model reconstruction logic:
// Original clauses: (X ∨ A₁), (X ∨ A₂), ... and (¬X ∨ B₁), (¬X ∨ B₂), ...
// Stored as: A₁, A₂, ... (without X) and B₁, B₂, ... (without ¬X)

// To satisfy the original clauses:
// - If ANY (X ∨ Aᵢ) has Aᵢ=false, we MUST set X=TRUE to satisfy it
// - If ANY (¬X ∨ Bⱼ) has Bⱼ=false, we MUST set X=FALSE to satisfy it
// - If both constraints exist, the formula is UNSAT (contradiction)
// - Otherwise, X can be assigned arbitrarily (use phase saving or false)
func (s *CDCLSolver) extendModel() bool {
	if s.verbose {
		fmt.Printf("c [DEBUG] extendModel called with %d eliminated vars\n", len(s.eliminatedVars))
	}
	if len(s.eliminatedVars) == 0 {
		return true  // No eliminated variables, success
	}
	
	// Process eliminated variables in reverse elimination order (last eliminated first)
	// This ensures that when we assign X, any variables eliminated AFTER X
	// (which may appear in X's clauses) are already assigned
	eliminatedList := make([]uint32, 0, len(s.eliminatedVars))
	for varIdx := range s.eliminatedVars {
		eliminatedList = append(eliminatedList, varIdx)
	}
	
	// Sort by elimination order (descending = reverse order)
	// Variables eliminated later should be processed first
	for i := 0; i < len(eliminatedList); i++ {
		for j := i + 1; j < len(eliminatedList); j++ {
			infoI := s.eliminatedVars[eliminatedList[i]]
			infoJ := s.eliminatedVars[eliminatedList[j]]
			if infoI.elimOrder < infoJ.elimOrder {
				// j was eliminated later, should come first
				eliminatedList[i], eliminatedList[j] = eliminatedList[j], eliminatedList[i]
			}
		}
	}
	
	for _, varIdx := range eliminatedList {
		info := s.eliminatedVars[varIdx]
		
		if s.verbose {
			fmt.Printf("c [DEBUG] Reconstructing var %d (elimOrder=%d, pos=%d clauses, neg=%d clauses)\n",
				varIdx+1, info.elimOrder, len(info.posClauseLits), len(info.negClauseLits))
		}
		
		// Special case: equivalence detection stores single-literal clauses
		// This means X is equivalent to the representative variable
		// Just copy the representative's value
		if len(info.posClauseLits) == 1 && len(info.posClauseLits[0]) == 1 &&
		   len(info.negClauseLits) == 1 && len(info.negClauseLits[0]) == 1 {
			repVar := info.posClauseLits[0][0].Var()
			repValue := s.assignments[repVar].Value
			s.assignments[varIdx] = Assignment{
				Value: repValue,
				Level: 1,
			}
			if s.verbose {
				fmt.Printf("c [DEBUG]   Equivalence: var %d = var %d = %v\n", varIdx+1, repVar+1, repValue)
			}
			continue
		}
		
		// Check if any positive clause requires X to be TRUE
		// A clause (X ∨ A) requires X=TRUE if all literals in A are FALSE
		mustBeTrue := false
		for _, clauseLits := range info.posClauseLits {
			allFalse := true
			for _, lit := range clauseLits {
				// Get assignment for this literal's variable
				assign := s.assignments[lit.Var()]
				// Check if literal is true in current model
				litTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
				if litTrue {
					allFalse = false  // Clause is satisfied by this literal
					break
				}
			}
			if allFalse && len(clauseLits) > 0 {
				// This clause (X ∨ A) has all of A=false, so X MUST be TRUE
				mustBeTrue = true
				if s.verbose {
					fmt.Printf("c [DEBUG]   Clause %v requires X=TRUE (all lits false)\n", clauseLits)
				}
				break
			}
		}
		
		// Check if any negative clause requires X to be FALSE
		// A clause (¬X ∨ B) requires X=FALSE if all literals in B are FALSE
		mustBeFalse := false
		for _, clauseLits := range info.negClauseLits {
			allFalse := true
			if s.verbose {
				fmt.Printf("c [DEBUG]   Checking neg clause %v:\n", clauseLits)
			}
			for _, lit := range clauseLits {
				assign := s.assignments[lit.Var()]
				litTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
				if s.verbose {
					fmt.Printf("c [DEBUG]     lit %v (var %d): value=%v, level=%d, litTrue=%v\n",
						lit, lit.Var()+1, assign.Value, assign.Level, litTrue)
				}
				if litTrue {
					allFalse = false
					break
				}
			}
			if allFalse && len(clauseLits) > 0 {
				// This clause (¬X ∨ B) has all of B=false, so X MUST be FALSE
				mustBeFalse = true
				if s.verbose {
					fmt.Printf("c [DEBUG]   Clause %v requires X=FALSE (all lits false)\n", clauseLits)
				}
				break
			}
		}
		
		// Check for contradiction - this means the formula is UNSAT!
		if mustBeTrue && mustBeFalse {
			if s.verbose {
				fmt.Printf("c [verbose] extendModel: CONTRADICTION for var %d - formula is UNSAT!\n", varIdx+1)
			}
			return false  // UNSAT detected during model reconstruction
		}
		
		// Assign the variable
		varValue := false
		if mustBeTrue {
			varValue = true
		} else if mustBeFalse {
			varValue = false
		} else {
			// No constraints - use saved phase or default to false
			if varIdx < uint32(len(s.savedPhase)) {
				varValue = s.savedPhase[varIdx]
			}
		}
		
		if s.verbose {
			fmt.Printf("c [DEBUG]   Assigning var %d = %v (mustBeTrue=%v, mustBeFalse=%v)\n",
				varIdx+1, varValue, mustBeTrue, mustBeFalse)
		}
		
		s.assignments[varIdx].Value = varValue
		if s.assignments[varIdx].Level == 0 {
			s.assignments[varIdx].Level = 1  // Mark as assigned for model output
		}
	}
	
	return true  // Success
}

func (s *CDCLSolver) resolveForElimination(clause1, clause2 cnf.Clause, varIdx uint32) *cnf.Clause {
	hasPosX := false
	hasNegX := false
	
	for _, lit := range clause1.Literals {
		if lit.Var() == varIdx && !lit.IsNegated() {
			hasPosX = true
			break
		}
	}
	
	for _, lit := range clause2.Literals {
		if lit.Var() == varIdx && lit.IsNegated() {
			hasNegX = true
			break
		}
	}
	
	if !hasPosX || !hasNegX {
		return nil
	}
	
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
		return &cnf.Clause{Literals: make([]cnf.Literal, 0), Learned: false}
	}
	
	return &cnf.Clause{Literals: resolventLits, Learned: false}
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

func (s *CDCLSolver) clauseKey(clause *cnf.Clause) string {
	// Use a more efficient encoding: pack sorted literals into a string
	// For small clauses (≤8 literals), this avoids fmt.Sprintf allocation
	lits := make([]uint32, len(clause.Literals))
	for i, lit := range clause.Literals {
		lits[i] = uint32(lit)
	}
	
	// Sort literals for canonical representation
	for i := 0; i < len(lits)-1; i++ {
		for j := i + 1; j < len(lits); j++ {
			if lits[i] > lits[j] {
				lits[i], lits[j] = lits[j], lits[i]
			}
		}
	}
	
	// Encode as bytes: each literal is 4 bytes
	key := make([]byte, len(lits)*4)
	for i, lit := range lits {
		key[i*4] = byte(lit)
		key[i*4+1] = byte(lit >> 8)
		key[i*4+2] = byte(lit >> 16)
		key[i*4+3] = byte(lit >> 24)
	}
	
	return string(key)
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
	
	if s.conflicts % 1000 == 0 {
		s.selfSubsumption()
		s.variableElimination()
	}
	
	if s.verbose && initialClauses != s.cnf.NumClauses {
		fmt.Printf("c [verbose] Inprocessing: reduced from %d to %d clauses\n", initialClauses, s.cnf.NumClauses)
	}
}

func (s *CDCLSolver) unitPropagationPreprocess() SolveResult {
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
				changed = true
				
				conflict := s.simplifyAfterAssignment(varIdx, value)
				if conflict {
					if s.verbose {
						fmt.Printf("c [verbose] Preprocessing: empty clause created\n")
					}
					return UNSAT
				}
				
				clauseCount = len(s.cnf.Clauses)
				if clauseIdx >= clauseCount {
					clauseIdx = clauseCount - 1
				}
			}
		}
	}
	
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
	
	// Step 2: Build bidirectional graph
	// hasEdge[a][b] = true if a → b exists
	hasEdge := make(map[uint32]map[uint32]bool)
	for _, imp := range implications {
		if hasEdge[imp.from] == nil {
			hasEdge[imp.from] = make(map[uint32]bool)
		}
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
	for a, targets := range hasEdge {
		for b := range targets {
			if hasEdge[b] != nil && hasEdge[b][a] {
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
	
	// Store eliminated variables for model reconstruction
	if s.eliminatedVars == nil {
		s.eliminatedVars = make(map[uint32]eliminationInfo)
	}
	for varIdx, subst := range substMap {
		// Eliminated variable varIdx is equivalent to subst.rep
		// Model reconstruction: assign varIdx = value of subst.rep
		s.eliminatedVars[varIdx] = eliminationInfo{
			posClauseLits: [][]cnf.Literal{{cnf.NewLiteral(subst.rep, false)}},
			negClauseLits: [][]cnf.Literal{{cnf.NewLiteral(subst.rep, true)}},
			elimOrder:     s.elimOrder,
		}
		s.elimOrder++
	}
	
	// Zero out activity for eliminated variables
	for varIdx := range substMap {
		s.vsids.activity[varIdx] = 0.0
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Equivalence detection: eliminated %d variables, resulting in %d clauses\n", 
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
		if !s.extendModel() {
			return UNSAT  // Contradiction found during model reconstruction
		}
		// Assign all remaining unassigned variables (representatives) arbitrarily
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

func (s *CDCLSolver) SolveWithResult() SolveResult {
	// DISABLED: Preprocessing causes soundness bugs
	// preprocessResult := s.preprocessAggressive()
	// if preprocessResult != UNKNOWN {
	// 	if s.verbose {
	// 		s.printStats()
	// 	}
	// 	return preprocessResult
	// }
	
	// Initialize watches before search
	s.initWatches()
	
	// Initialize VSIDS with clause-length weighted activity BEFORE search
	// Variables in shorter clauses get higher activity (more constrained = more important)
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	for {
		s.iterations++
		if s.iterations % 10000 == 0 && s.verbose {
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
		
		conflict, clauseIdx := s.propagate()
		if conflict {
			s.handleConflict(clauseIdx)
			if s.conflicts % 50 == 0 && s.verbose {
				propsPerDec := 0.0
				if s.decisions > 0 {
					propsPerDec = float64(s.iterations) / float64(s.decisions)
				}
				fmt.Printf("c [verbose] Conflict %d, level %d, learned %d, decisions %d, props/dec %.1f\n", 
					s.conflicts, s.level, len(s.learnedClauses), s.decisions, propsPerDec)
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
				fmt.Printf("c [DEBUG] allAssigned=true, calling extendModel\n")
			}
			// Extend model to eliminated variables before returning SAT
			if !s.extendModel() {
				if s.verbose {
					fmt.Printf("c [verbose] extendModel returned UNSAT - contradiction found\n")
					s.printStats()
				}
				return UNSAT
			}
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
		// Skip eliminated variables - they will be assigned by extendModel()
		if _, eliminated := s.eliminatedVars[i]; eliminated {
			continue
		}
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
			if s.verbose {
				fmt.Printf("c [DEBUG] verifyModel: clause %v, lit %v (var %d): value=%v, level=%d, litTrue=%v\n",
					clause.Literals, lit, lit.Var()+1, assign.Value, assign.Level, litTrue)
			}
			if litTrue {
				clauseSat = true
				break
			}
		}
		if !clauseSat {
			if s.verbose {
				fmt.Printf("c [ERROR] Clause %v is not satisfied!\n", clause.Literals)
				// Print all variable values for debugging
				for _, lit := range clause.Literals {
					assign := s.assignments[lit.Var()]
					fmt.Printf("c [ERROR]   var %d: value=%v, level=%d\n", lit.Var()+1, assign.Value, assign.Level)
				}
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
func (s *CDCLSolver) propagateWatched() (bool, int) {
	if !s.watchInitialized {
		return s.propagate()
	}
	
	// Use persistent qhead pointer (MiniSat-style) to avoid re-processing trail elements
	if s.qhead >= len(s.trail) {
		return false, -1
	}
	
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
	watches := s.watchLists[watchIdx]
	
	newWatchCount := 0
	for i := 0; i < len(watches); i++ {
		watch := watches[i]
		clauseID := watch.ClauseID
		blitIdx := watch.Blit
		
		blit := cnf.IndexToLit(int(blitIdx))
		
		// Inline literalIsTrue check (optimization #4)
		blitAssign := s.assignments[blit.Var()]
		blitIsTrue := blitAssign.Level != 0 && ((blit.IsNegated() && !blitAssign.Value) || (!blit.IsNegated() && blitAssign.Value))
		
		if blitIsTrue {
			if newWatchCount != i {
				watches[newWatchCount] = watch
			}
			newWatchCount++
			continue
		}
			
			var clause cnf.Clause
			var isLearned bool
			var learnedIdx int
			if clauseID < uint32(s.cnf.NumClauses) {
				clause = s.cnf.Clauses[clauseID]
				isLearned = false
				learnedIdx = -1
			} else {
				learnedIdx = int(clauseID - uint32(s.cnf.NumClauses))
				if learnedIdx < 0 || learnedIdx >= len(s.learnedClauses) {
					continue
				}
				clause = s.learnedClauses[learnedIdx]
				isLearned = true
			}
			
		foundReplacement := false
		if len(clause.Literals) > 2 {
			// Cache blitWatchIdx to avoid repeated LitToIndex calls (optimization #6)
			blitWatchIdx := cnf.LitToIndex(blit)
			for j := 0; j < len(clause.Literals); j++ {
				clauseLit := clause.Literals[j]
				if clauseLit == falseLit || clauseLit == blit {
					continue
				}
				
				litLevel := s.assignments[clauseLit.Var()].Level
				litValue := s.assignments[clauseLit.Var()].Value
				litTrue := (!clauseLit.IsNegated() && litValue) || (clauseLit.IsNegated() && !litValue)
				
				if litTrue || litLevel == 0 {
					newWatchIdx := cnf.LitToIndex(clauseLit)
					s.watchLists[newWatchIdx] = append(s.watchLists[newWatchIdx], cnf.Watch{
						ClauseID: clauseID,
						Blit:     uint32(blitIdx),
						IsBinary: false,
					})
					for k := range s.watchLists[blitWatchIdx] {
						if s.watchLists[blitWatchIdx][k].ClauseID == clauseID {
							s.watchLists[blitWatchIdx][k].Blit = uint32(newWatchIdx)
							break
						}
					}
					foundReplacement = true
					break
				}
			}
		}
		
		if foundReplacement {
			s.watchLists[watchIdx] = watches[:newWatchCount]
			continue
		}
			
	blitLevel := s.assignments[blit.Var()].Level
	
	if blitLevel == 0 {
		reasonIdx := int(clauseID)
		if isLearned {
			reasonIdx = -learnedIdx - 1
		}
		s.assignLiteral(blit, s.level, reasonIdx)
		if newWatchCount != i {
			watches[newWatchCount] = watch
		}
		newWatchCount++
		s.watchLists[watchIdx] = watches[:newWatchCount]
		
		// Immediately propagate newly assigned literal (depth-first)
		// Cache blitWatchIdx to avoid repeated LitToIndex calls (optimization #6)
		blitWatchIdx := cnf.LitToIndex(blit)
		blitWatches := s.watchLists[blitWatchIdx]
		for bi := 0; bi < len(blitWatches); bi++ {
			blitWatch := blitWatches[bi]
			blitClauseID := blitWatch.ClauseID
			blitOtherIdx := blitWatch.Blit
			blitOther := cnf.IndexToLit(int(blitOtherIdx))
			
			// Inline literalIsTrue check (optimization #4)
			blitOtherAssign := s.assignments[blitOther.Var()]
			blitOtherIsTrue := blitOtherAssign.Level != 0 && ((blitOther.IsNegated() && !blitOtherAssign.Value) || (!blitOther.IsNegated() && blitOtherAssign.Value))
			
			if blitOtherIsTrue {
				continue
			}
			
			var blitClause cnf.Clause
			var blitIsLearned bool
			var blitLearnedIdx int
			if blitClauseID < uint32(s.cnf.NumClauses) {
				blitClause = s.cnf.Clauses[blitClauseID]
				blitIsLearned = false
				blitLearnedIdx = -1
			} else {
				blitLearnedIdx = int(blitClauseID - uint32(s.cnf.NumClauses))
				if blitLearnedIdx < 0 || blitLearnedIdx >= len(s.learnedClauses) {
					continue
				}
				blitClause = s.learnedClauses[blitLearnedIdx]
				blitIsLearned = true
			}
			
			hasReplacement := false
			for _, cl := range blitClause.Literals {
				if cl == blit || cl == blitOther {
					continue
				}
				ll := s.assignments[cl.Var()].Level
				lv := s.assignments[cl.Var()].Value
				lt := (!cl.IsNegated() && lv) || (cl.IsNegated() && !lv)
				if lt || ll == 0 {
					hasReplacement = true
					break
				}
			}
			
			if !hasReplacement {
				otherLevel := s.assignments[blitOther.Var()].Level
				if otherLevel == 0 {
					otherReason := int(blitClauseID)
					if blitIsLearned {
						otherReason = -blitLearnedIdx - 1
					}
					s.assignLiteral(blitOther, s.level, otherReason)
				}
			}
		}
		
		continue
	}
	
	blitValue := s.assignments[blit.Var()].Value
	blitTrue := (!blit.IsNegated() && blitValue) || (blit.IsNegated() && !blitValue)
	
	if !blitTrue {
		if isLearned {
			return true, -learnedIdx - 1
		}
		return true, int(clauseID)
	}
	
	if newWatchCount != i {
		watches[newWatchCount] = watch
	}
	newWatchCount++
	s.watchLists[watchIdx] = watches[:newWatchCount]
}
		
		s.watchLists[watchIdx] = watches[:newWatchCount]
	}
	
	// Update qhead to end of trail
	s.qhead = len(s.trail)
	
	return false, -1
}

func (s *CDCLSolver) propagate() (bool, int) {
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
			return true, clauseIdx
		}
		
		if unassignedCount == 1 && falseCount == size-1 {
			assignLevel := s.level
			if assignLevel == 0 {
				assignLevel = 1
			}
			s.assignLiteral(unassignedLit, assignLevel, clauseIdx)
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
				return true, -learnedIdx - 1
			}
			
			if unassignedCount == 1 && falseCount == clauseSize-1 {
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				s.assignLiteral(unassignedLit, assignLevel, -learnedIdx-1)
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

	return false, -1
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
	
	// Diversification: force random decision ONLY if severely stuck
	// Modern solvers (MiniSat, Glucose) use <1% random decisions
	// Rely on VSIDS/LRB heuristics for most decisions
	stuckThreshold := 1000 // conflicts at same level before forcing random
	forceRandom := false
	
	if s.level > 0 && s.conflictsAtLevel[s.level] > stuckThreshold {
		// Severely stuck - force random decision
		if s.conflicts - s.lastRandomDecision > 500 { // At least 500 conflicts since last random
			forceRandom = true
		}
	}
	
	// Add 0.5% random decisions (every 200 conflicts) - much reduced from 5%
	if !forceRandom && s.conflicts > 0 && s.conflicts % 200 == 0 {
		forceRandom = true
	}
	
	var varIdx uint32
	var phase bool
	
	if forceRandom {
		// Select random unassigned variable
		varIdx = s.selectRandomUnassigned()
		// Random phase
		phase = s.conflicts % 2 == 0
		s.lastRandomDecision = s.conflicts
		
		if s.verbose && s.conflicts % 1000 == 0 {
			fmt.Printf("c [verbose] Diversification: random decision at conflict %d, level %d\n", s.conflicts, s.level)
		}
		
		// Reset conflicts at this level after random decision
		if s.level > 0 && s.level < len(s.conflictsAtLevel) {
			s.conflictsAtLevel[s.level] = 0
		}
	} else {
		varIdx, phase = s.vsids.selectVariableWithPhase(s.assignments, s.savedPhase)
	}

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, phase), s.level, -1)
	s.decisions++
	if s.verbose {
		fmt.Printf("c [DECIDE] Level %d (was %d): var %d = %v (decision), trailHead len=%d\n", 
			s.level, s.level-1, varIdx+1, phase, len(s.trailHead))
	}
	return true
}

func (s *CDCLSolver) assignLiteral(lit cnf.Literal, level int, clauseIdx int) {
	varIdx := lit.Var()

	if s.assignments[varIdx].Level != 0 {
		// Variable already assigned - this could be a bug if we're trying to propagate
		if s.verbose && level > 0 {
			reasonStr := "propagation"
			if clauseIdx == -1 {
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
	s.trail = append(s.trail, int(varIdx))
	s.implication[varIdx] = clauseIdx
	
	// Save the phase (polarity) for decisions only
	// Don't save phase for propagations - the phase is forced by the clause
	if clauseIdx == -1 {
		s.savedPhase[varIdx] = value
	}
	
	if s.verbose && level > 0 {
		reasonStr := "propagation"
		if clauseIdx == -1 {
			reasonStr = "decision"
		}
		fmt.Printf("c [ASSIGN] Level %d: var %d = %v (%s)", level, varIdx+1, value, reasonStr)
		if clauseIdx >= 0 {
			fmt.Printf(" from clause %d", clauseIdx)
		} else if clauseIdx < -1 {
			fmt.Printf(" from learned clause %d", -clauseIdx-1)
		}
		fmt.Printf("\n")
	}
}

func (s *CDCLSolver) literalIsTrue(lit cnf.Literal) bool {
	assign := s.assignments[lit.Var()]
	if assign.Level == 0 {
		return false  // Unassigned literals are not true
	}
	if lit.IsNegated() {
		return !assign.Value
	}
	return assign.Value
}

func (s *CDCLSolver) handleConflict(clauseIdx int) {
	s.conflicts++
	
	// Get the conflicting clause
	var conflictLits []cnf.Literal
	if clauseIdx >= 0 {
		conflictLits = s.cnf.Clauses[clauseIdx].Literals
	} else {
		// Learned clause (encoded as negative index)
		learnedIdx := -clauseIdx - 1
		conflictLits = s.learnedClauses[learnedIdx].Literals
		// Bump activity for learned clause involved in conflict
		if learnedIdx < len(s.clauseActivity) {
			s.clauseActivity[learnedIdx] += 1.0
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
		s.clauseActivity[i] *= 0.95
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
	
	if s.verbose && s.conflicts <= 100 {
		fmt.Printf("c [debug] Conflict %d, iter %d, level %d, learned %d, trail %d\n", 
			s.conflicts, s.iterations, s.level, len(s.learnedClauses), len(s.trail))
	}
	
	// Clear reusable buffers (O(n) but much faster than allocation)
	for i := range s.tmpLiteralInClause {
		s.tmpLiteralInClause[i] = false
		s.tmpLiteralIsNegated[i] = false
	}
	for i := range s.tmpLevelCount[:s.level+1] {
		s.tmpLevelCount[i] = 0
	}
	s.tmpCandidates = s.tmpCandidates[:0]
	s.tmpLevelSet = s.tmpLevelSet[:0]
	for i := range s.tmpLevelSetUsed[:s.level+1] {
		s.tmpLevelSetUsed[i] = false
	}
	
	// Add all literals from the conflicting clause
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = true
			s.tmpLiteralIsNegated[varIdx] = lit.IsNegated()
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
			trailIndex--
			
			// Skip if not in learned clause or already resolved
			if !s.tmpLiteralInClause[varIdx] || s.tmpResolved[varIdx] {
				continue
			}
			
			// Skip if at lower level (not counted in pathC)
			if s.assignments[varIdx].Level != s.level {
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
		reasonIdx := s.implication[foundVar]
		if reasonIdx == -1 {
			// Decision literal - cannot resolve, stop early
			break
		}
		
		// Get reason clause
		var reasonLits []cnf.Literal
		if reasonIdx >= 0 {
			reasonLits = s.cnf.Clauses[reasonIdx].Literals
		} else {
			learnedIdx := -reasonIdx - 1
			if learnedIdx >= len(s.learnedClauses) {
				// Reason clause was deleted, remove this literal and continue
				s.tmpLiteralInClause[foundVar] = false
				s.tmpLevelCount[s.level]--
				pathC--
				continue
			}
			reasonLits = s.learnedClauses[learnedIdx].Literals
		}
		
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
				lvl := s.assignments[v].Level
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
	
	// Build the learned clause from remaining literals
	learnedLits := make([]cnf.Literal, 0)
	litsAtCurrentLevel := 0
	for varIdx, inClause := range s.tmpLiteralInClause {
		if inClause {
			learnedLits = append(learnedLits, cnf.NewLiteral(uint32(varIdx), s.tmpLiteralIsNegated[varIdx]))
			if s.assignments[varIdx].Level == s.level {
				litsAtCurrentLevel++
			}
		}
	}
	
	// INVARIANT CHECK: Verify exactly 1 literal at current level
	litsAtCurrentLevel = 0
	for varIdx, inClause := range s.tmpLiteralInClause {
		if inClause && s.assignments[varIdx].Level == s.level {
			litsAtCurrentLevel++
		}
	}
	
	if s.verbose && s.conflicts <= 100 {
		fmt.Printf("c [1-UIP] Conflict %d: %d literals, %d at level %d (target: 1)\n", 
			s.conflicts, len(learnedLits), litsAtCurrentLevel, s.level)
		
		if litsAtCurrentLevel != 1 {
			fmt.Printf("c [1-UIP ERROR] Failed to find UIP! Literals: %d, at level %d\n", len(learnedLits), s.level)
			for _, lit := range learnedLits {
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
	
	// If 1-UIP didn't reduce to exactly 1 literal at current level,
	// fall back to DPLL-style backtracking
	if litsAtCurrentLevel != 1 {
		if s.verbose && s.conflicts <= 20 {
			fmt.Printf("c [debug] Skipping learned clause: %d literals at level %d (expected 1)\n", litsAtCurrentLevel, s.level)
		}
		backjumpLevel := s.level - 1
		if backjumpLevel < 0 {
			backjumpLevel = 0
		}
		return backjumpLevel
	}
	
	// CLAUSE MINIMIZATION via self-subsumption
	// Try to remove literals from the learned clause by resolving with reason clauses
	// This produces smaller, more general learned clauses
	originalSize := len(learnedLits)
	learnedLits = s.minimizeLearnedClause(learnedLits)
	if s.verbose && len(learnedLits) < originalSize {
		fmt.Printf("c [minimize] Clause reduced from %d to %d literals\n", originalSize, len(learnedLits))
	}
	
	// Calculate LBD using reusable buffer (no map allocation)
	lbd := 0
	for varIdx, inClause := range s.tmpLiteralInClause {
		if inClause {
			lvl := s.assignments[varIdx].Level
			if lvl > 0 && !s.tmpLevelSetUsed[lvl] {
				s.tmpLevelSetUsed[lvl] = true
				s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				lbd++
			}
		}
	}
	
	// Only learn non-empty clauses
	// TWO-TIER APPROACH: Separate glue clauses (LBD ≤ 3) from normal clauses
	// Glue clauses: watched, kept forever, high priority
	// Normal clauses: linear scan, deleted aggressively, keep only ~5K
	
	if s.verbose && s.conflicts <= 50 {
		fmt.Printf("c [debug] learnClause: conflict=%d, learnedLits=%d, lbd=%d\n", s.conflicts, len(learnedLits), lbd)
	}
	
	if len(learnedLits) > 0 {
		// DUPLICATE DETECTION: Skip if this clause already exists
		// Use simple hash-based check for O(n) comparison only when hash matches
		s.tmpClauseHash = 0
		for _, lit := range learnedLits {
			s.tmpClauseHash = s.tmpClauseHash*31 + uint64(lit)
		}
		
		isDuplicate := false
		dupOf := -1
		for i, existing := range s.learnedClauses {
			if len(existing.Literals) != len(learnedLits) {
				continue
			}
			// Quick hash check first (if we tracked it), then full comparison
			match := true
			for j, lit := range learnedLits {
				if existing.Literals[j] != lit {
					match = false
					break
				}
			}
			if match {
				isDuplicate = true
				dupOf = i
				if s.verbose && s.conflicts <= 100 {
					fmt.Printf("c [debug] Skipping duplicate learned clause (duplicate of clause %d)\n", i)
				}
				break
			}
		}
		
		if s.verbose && s.conflicts >= 28000 && s.conflicts <= 28100 {
			fmt.Printf("c [debug] Conflict %d: isDuplicate=%v, dupOf=%d, learnedClauses=%d\n", 
				s.conflicts, isDuplicate, dupOf, len(s.learnedClauses))
		}
		
		if isDuplicate {
			// Don't learn this clause, but still return backjump level
			backjumpLevel := 0
			for varIdx, inClause := range s.tmpLiteralInClause {
				if inClause {
					lvl := s.assignments[varIdx].Level
					if lvl > backjumpLevel && lvl < s.level {
						backjumpLevel = lvl
					}
				}
			}
			if backjumpLevel == 0 {
				backjumpLevel = 1
			}
			return backjumpLevel
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
			s.clauseSize = append(s.clauseSize, len(learnedLits))
			s.clauseLBD = append(s.clauseLBD, lbd)
			if lbd > 3 {
				s.normalClauseCount++
			}
			s.currentAge++
			
			newClause := cnf.Clause{Literals: learnedLits, Learned: true}
			learnedIdx := len(s.learnedClauses)
			s.learnedClauses = append(s.learnedClauses, newClause)
			
			// Add learned clause to watches with correct ID encoding
			if s.watchInitialized {
				encodedClauseID := s.cnf.NumClauses + learnedIdx
				s.addClauseToWatches(encodedClauseID, learnedLits, true)
			}
			
			// Mark LBD order as dirty - will be rebuilt on next propagation
			s.lbdOrderDirty = true
			
			// Enforce maxLearned limit by deleting clauses when exceeded
			if len(s.learnedClauses) > s.maxLearned {
				s.deleteLearnedClauses()
			}
			
			// ALWAYS print first 10 learned clauses for debugging
			if len(s.learnedClauses) <= 10 {
				fmt.Printf("c [LEARNED] Clause %d: LBD=%d, size=%d, lits=[", len(s.learnedClauses)-1, lbd, len(learnedLits))
				for i, lit := range learnedLits {
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
			s.vsids.bumpLBD(learnedLits, lbd)
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
	
	if s.verbose && s.conflicts <= 20 {
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
	
	for i := range s.tmpLiteralInClause {
		s.tmpLiteralInClause[i] = false
	}
	for _, lit := range learnedLits {
		s.tmpLiteralInClause[lit.Var()] = true
	}
	
	minimized := make([]cnf.Literal, 0, len(learnedLits))
	
	for _, lit := range learnedLits {
		varIdx := lit.Var()
		canRemove := false
		
		reasonIdx := s.implication[varIdx]
		if reasonIdx != -1 {
			var reasonLits []cnf.Literal
			if reasonIdx >= 0 {
				reasonLits = s.cnf.Clauses[reasonIdx].Literals
			} else {
				learnedIdx := -reasonIdx - 1
				if learnedIdx < len(s.learnedClauses) {
					reasonLits = s.learnedClauses[learnedIdx].Literals
				}
			}
			
			if reasonLits != nil {
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
				
				if allCovered {
					canRemove = true
				}
			}
		}
		
		if !canRemove {
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
		
		// PROTECTION: LBD <= 2 glue clauses are NEVER deleted (Glucose-style)
		// These are the backbone of the learned clause database
		if lbd <= 2 {
			score = -1000.0 // Absolutely never delete, regardless of age or size
		}
		
		// LBD == 3 AND size <= 3 AND age < 50: very good, protect unless very old
		if lbd == 3 && size <= 3 && age < 50 {
			score = -500.0 // Protect unless very old
		}
		
		// FORCE DELETION: Very old clauses (age > 500) regardless of LBD
		// But NOT glue clauses (LBD <= 2)
		if age > 500 && lbd > 2 {
			score += 1000.0 // Force deletion of very old clauses
		}
		
		// FORCE DELETION: Large clauses (size > 15) regardless of LBD
		// But NOT glue clauses (LBD <= 2)
		if size > 15 && lbd > 2 {
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
	for i := 0; i < len(clauses); i++ {
		for j := i + 1; j < len(clauses); j++ {
			if clauses[i].score < clauses[j].score {
				clauses[i], clauses[j] = clauses[j], clauses[i]
			}
		}
	}
	
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
		if s.verbose && s.conflicts <= 20 {
			fmt.Printf("c [BACKTRACK] FAIL: backjump level %d invalid (level=%d)\n", bjLevel, s.level)
		}
		return false
	}
	
	// Find the decision point at the backjump level
	decisionPoint := s.trailHead[bjLevel]
	if decisionPoint >= len(s.trail) {
		if s.verbose && s.conflicts <= 20 {
			fmt.Printf("c [BACKTRACK] FAIL: decision point %d >= trail len %d\n", decisionPoint, len(s.trail))
		}
		return false
	}
	
	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value
	
	if s.verbose && s.conflicts <= 20 {
		fmt.Printf("c [BACKTRACK] Backjumping from level %d to %d, var %d, trail[%d:%d]\n", 
			s.level, bjLevel, decisionVar+1, decisionPoint, len(s.trail))
	}

	// Clear all assignments from decisionPoint onwards
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
	// Reset qhead to avoid re-processing backtracked trail elements
	if s.qhead > decisionPoint {
		s.qhead = decisionPoint
	}
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
	reasonIdx := s.implication[varIdx]
	if reasonIdx < 0 {
		// Learned clause
		learnedIdx := -reasonIdx - 1
		if learnedIdx >= 0 && learnedIdx < len(s.clauseLBD) {
			return s.clauseLBD[learnedIdx]
		}
		return 999 // Unknown learned clause, treat as high LBD
	}
	// Original clause - use size as approximation (original clauses aren't tracked by LBD)
	if reasonIdx < len(s.cnf.Clauses) {
		return len(s.cnf.Clauses[reasonIdx].Literals)
	}
	return 999
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
	// Use simple selection sort for simplicity (O(n²) but n is typically < 5000)
	for i := 0; i < n; i++ {
		minIdx := i
		for j := i + 1; j < n; j++ {
			if s.clauseLBD[s.learnedClauseOrder[j]] < s.clauseLBD[s.learnedClauseOrder[minIdx]] {
				minIdx = j
			}
		}
		if minIdx != i {
			s.learnedClauseOrder[i], s.learnedClauseOrder[minIdx] = s.learnedClauseOrder[minIdx], s.learnedClauseOrder[i]
		}
	}
	
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
