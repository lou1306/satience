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
	learnedClauses []cnf.Clause      // Original clauses (keep for compatibility)
	learnedArena *cnf.ClauseArena    // Arena-based learned clause storage
	clauseActivity []float64
	clauseAge    []int
	clauseSize   []int // Track clause size for deletion
	clauseLBD    []int // Track LBD at time of learning
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
}

// resolveCandidate is used in learnClause for sorting resolution order
type resolveCandidate struct {
	varIdx uint32
	size   int
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	maxLearned := 100   // CRITICAL: Keep learned clause database small to avoid O(n) propagation slowdown
	minLearned := 50    // Target after deletion (50% reduction)
	restartBase := 100  // Base for Luby restart sequence
	
	solver := &CDCLSolver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		trailHead:   make([]int, 1),
		level:       0,
		vsids:       NewVSIDS(formula.NumVars),
		conflicts:   0,
		implication: make([]int, formula.NumVars),
		iterations:  0,
		maxIter:     0,
		learnedClauses: make([]cnf.Clause, 0),
		learnedArena: cnf.NewClauseArena(50000), // Pre-allocate arena (200 KB)
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
		
		unitResult := s.unitPropagationPreprocess()
		if unitResult != UNKNOWN {
			return unitResult
		}
		
		pureResult := s.pureLiteralElimination()
		if pureResult != UNKNOWN {
			return pureResult
		}
		
		s.selfSubsumption()
		
		s.hyperBinaryResolution()
		
		// DISABLED: Equivalence detection causes issues with certain patterns
		// Needs more testing before re-enabling
		// equivResult := s.equivalenceDetection()
		// if equivResult != UNKNOWN {
		// 	return equivResult
		// }
		
		// RE-ENABLED: Failed literal elimination with strict safeguards
		failedResult := s.failedLiteralElimination()
		if failedResult != UNKNOWN {
			return failedResult
		}
		
		veResult := s.variableElimination()
		if veResult != UNKNOWN {
			return veResult
		}
		
		// Stop if no progress made for 2 consecutive passes
		if s.cnf.NumClauses == initialClauses && pass >= 1 {
			break
		}
		initialClauses = s.cnf.NumClauses
	}
	
	bceResult := s.blockedClauseElimination()
	if bceResult != UNKNOWN {
		return bceResult
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] After preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}
	
	return UNKNOWN
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
	
	if s.verbose && removed > 0 {
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
//
// Inprocessing is the application of preprocessing techniques during the search phase.
// This is crucial for maintaining a small, simplified formula throughout solving.
//
// What it does:
// 1. Remove original clauses subsumed by shorter original clauses
// 2. Remove original clauses subsumed by learned clauses
// 3. Remove learned clauses subsumed by other learned clauses
//
// Why it helps:
// - Learned clauses can subsume original clauses (especially short learned clauses)
// - Reduces the formula size, making propagation faster
// - Removes redundant constraints that slow down search
//
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
//
// Algorithm:
// For each unassigned variable x, try assigning x=false and propagate.
// If conflict occurs, then x must be true (failed literal).
// This is a powerful preprocessing technique but can be expensive.
//
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
//
// Restart Policies in CDCL Solvers:
// Restarts are essential for modern SAT solvers. They escape unproductive search
// regions where the solver is making poor decisions or exploring fruitless branches.
//
// Two Restart Policies Implemented:
//
// 1. Glucose-Style Adaptive Restarts (PRIMARY, more aggressive):
//    - Monitor the LBD of learned clauses during search
//    - When current LBD > 1.5× average LBD, the search is unproductive
//    - Restart immediately to try different decisions
//    - This is reactive: restarts based on actual search quality
//    
//    Why it works:
//    - High LBD means the learned clause spans many decision levels
//    - This indicates the search is "lost" - decisions don't connect well
//    - Restarting allows the solver to make different decisions
//    - The 1.5× threshold is empirically optimal (Glucose solver)
//
// 2. Luby Sequence (FALLBACK, conservative):
//    - Geometric sequence: 1, 1, 2, 1, 1, 2, 4, 1, 1, 2, ...
//    - Multiply by restartBase (default 100) for conflict threshold
//    - Guaranteed to restart periodically even if LBD criterion not met
//    - This is proactive: restarts based on conflict count
//
// Hybrid Approach:
// - First 50 conflicts: Use Luby (need LBD statistics)
// - After 50 conflicts: Use Glucose criterion (more aggressive)
// - If Glucose criterion not met: Fall back to Luby
//
// What Happens on Restart:
// 1. Clear the trail (all assignments)
// 2. Keep only "glue clauses" (LBD ≤ 3) - most valuable learned clauses
// 3. Delete all other learned clauses (50-90% reduction)
// 4. Reset LBD statistics for fresh measurement
// 5. Continue search with same VSIDS scores (learnings preserved)
//
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
		
		// Keep glue clauses (LBD <= 3) - Glucose-style strict threshold
		// LBD <= 2: core glue (most valuable, never delete)
		// LBD 3: useful glue (keep across restarts)
		// LBD > 3: trash (delete on restart)
		if lbd <= 3 {
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
	
	// Inprocessing: DISABLED due to soundness bug with watched literals
	// When clauses are removed, ternary watch structures become stale
	// Fix requires rebuilding watches after clause removal (expensive)
	// TODO: Fix by calling InitializeWatches() after inprocessing
	// if s.conflicts > 0 && s.conflicts % 500 == 0 {
	// 	s.inprocessing()
	// }
	
	// Reset restart counters
	s.lubyIndex++
	s.restartCount = s.conflicts
	s.lbdSum = 0
	s.lbdCount = 0
	s.lastConflictLBD = 0
	s.backjumpLevel = 0
	
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
		return SAT
	}
	
	return UNKNOWN
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
	
	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) != 2 {
			continue
		}
		
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
		if s.verbose {
			fmt.Printf("c [verbose] Equivalence detection: no equivalences found\n")
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
				newLit := cnf.NewLiteral(subst.rep, lit.IsNegated() != subst.samePol)
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
				fmt.Printf("c [verbose] Equivalence detection: empty clause (UNSAT)\n")
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
		fmt.Printf("c [verbose] Equivalence detection: eliminated %d variables\n", len(substMap))
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
		return SAT
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

func (s *CDCLSolver) SolveWithResult() SolveResult {
	preprocessResult := s.preprocessAggressive()
	if preprocessResult != UNKNOWN {
		if s.verbose {
			s.printStats()
		}
		return preprocessResult
	}

	// Initialize VSIDS with clause-length weighted activity BEFORE search
	// Variables in shorter clauses get higher activity (more constrained = more important)
	s.vsids.InitializeFromClauses(s.cnf.Clauses)

	// Rebuild short clause caches AFTER preprocessing (preprocessing modifies clauses)
	// This ensures BinaryClauses and TernaryClauses arrays are up-to-date
	s.cnf.RebuildShortClauses()

	// Initialize watched literals AFTER preprocessing (preprocessing modifies clauses)
	s.cnf.InitializeWatches()

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
				fmt.Printf("c [verbose] Conflict %d, level %d, learned %d\n", s.conflicts, s.level, len(s.learnedClauses))
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

// propagateBinary propagates binary clauses using watched literals
// Returns (conflict, clauseIdx) where clauseIdx >= 0 means unit propagated (encoded), < 0 means conflict
//
// Watched Literals Invariant:
// Each binary clause watches two literals. When a watched literal becomes false,
// we must either find a new watch or propagate/conflict. This ensures O(1) amortized
// propagation cost per clause.
//
// Algorithm:
// 1. When literal L becomes false, check all clauses watching L
// 2. For each clause, check the other watch
// 3. If other watch is true: clause satisfied, continue
// 4. If other watch is false: CONFLICT
// 5. If other watch is unassigned: try to find new watch among clause literals
//    - Found unassigned/true literal: update watches (lazy - keep false watch)
//    - No alternative: propagate other watch to true
func (s *CDCLSolver) propagateBinary() (bool, int) {
	if s.cnf.BinaryWatchA == nil {
		return false, -1
	}
	
	for trailIdx := s.trailHead[s.level]; trailIdx < len(s.trail); trailIdx++ {
		assignedVar := uint32(s.trail[trailIdx])
		assignedValue := s.assignments[assignedVar].Value
		
		falseLit := cnf.NewLiteral(assignedVar, assignedValue)
		falseLitIdx := cnf.LitToIndex(falseLit)
		watchList := s.cnf.WatchList[falseLitIdx]
		
		for i := 0; i < len(watchList); i++ {
			binIdx := watchList[i]
			binClause := s.cnf.BinaryClauses[binIdx]
			
			watchAIdx := s.cnf.BinaryWatchA[binIdx]
			watchBIdx := s.cnf.BinaryWatchB[binIdx]
			watchA := cnf.IndexToLit(watchAIdx)
			watchB := cnf.IndexToLit(watchBIdx)
			
			var otherWatch cnf.Literal
			var otherWatchIdx int
			
			if watchA == falseLit {
				otherWatch = watchB
				otherWatchIdx = watchBIdx
			} else if watchB == falseLit {
				otherWatch = watchA
				otherWatchIdx = watchAIdx
			} else {
				continue
			}
			
			otherVar := otherWatch.Var()
			
			if s.assignments[otherVar].Level != 0 {
				otherValue := s.assignments[otherVar].Value
				otherIsTrue := (!otherWatch.IsNegated() && otherValue) || (otherWatch.IsNegated() && !otherValue)
				
				if otherIsTrue {
					continue
				}
				return true, -binIdx - 2
			}
			
			lit1 := cnf.Literal(binClause.Lit1)
			lit2 := cnf.Literal(binClause.Lit2)
			foundNewWatch := false
			
			s.tryNewWatch(lit1, falseLitIdx, otherWatchIdx, watchA == falseLit, binIdx, &foundNewWatch)
			if !foundNewWatch {
				s.tryNewWatch(lit2, falseLitIdx, otherWatchIdx, watchA == falseLit, binIdx, &foundNewWatch)
			}
			
			if !foundNewWatch {
				propLit := otherWatch
				propVar := propLit.Var()
				
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				s.assignLiteral(propLit, assignLevel, -binIdx-2)
				return false, int(propVar) + 1
			}
		}
	}
	
	return false, -1
}

// tryNewWatch attempts to update watches for a binary clause
// Returns true if a new watch was found
func (s *CDCLSolver) tryNewWatch(lit cnf.Literal, falseLitIdx, otherWatchIdx int, watchAIsFalse bool, binIdx int, foundNewWatch *bool) {
	varIdx := lit.Var()
	litIsFalse := s.assignments[varIdx].Level != 0 && 
		((!lit.IsNegated() && !s.assignments[varIdx].Value) || 
		 (lit.IsNegated() && s.assignments[varIdx].Value))
	
	if !litIsFalse {
		newWatchIdx := cnf.LitToIndex(lit)
		if newWatchIdx != otherWatchIdx && newWatchIdx != falseLitIdx {
			if watchAIsFalse {
				s.cnf.BinaryWatchA[binIdx] = falseLitIdx
				s.cnf.BinaryWatchB[binIdx] = newWatchIdx
			} else {
				s.cnf.BinaryWatchA[binIdx] = newWatchIdx
				s.cnf.BinaryWatchB[binIdx] = falseLitIdx
			}
			s.cnf.WatchList[newWatchIdx] = append(s.cnf.WatchList[newWatchIdx], binIdx)
			*foundNewWatch = true
		}
	}
}

// propagateTernary propagates ternary clauses (3 literals) using watched literals
// Returns (conflict, clauseIdx) where clauseIdx >= 0 means unit propagated, < 0 means no issue/conflict
func (s *CDCLSolver) propagateTernary() (bool, int) {
	// Skip if watches not initialized yet (during preprocessing)
	if s.cnf.TernaryWatchA == nil {
		return false, -1
	}
	
	// Process all assigned literals on the trail that haven't been processed yet
	for trailIdx := s.trailHead[s.level]; trailIdx < len(s.trail); trailIdx++ {
		assignedVar := uint32(s.trail[trailIdx])
		assignedValue := s.assignments[assignedVar].Value
		
		// When a literal becomes false, check clauses watching it
		falseLit := cnf.NewLiteral(assignedVar, assignedValue) // The literal that is FALSE
		falseLitIdx := cnf.LitToIndex(falseLit)
		
		// Get all ternary clauses watching this false literal
		watchList := s.cnf.TernaryWatchList[falseLitIdx]
		
		for i := 0; i < len(watchList); i++ {
			ternIdx := watchList[i]
			clauseIdx := s.cnf.TernaryClauseIndices[ternIdx]
			
			// Handle learned clauses (negative index encoding)
			var clause cnf.Clause
			if clauseIdx < 0 {
				learnedIdx := -clauseIdx - 1
				if learnedIdx >= len(s.learnedClauses) {
					continue // Skip invalid learned clause reference
				}
				clause = s.learnedClauses[learnedIdx]
			} else {
				if clauseIdx >= len(s.cnf.Clauses) {
					continue // Skip invalid original clause reference
				}
				clause = s.cnf.Clauses[clauseIdx]
			}
			
			// Get the three watched literals
			watchAIdx := s.cnf.TernaryWatchA[ternIdx]
			watchBIdx := s.cnf.TernaryWatchB[ternIdx]
			watchCIdx := s.cnf.TernaryWatchC[ternIdx]
			
			watchA := cnf.IndexToLit(watchAIdx)
			watchB := cnf.IndexToLit(watchBIdx)
			watchC := cnf.IndexToLit(watchCIdx)
			
			// One of these watches should be the false literal
			// The other two watches are what we need to check
			var otherWatch1, otherWatch2 cnf.Literal
			var otherWatchIdx1, otherWatchIdx2 int
			
			if watchA == falseLit {
				otherWatch1 = watchB
				otherWatchIdx1 = watchBIdx
				otherWatch2 = watchC
				otherWatchIdx2 = watchCIdx
			} else if watchB == falseLit {
				otherWatch1 = watchA
				otherWatchIdx1 = watchAIdx
				otherWatch2 = watchC
				otherWatchIdx2 = watchCIdx
			} else if watchC == falseLit {
				otherWatch1 = watchA
				otherWatchIdx1 = watchAIdx
				otherWatch2 = watchB
				otherWatchIdx2 = watchBIdx
			} else {
				// Neither watch is the false literal - skip this clause
				continue
			}
			
			// Check the other two watches
			var1 := otherWatch1.Var()
			var2 := otherWatch2.Var()
			
			assigned1 := s.assignments[var1].Level != 0
			assigned2 := s.assignments[var2].Level != 0
			
			if assigned1 && assigned2 {
				// Both other watches are already assigned
				value1 := s.assignments[var1].Value
				value2 := s.assignments[var2].Value
				
				isTrue1 := (!otherWatch1.IsNegated() && value1) || (otherWatch1.IsNegated() && !value1)
				isTrue2 := (!otherWatch2.IsNegated() && value2) || (otherWatch2.IsNegated() && !value2)
				
				if isTrue1 || isTrue2 {
					// Clause is satisfied by one of the other watches
					continue
				}
				// All three watches are false - CONFLICT!
				return true, clauseIdx
			}
			
			// At least one of the other watches is unassigned
			// Try to find a new watch among the unwatched literal
			lit1 := clause.Literals[0]
			lit2 := clause.Literals[1]
			lit3 := clause.Literals[2]
			
			foundNewWatch := false
			
			// Try each clause literal as a potential new watch
			for _, lit := range []cnf.Literal{lit1, lit2, lit3} {
				litIdx := cnf.LitToIndex(lit)
				
				// Skip if this is one of the current watches
				if litIdx == watchAIdx || litIdx == watchBIdx || litIdx == watchCIdx {
					continue
				}
				
				// Check if this literal can be a new watch
				varIdx := lit.Var()
				if s.assignments[varIdx].Level == 0 {
					// Unassigned literal - perfect new watch!
					// Update watches: keep falseLit, replace the false watch with this lit
					if watchA == falseLit {
						s.cnf.TernaryWatchA[ternIdx] = falseLitIdx
						s.cnf.TernaryWatchB[ternIdx] = otherWatchIdx1
						s.cnf.TernaryWatchC[ternIdx] = litIdx
					} else if watchB == falseLit {
						s.cnf.TernaryWatchA[ternIdx] = otherWatchIdx1
						s.cnf.TernaryWatchB[ternIdx] = falseLitIdx
						s.cnf.TernaryWatchC[ternIdx] = litIdx
					} else {
						s.cnf.TernaryWatchA[ternIdx] = otherWatchIdx1
						s.cnf.TernaryWatchB[ternIdx] = otherWatchIdx2
						s.cnf.TernaryWatchC[ternIdx] = falseLitIdx
					}
					
					// Update watch lists (lazy - just add to new watch list)
					s.cnf.TernaryWatchList[litIdx] = append(s.cnf.TernaryWatchList[litIdx], ternIdx)
					
					foundNewWatch = true
					break
				}
				
				// Check if assigned literal is true
				litValue := s.assignments[varIdx].Value
				litIsTrue := (!lit.IsNegated() && litValue) || (lit.IsNegated() && !litValue)
				if litIsTrue {
					// True literal - good new watch
					if watchA == falseLit {
						s.cnf.TernaryWatchA[ternIdx] = falseLitIdx
						s.cnf.TernaryWatchB[ternIdx] = otherWatchIdx1
						s.cnf.TernaryWatchC[ternIdx] = litIdx
					} else if watchB == falseLit {
						s.cnf.TernaryWatchA[ternIdx] = otherWatchIdx1
						s.cnf.TernaryWatchB[ternIdx] = falseLitIdx
						s.cnf.TernaryWatchC[ternIdx] = litIdx
					} else {
						s.cnf.TernaryWatchA[ternIdx] = otherWatchIdx1
						s.cnf.TernaryWatchB[ternIdx] = otherWatchIdx2
						s.cnf.TernaryWatchC[ternIdx] = litIdx
					}
					
					s.cnf.TernaryWatchList[litIdx] = append(s.cnf.TernaryWatchList[litIdx], ternIdx)
					foundNewWatch = true
					break
				}
			}
			
			if !foundNewWatch {
				// No alternative watch found - check if we can propagate
				// If exactly one of the other watches is unassigned, propagate it
				if !assigned1 && assigned2 {
					// var1 is unassigned, var2 is assigned (and false)
					// Propagate otherWatch1 to true
					assignLevel := s.level
					if assignLevel == 0 {
						assignLevel = 1
					}
					s.assignLiteral(otherWatch1, assignLevel, clauseIdx)
					return false, int(var1) + 1 // Signal propagation
				} else if assigned1 && !assigned2 {
					// var2 is unassigned, var1 is assigned (and false)
					// Propagate otherWatch2 to true
					assignLevel := s.level
					if assignLevel == 0 {
						assignLevel = 1
					}
					s.assignLiteral(otherWatch2, assignLevel, clauseIdx)
					return false, int(var2) + 1 // Signal propagation
				} else if !assigned1 && !assigned2 {
					// Both unassigned - can't propagate yet, but clause is not in danger
					// Keep watching, no action needed
					continue
				}
			}
		}
	}
	
	return false, -1 // No conflict, no propagation
}

// propagateLong propagates long clauses (>3 literals) using watched literals
// Returns (conflict, clauseIdx) where clauseIdx >= 0 means conflict/unit, < 0 means no issue
func (s *CDCLSolver) propagateLong() (bool, int) {
	// Skip if watches not initialized yet (during preprocessing)
	if s.cnf.LongWatchA == nil {
		return false, -1
	}
	
	// Process all assigned literals on the trail that haven't been processed yet
	for trailIdx := s.trailHead[s.level]; trailIdx < len(s.trail); trailIdx++ {
		assignedVar := uint32(s.trail[trailIdx])
		assignedValue := s.assignments[assignedVar].Value
		
		// When a literal becomes false, check clauses watching it
		falseLit := cnf.NewLiteral(assignedVar, assignedValue) // The literal that is FALSE
		falseLitIdx := cnf.LitToIndex(falseLit)
		
		// Get all long clauses watching this false literal
		watchList := s.cnf.WatchListLong[falseLitIdx]
		
		for i := 0; i < len(watchList); i++ {
			watchIdx := watchList[i]
			clauseIdx := s.cnf.LongClauseIndices[watchIdx]
			
			// Handle learned clauses (negative index encoding)
			var clause *cnf.Clause
			if clauseIdx < 0 {
				learnedIdx := -clauseIdx - 1
				if learnedIdx >= len(s.learnedClauses) {
					continue // Skip invalid learned clause reference
				}
				clause = &s.learnedClauses[learnedIdx]
			} else {
				if clauseIdx >= len(s.cnf.Clauses) {
					continue // Skip invalid original clause reference
				}
				clause = &s.cnf.Clauses[clauseIdx]
			}
			
			// Get the two watched literals
			watchAIdx := s.cnf.LongWatchA[watchIdx]
			watchBIdx := s.cnf.LongWatchB[watchIdx]
			
			watchA := cnf.IndexToLit(watchAIdx)
			watchB := cnf.IndexToLit(watchBIdx)
			
			// One of these watches should be the false literal
			// The other watch is what we need to check
			var otherWatch cnf.Literal
			
			if watchA == falseLit {
				otherWatch = watchB
			} else if watchB == falseLit {
				otherWatch = watchA
			} else {
				// Neither watch is the false literal - this shouldn't happen
				// Skip this clause (it's watching other literals)
				continue
			}
			
			// Check the other watch
			otherVar := otherWatch.Var()
			
			if s.assignments[otherVar].Level != 0 {
				// Other literal is already assigned
				otherValue := s.assignments[otherVar].Value
				otherIsTrue := (!otherWatch.IsNegated() && otherValue) || (otherWatch.IsNegated() && !otherValue)
				
				if otherIsTrue {
					// Clause is satisfied by the other watch
					continue
				}
				// Both watches are false - need to find a new watch
			}
			
			// Try to find a new watch among the unwatched literals
			foundNewWatch := false
			for _, lit := range clause.Literals {
				litIdx := cnf.LitToIndex(lit)
				
				// Skip if this is one of the current watches
				if litIdx == watchAIdx || litIdx == watchBIdx {
					continue
				}
				
				// Check if this literal can be a new watch
				varIdx := lit.Var()
				if s.assignments[varIdx].Level == 0 {
					// Unassigned literal - perfect new watch!
					// Update watches: keep falseLit, replace otherWatch with this lit
					if watchA == falseLit {
						s.cnf.LongWatchA[watchIdx] = falseLitIdx
						s.cnf.LongWatchB[watchIdx] = litIdx
					} else {
						s.cnf.LongWatchA[watchIdx] = litIdx
						s.cnf.LongWatchB[watchIdx] = falseLitIdx
					}
					
					// Update watch lists
					// Remove from old watch list, add to new one
					// For simplicity, just add to new watch list (lazy cleanup)
					s.cnf.WatchListLong[litIdx] = append(s.cnf.WatchListLong[litIdx], watchIdx)
					
					foundNewWatch = true
					break
				}
				
				// Check if assigned literal is true
				litValue := s.assignments[varIdx].Value
				litIsTrue := (!lit.IsNegated() && litValue) || (lit.IsNegated() && !litValue)
				if litIsTrue {
					// True literal - good new watch
					if watchA == falseLit {
						s.cnf.LongWatchA[watchIdx] = falseLitIdx
						s.cnf.LongWatchB[watchIdx] = litIdx
					} else {
						s.cnf.LongWatchA[watchIdx] = litIdx
						s.cnf.LongWatchB[watchIdx] = falseLitIdx
					}
					
					s.cnf.WatchListLong[litIdx] = append(s.cnf.WatchListLong[litIdx], watchIdx)
					foundNewWatch = true
					break
				}
			}
			
			if !foundNewWatch {
				// No alternative watch found - the other watch MUST be true
				// If other watch is unassigned, this is unit propagation
				if s.assignments[otherVar].Level == 0 {
					// Unit propagation - assign it
					assignLevel := s.level
					if assignLevel == 0 {
						assignLevel = 1
					}
					s.assignLiteral(otherWatch, assignLevel, clauseIdx)
					return false, int(otherVar) + 1 // Signal propagation
				}
				// Both watches false and no new watch - CONFLICT!
				return true, clauseIdx
			}
		}
	}
	
	return false, -1 // No conflict, no propagation
}

func (s *CDCLSolver) propagate() (bool, int) {
	trailIndex := s.trailHead[s.level]

	// For initial unit propagation at level 0, we need to check all clauses even with empty trail
	// Use a do-while pattern: always check at least once per level
	firstPass := true
	for firstPass || trailIndex < len(s.trail) {
		firstPass = false
		unitPropagated := false
		
		// Propagate binary clauses using watched literals (O(1) per clause)
		conflict, clauseIdx := s.propagateBinary()
		if conflict {
			return true, clauseIdx
		}
		if clauseIdx >= 0 {
			// Unit propagation happened (clauseIdx encodes which literal was propagated)
			unitPropagated = true
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		// Propagate ternary clauses using watched literals
		conflict, clauseIdx = s.propagateTernary()
		if conflict {
			return true, clauseIdx
		}
		if clauseIdx >= 0 {
			// Unit propagation happened
			unitPropagated = true
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		// Propagate long clauses using watched literals
		conflict, clauseIdx = s.propagateLong()
		if conflict {
			return true, clauseIdx
		}
		if clauseIdx >= 0 {
			// Unit propagation happened
			unitPropagated = true
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		// Check original clauses not using watched literals
		// (only 4-literal clauses; binary/ternary and >3 literal use watched literals)
		for clauseIdx := range s.cnf.Clauses {
			clause := &s.cnf.Clauses[clauseIdx]
			
			// Skip binary clauses (handled by propagateBinary)
			if len(clause.Literals) == 2 {
				continue
			}
			
			// Skip ternary clauses (handled by propagateTernary)
			if len(clause.Literals) == 3 {
				continue
			}
			
			// Skip long clauses (handled by propagateLong)
			if len(clause.Literals) > 4 {
				continue
			}
			
			// Count satisfied, false, and unassigned literals
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
					// Inline literalIsTrue check for performance
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
				continue // Clause is satisfied
			}
			
			if unassignedCount == 0 && falseCount > 0 {
				// All literals are false - conflict!
				return true, clauseIdx
			}
			
			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				// Unit clause - propagate the unassigned literal
				// Use max(1, s.level) to ensure we never assign at level 0
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				s.assignLiteral(unassignedLit, assignLevel, clauseIdx)
				unitPropagated = true
				break
			}
		}
		
		// If we propagated a unit, restart from beginning
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
	return true
}

func (s *CDCLSolver) assignLiteral(lit cnf.Literal, level int, clauseIdx int) {
	varIdx := lit.Var()

	if s.assignments[varIdx].Level != 0 {
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value: value,
		Level: level,
	}
	s.trail = append(s.trail, int(varIdx))
	s.implication[varIdx] = clauseIdx
	
	// Save the phase (polarity) that satisfied this variable
	s.savedPhase[varIdx] = value
}

func (s *CDCLSolver) literalIsTrue(lit cnf.Literal) bool {
	assign := s.assignments[lit.Var()]
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

	// Decay clause activity periodically
	if s.conflicts%100 == 0 {
		s.vsids.decay()
		// Also decay LBD bonus
		s.vsids.decayLBD()
		// Also decay clause activity
		for i := range s.clauseActivity {
			s.clauseActivity[i] *= 0.95
		}
	}
	
	// DISABLED: Inprocessing causes soundness bugs with watched literals
	// When clauses are removed during search, watch structures become stale
	// The fix requires careful handling of both original and learned clauses
	// 
	// Inprocessing: apply subsumption elimination every 500 conflicts
	// This removes redundant clauses during search to keep the formula small
	// if s.conflicts%500 == 0 && s.conflicts > 0 {
	// 	s.inprocessSubsumption()
	// 	// CRITICAL: Rebuild watched literals after clause removal
	// 	if s.verbose {
	// 		fmt.Printf("c [inprocess] Rebuilding watched literals after clause removal\n")
	// 	}
	// 	// Need to rebuild both original AND learned clause watches
	// 	// This is complex because learned clauses are stored separately
	// 	// For now, inprocessing is disabled to maintain soundness
	// }
}

// learnClause performs 1-UIP conflict analysis to learn a new clause
//
// 1-UIP (First Unique Implication Point) Algorithm:
// The goal is to find the earliest point in the implication graph where the
// conflict can be explained with exactly one literal at the current decision level.
//
// Algorithm:
// 1. Start with the conflict clause (all literals are false)
// 2. While there is more than one literal at current level:
//    - Pick the most recently decided literal at current level
//    - Resolve with its reason clause (the clause that forced it)
//    - This eliminates the literal and adds the reason's literals
// 3. The result is the 1-UIP learned clause with exactly one literal at current level
//
// Why 1-UIP?
// - Produces shorter, more general learned clauses than other schemes
// - The UIP literal is the "bottleneck" through which all paths to conflict pass
// - Backjumping to the second-highest level in the learned clause is sound
//
// Example:
// Decision: x=1, y=1, z=1 (level 3)
// Propagate: ¬x∨¬y∨a, a=0 (level 3)
// Propagate: ¬a∨¬z∨b, b=0 (level 3)
// Conflict: ¬b∨¬z (both false at level 3)
// 
// Resolution:
// Start: {b, z} (conflict clause)
// Resolve on b with reason (¬a∨¬z∨b): {z, ¬a, ¬z} = {¬a} (z cancels)
// Now only ¬a at level 3 - this is the 1-UIP!
// Learned clause: (a ∨ ¬z) - backjump to level of ¬z
//
// Backjump Level Calculation:
// The backjump level is the second-highest decision level in the learned clause.
// This is the highest level we can backjump to while still preventing the conflict.
// We backjump to this level and flip the decision there.
//
// LBD (Literal Block Distance):
// LBD = number of distinct decision levels in the learned clause.
// Lower LBD = better clause (involves fewer decision levels).
// Clauses with LBD=2 are "glue clauses" - most valuable, never delete.
func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) int {
	s.conflicts++
	
	if s.verbose && s.conflicts <= 100 {
		arenaCapMB := s.learnedArena.CapacityBytes() / 1024 / 1024
		fmt.Printf("c [debug] Conflict %d, iter %d, level %d, learned %d, trail %d, arenaCap %dMB\n", 
			s.conflicts, s.iterations, s.level, len(s.learnedClauses), len(s.trail), arenaCapMB)
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
	
	// Build list of variables to resolve on (those in clause at current level with reasons)
	for i := s.trailHead[s.level]; i < len(s.trail) && len(s.tmpCandidates) < 100; i++ {
		varIdx := uint32(s.trail[i])
		if s.tmpLiteralInClause[varIdx] {
			reasonIdx := s.implication[varIdx]
			if reasonIdx >= 0 {
				size := len(s.cnf.Clauses[reasonIdx].Literals)
				s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx, size})
			} else if reasonIdx < 0 {
				learnedIdx := -reasonIdx - 1
				if learnedIdx < len(s.learnedClauses) {
					size := len(s.learnedClauses[learnedIdx].Literals)
					s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx, size})
				}
			}
		}
	}
	
	// Simple selection sort for smallest reason clauses
	for i := 0; i < len(s.tmpCandidates) && i < 10; i++ {
		minIdx := i
		for j := i + 1; j < len(s.tmpCandidates); j++ {
			if s.tmpCandidates[j].size < s.tmpCandidates[minIdx].size {
				minIdx = j
			}
		}
		if minIdx != i {
			s.tmpCandidates[i], s.tmpCandidates[minIdx] = s.tmpCandidates[minIdx], s.tmpCandidates[i]
		}
	}
	
	// Resolve in order of preference (shortest reason clauses first)
	currentSize := 0
	for _, inClause := range s.tmpLiteralInClause {
		if inClause {
			currentSize++
		}
	}
	
	for i := 0; i < len(s.tmpCandidates) && s.tmpLevelCount[s.level] > 1; i++ {
		varIdx := s.tmpCandidates[i].varIdx
		
		reasonIdx := s.implication[varIdx]
		if reasonIdx < 0 {
			continue
		}
		
		if !s.tmpLiteralInClause[varIdx] {
			continue
		}
		
		var reasonLits []cnf.Literal
		if reasonIdx >= 0 {
			reasonLits = s.cnf.Clauses[reasonIdx].Literals
		} else {
			learnedIdx := -reasonIdx - 1
			reasonLits = s.learnedClauses[learnedIdx].Literals
		}
		
		if currentSize < 8 && s.tmpLevelCount[s.level] == 2 {
			if len(reasonLits) > 6 {
				continue
			}
		}
		
		s.tmpLiteralInClause[varIdx] = false
		s.tmpLevelCount[s.assignments[varIdx].Level]--
		
		newLiterals := 0
		for _, lit := range reasonLits {
			v := lit.Var()
			if v == varIdx {
				continue
			}
			if !s.tmpLiteralInClause[v] {
				s.tmpLiteralInClause[v] = true
				s.tmpLiteralIsNegated[v] = lit.IsNegated()
				lvl := s.assignments[v].Level
				if lvl <= s.level {
					s.tmpLevelCount[lvl]++
					newLiterals++
				}
			}
		}
		
		currentSize = currentSize - 1 + newLiterals
	}
	
	// Build the learned clause from remaining literals
	learnedLits := make([]cnf.Literal, 0)
	for varIdx, inClause := range s.tmpLiteralInClause {
		if inClause {
			learnedLits = append(learnedLits, cnf.NewLiteral(uint32(varIdx), s.tmpLiteralIsNegated[varIdx]))
		}
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
	
	if len(learnedLits) > 0 {
		// LBD FILTERING: Reject very low-quality clauses immediately
		if lbd > 50 {
			// Skip this clause - too many decision levels, unlikely to be useful
			if s.verbose && s.conflicts <= 100 {
				fmt.Printf("c [debug] Skipping learned clause: LBD=%d > 50\n", lbd)
			}
		} else {
			// Check if we need to delete clauses
			// Keep max 5000 normal clauses + all glue clauses
			maxNormalClauses := 5000
			normalCount := 0
			for _, lbdVal := range s.clauseLBD {
				if lbdVal > 3 {
					normalCount++
				}
			}
			
			if normalCount >= maxNormalClauses && lbd > 3 {
				// Delete oldest 50% of normal clauses (by age)
				if s.verbose {
					fmt.Printf("c [verbose] Deleting old normal clauses: %d normal clauses (limit %d)\n", normalCount, maxNormalClauses)
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
						keepClause[i] = (ageRank < normalCount/2)
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
				
				// Rebuild watches from scratch (O(n) but rare)
				// Clear old learned clause watches
				for i := range s.cnf.WatchListLong {
					s.cnf.WatchListLong[i] = make([]int, 0)
				}
				for i := range s.cnf.WatchList {
					s.cnf.WatchList[i] = make([]int, 0)
				}
				for i := range s.cnf.TernaryWatchList {
					s.cnf.TernaryWatchList[i] = make([]int, 0)
				}
				
				// Re-add all kept learned clauses with new indices
				for i, clause := range s.learnedClauses {
					s.cnf.AddLearnedClauseToWatches(i, clause.Literals)
				}
			}
			
			// Add the new learned clause
			learnedClauseIdx := len(s.learnedClauses)
			_ = s.learnedArena.AllocateClause(learnedLits, true)
			
			s.clauseActivity = append(s.clauseActivity, 0.0)
			s.clauseAge = append(s.clauseAge, s.currentAge)
			s.clauseSize = append(s.clauseSize, len(learnedLits))
			s.clauseLBD = append(s.clauseLBD, lbd)
			s.currentAge++
			
			newClause := cnf.Clause{Literals: learnedLits, Learned: true}
			s.learnedClauses = append(s.learnedClauses, newClause)
			
			// Add learned clause to watched literals scheme
			s.cnf.AddLearnedClauseToWatches(learnedClauseIdx, learnedLits)
			
			// LBD-based VSIDS: bump variables in low-LBD clauses
			s.vsids.bumpLBD(learnedLits, lbd)
		}
	}
	
	// Calculate backjump level
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
	
	s.lastConflictLBD = lbd
	s.lbdSum += lbd
	s.lbdCount++
	
	return backjumpLevel
}

// deleteLearnedClauses removes low-quality learned clauses to control memory usage
//
// Clause Database Management Strategy:
// Learned clauses can grow unbounded, causing memory explosion and slowing down
// propagation. We use a quality-based deletion scheme that considers:
//
// 1. LBD (Literal Block Distance): PRIMARY QUALITY METRIC
//    - LBD = number of distinct decision levels in the clause
//    - Lower LBD = better clause (spans fewer decision levels)
//    - LBD=2: "Glue clauses" - most valuable, connect decision levels
//    - LBD=3: Very good clauses
//    - LBD>5: Usually not useful long-term
//
// 2. Age: SECONDARY FACTOR
//    - Old clauses may become irrelevant as search progresses
//    - Even good LBD clauses can become stale after hundreds of conflicts
//    - Force deletion of clauses older than 500 conflicts
//
// 3. Size: TERTIARY FACTOR
//    - Large clauses (>15 literals) are rarely useful
//    - Small clauses are more general and propagate more often
//    - Force deletion of clauses larger than 15 literals
//
// 4. Activity: PROTECTION FACTOR
//    - Clauses involved in recent conflicts are more relevant
//    - Activity decays over time (like VSIDS)
//    - High activity provides some protection against deletion
//
// Protection Rules (clauses never/ rarely deleted):
// - LBD=2 AND size≤4 AND age<100: Core glue, NEVER delete (score=-1000)
// - LBD=3 AND size≤3 AND age<50: Very good, protect unless very old (score=-500)
//
// Deletion Trigger:
// When learned clause count exceeds maxLearned (default 10,000), delete down to
// minLearned (default 5,000) - aggressive 50% reduction.
//
// Scoring Formula:
// score = age*10 + LBD*50 + size*5 - activity*20 + bonuses/penalties
// Higher score = more likely to delete
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
		
		// PROTECTION: Only protect truly exceptional clauses
		// LBD == 2 AND size <= 4 AND age < 100: core glue, never delete
		if lbd == 2 && size <= 4 && age < 100 {
			score = -1000.0 // Absolutely never delete
		}
		
		// LBD == 3 AND size <= 3 AND age < 50: very good, protect unless very old
		if lbd == 3 && size <= 3 && age < 50 {
			score = -500.0 // Protect unless very old
		}
		
		// FORCE DELETION: Very old clauses (age > 500) regardless of LBD
		if age > 500 {
			score += 1000.0 // Force deletion of very old clauses
		}
		
		// FORCE DELETION: Large clauses (size > 15) regardless of LBD
		if size > 15 {
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
	
	// Rebuild arena from compacted slices (ensures contiguous memory)
	s.learnedArena.Reset()
	for i, clause := range s.learnedClauses {
		s.learnedArena.AllocateClause(clause.Literals, true)
		_ = i // Use index variable
	}
	
	// CRITICAL: Rebuild watches after deleting learned clauses
	// The learned clause indices have changed (compacted), so all watch structures
	// referencing learned clauses are now stale and must be rebuilt
	s.cnf.InitializeWatches()
	// Re-add all learned clauses to watches with correct indices
	for i, clause := range s.learnedClauses {
		s.cnf.AddLearnedClauseToWatches(i, clause.Literals)
	}
	
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
//
// Backjumping vs Chronological Backtracking:
// Traditional DPLL backtracks one level at a time (chronological).
// CDCL solvers use backjumping (non-chronological backtracking) to skip
// irrelevant decision levels.
//
// How Backjumping Works:
// 1. After 1-UIP conflict analysis, the learned clause has exactly one literal
//    at the current decision level (the UIP - Unique Implication Point)
// 2. The backjump level is the second-highest level in the learned clause
// 3. Instead of backtracking to level-1, we jump directly to backjumpLevel
// 4. At backjumpLevel, we flip the decision that led to the conflict
//
// Why Backjumping is Sound:
// The learned clause explains why the conflict occurred. All literals in the
// learned clause except the UIP are already false at levels < current.
// By backjumping to the second-highest level and flipping that decision,
// we ensure the learned clause becomes unit and propagates the UIP literal
// to false, preventing the same conflict.
//
// Example:
// Decisions: x=1 (level 1), y=1 (level 2), z=1 (level 3)
// Conflict at level 3
// Learned clause: (¬x ∨ ¬y ∨ ¬z) with LBD=3 (levels 1,2,3)
// Backjump level = 2 (second-highest in learned clause)
// After backjump: trail = [x=1, y=0], z is unassigned
// The learned clause is now unit: ¬z is forced at level 2
//
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
		return false
	}
	
	// Find the decision point at the backjump level
	decisionPoint := s.trailHead[bjLevel]
	if decisionPoint >= len(s.trail) {
		return false
	}
	
	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value

	// Clear all assignments from decisionPoint onwards
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
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
	
	return true
}


