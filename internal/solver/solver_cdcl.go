package solver

import (
	"fmt"
	"satience/internal/cnf"
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
	learnedClauses []cnf.Clause
	clauseActivity []float64
	clauseAge    []int
	currentAge   int
	verbose      bool
	decisions    int
	backjumpLevel int
	maxLearned   int
	savedPhase   []bool
	restartBase  int
	restartCount int
	lubyIndex    int
	// Adaptive restart fields (Glucose-style)
	lbdSum        int    // Sum of LBDs for recent conflicts
	lbdCount      int    // Number of conflicts tracked
	lastConflictLBD int  // LBD of last learned clause
	// Inprocessing fields
	inprocInterval int    // Run inprocessing every N conflicts
	inprocCount    int    // Number of times inprocessing ran
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	maxLearned := 10000 // Initial limit on learned clauses
	restartBase := 100  // Base restart interval (in conflicts)
	return &CDCLSolver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		trailHead:   make([]int, 1),
		level:       0,
		vsids:       NewVSIDS(formula.NumVars),
		conflicts:   0,
		implication: make([]int, formula.NumVars),
		iterations:  0,
		maxIter:     0, // disabled by default
		learnedClauses: make([]cnf.Clause, 0),
		clauseActivity: make([]float64, 0),
		clauseAge:    make([]int, 0),
		currentAge:   0,
		verbose:     false,
		decisions:   0,
		backjumpLevel: 0,
		maxLearned:   maxLearned,
		savedPhase:  make([]bool, formula.NumVars),
		restartBase:  restartBase,
		restartCount: 0,
		lubyIndex:    0,
		lbdSum:       0,
		lbdCount:     0,
		lastConflictLBD: 0,
		inprocInterval: 1000, // Run inprocessing every 1000 conflicts
		inprocCount:    0,
	}
}

// SetMaxIter sets the maximum iteration limit (0 = unlimited)
func (s *CDCLSolver) SetMaxIter(limit int) {
	s.maxIter = limit
}

// SetVerbose enables/disables verbose output
func (s *CDCLSolver) SetVerbose(v bool) {
	s.verbose = v
}

// GetStats returns solving statistics
func (s *CDCLSolver) GetStats() map[string]int {
	return map[string]int{
		"conflicts":     s.conflicts,
		"decisions":     s.decisions,
		"iterations":    s.iterations,
		"learned":       len(s.learnedClauses),
		"level":         s.level,
		"inprocessing":  s.inprocCount,
	}
}

func (s *CDCLSolver) printStats() {
	fmt.Printf("c \n")
	fmt.Printf("c === Solving Statistics ===\n")
	fmt.Printf("c Variables:     %d\n", s.cnf.NumVars)
	fmt.Printf("c Clauses:       %d\n", s.cnf.NumClauses)
	fmt.Printf("c Conflicts:     %d\n", s.conflicts)
	fmt.Printf("c Decisions:     %d\n", s.decisions)
	fmt.Printf("c Iterations:    %d\n", s.iterations)
	fmt.Printf("c Learned:       %d\n", len(s.learnedClauses))
	fmt.Printf("c Max Level:     %d\n", s.level)
	fmt.Printf("c Inprocessing:  %d\n", s.inprocCount)
	fmt.Printf("c \n")
}

// preprocess applies preprocessing techniques to simplify the formula
// Returns SAT if formula is trivially satisfiable, UNSAT if unsatisfiable, UNKNOWN otherwise
func (s *CDCLSolver) preprocess() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}
	
	// Apply unit propagation preprocessing
	unitResult := s.unitPropagationPreprocess()
	if unitResult != UNKNOWN {
		return unitResult
	}
	
	// Apply pure literal elimination
	pureResult := s.pureLiteralElimination()
	if pureResult != UNKNOWN {
		return pureResult
	}
	
	// Apply subsumption elimination
	s.subsumptionElimination()
	
	// Apply variable elimination
	veResult := s.variableElimination()
	if veResult != UNKNOWN {
		return veResult
	}
	
	// Apply blocked clause elimination
	bceResult := s.blockedClauseElimination()
	if bceResult != UNKNOWN {
		return bceResult
	}
	
	// Rebuild binary and ternary clause indices after preprocessing
	// Preprocessing modifies clauses, so the cached indices are stale
	s.cnf.RebuildShortClauses()
	
	// Initialize watched literals for binary clauses after preprocessing
	s.cnf.InitializeWatches()
	
	if s.verbose {
		fmt.Printf("c [verbose] After preprocessing: %d variables, %d clauses (%d binary, %d ternary)\n", 
			s.cnf.NumVars, s.cnf.NumClauses, len(s.cnf.BinaryClauses), len(s.cnf.TernaryClauses))
	}
	
	return UNKNOWN
}

// unitPropagationPreprocess performs unit propagation before search
// Assigns all unit clauses and simplifies the formula
func (s *CDCLSolver) unitPropagationPreprocess() SolveResult {
	changed := true
	for changed {
		changed = false
		
		// Find unit clauses in original formula
		clauseCount := len(s.cnf.Clauses)
		for clauseIdx := 0; clauseIdx < clauseCount; clauseIdx++ {
			clause := s.cnf.Clauses[clauseIdx]
			
			// Skip if clause already satisfied
			satisfied := false
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal
			
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level != 0 {
					// Already assigned
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
				// Conflict found during preprocessing
				if s.verbose {
					fmt.Printf("c [verbose] Preprocessing: conflict in unit propagation\n")
				}
				return UNSAT
			}
			
			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				// Unit clause - propagate (direct assignment without trail)
				varIdx := unassignedLit.Var()
				value := !unassignedLit.IsNegated()
				s.assignments[varIdx] = Assignment{
					Value: value,
					Level: 1,
				}
				changed = true
				
				// Simplify clauses by removing satisfied clauses and false literals
				conflict := s.simplifyAfterAssignment(varIdx, value)
				if conflict {
					if s.verbose {
						fmt.Printf("c [verbose] Preprocessing: empty clause created\n")
					}
					return UNSAT
				}
				
				// Update clause count since simplifyAfterAssignment modifies it
				clauseCount = len(s.cnf.Clauses)
				if clauseIdx >= clauseCount {
					clauseIdx = clauseCount - 1
				}
			}
		}
	}
	
	return UNKNOWN
}

// simplifyAfterAssignment removes satisfied clauses and false literals after an assignment
// Returns true if an empty clause was created (conflict)
func (s *CDCLSolver) simplifyAfterAssignment(varIdx uint32, value bool) bool {
	// Remove satisfied clauses and false literals from original clauses
	newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))
	for _, clause := range s.cnf.Clauses {
		// Check if clause is satisfied or contains false literals
		satisfied := false
		newLiterals := make([]cnf.Literal, 0, len(clause.Literals))
		
		for _, lit := range clause.Literals {
			if lit.Var() == varIdx {
				// This literal involves the assigned variable
				litValue := !lit.IsNegated()
				if litValue == value {
					// Literal is true, clause is satisfied
					satisfied = true
					break
				}
				// Literal is false, skip it
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}
		
		if satisfied {
			// Clause is satisfied, remove it
			continue
		}
		
		if len(newLiterals) == 0 {
			// Empty clause created - conflict!
			return true
		}
		
		newClauses = append(newClauses, cnf.Clause{Literals: newLiterals, Learned: false})
	}
	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)
	return false
}

// pureLiteralElimination finds and assigns pure literals
// A literal is pure if it appears with only one polarity in all clauses
func (s *CDCLSolver) pureLiteralElimination() SolveResult {
	changed := true
	for changed {
		changed = false
		
		// Track which variables appear as positive/negative
		hasPositive := make([]bool, s.cnf.NumVars)
		hasNegative := make([]bool, s.cnf.NumVars)
		
		// Scan all clauses
		for _, clause := range s.cnf.Clauses {
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level != 0 {
					continue // Already assigned
				}
				if lit.IsNegated() {
					hasNegative[varIdx] = true
				} else {
					hasPositive[varIdx] = true
				}
			}
		}
		
		// Find pure literals and assign them
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level != 0 {
				continue // Already assigned
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
				// Assign the pure literal (direct assignment without trail)
				s.assignments[varIdx] = Assignment{
					Value: pureValue,
					Level: 1,
				}
				changed = true
				
				// Simplify
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
	
	// Check if all clauses are satisfied (formula is empty = SAT)
	if len(s.cnf.Clauses) == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Pure literal elimination: all clauses satisfied\n")
		}
		return SAT
	}
	
	return UNKNOWN
}

// subsumptionElimination removes clauses that are subsumed by other clauses
// A clause C1 subsumes clause C2 if all literals in C1 are also in C2
// Example: clause [1, 2] subsumes clause [1, 2, 3] - we can remove [1, 2, 3]
// This is safe because if C1 is satisfied, C2 is automatically satisfied
func (s *CDCLSolver) subsumptionElimination() {
	if s.verbose {
		fmt.Printf("c [verbose] Subsumption elimination: checking %d clauses\n", len(s.cnf.Clauses))
	}
	
	removedCount := 0
	changed := true
	
	for changed {
		changed = false
		remainingClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))
		
		for i, clause := range s.cnf.Clauses {
			isSubsumed := false
			
			// Check if this clause is subsumed by any other clause
			for j, otherClause := range s.cnf.Clauses {
				if i == j {
					continue
				}
				
				// Skip if other clause is longer or equal length (can't subsume)
				if len(otherClause.Literals) >= len(clause.Literals) {
					continue
				}
				
				// Check if all literals in otherClause are in clause
				if s.isSubsumedBy(clause, otherClause) {
					isSubsumed = true
					if s.verbose {
						fmt.Printf("c [verbose] Clause %v subsumed by %v\n", clause.Literals, otherClause.Literals)
					}
					break
				}
			}
			
			if !isSubsumed {
				remainingClauses = append(remainingClauses, clause)
			} else {
				removedCount++
				changed = true
			}
		}
		
		s.cnf.Clauses = remainingClauses
		s.cnf.NumClauses = len(remainingClauses)
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Subsumption elimination: removed %d clauses\n", removedCount)
	}
}

// isSubsumedBy checks if clause is subsumed by other clause
// Returns true if all literals in 'other' appear in 'clause'
func (s *CDCLSolver) isSubsumedBy(clause, other cnf.Clause) bool {
	// Build a set of literals in the clause
	literalSet := make(map[cnf.Literal]bool, len(clause.Literals))
	for _, lit := range clause.Literals {
		literalSet[lit] = true
	}
	
	// Check if all literals in 'other' are in the set
	for _, lit := range other.Literals {
		if !literalSet[lit] {
			return false
		}
	}
	
	return true
}

// variableElimination eliminates variables via resolution when it reduces formula size
// For each variable x, compute all resolvents of clauses containing x and ¬x
// If the resolvents are fewer than the original clauses, replace them
// This is a powerful preprocessing technique that can dramatically reduce formula size
// Returns UNSAT if empty clause is created, SAT if all clauses satisfied, UNKNOWN otherwise
func (s *CDCLSolver) variableElimination() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Variable elimination: checking %d variables\n", s.cnf.NumVars)
	}
	
	eliminatedCount := 0
	resolventCount := 0
	
	changed := true
	for changed {
		changed = false
		
		// Track which variables are eliminated in this pass
		eliminated := make([]bool, s.cnf.NumVars)
		
		// Try to eliminate each variable
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if eliminated[varIdx] {
				continue
			}
			
			// Find clauses containing x (positive) and ¬x (negative)
			posClauses := make([]int, 0)
			negClauses := make([]int, 0)
			
			for i, clause := range s.cnf.Clauses {
				// Check if clause contains x or ¬x
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
			
			// Skip if variable doesn't appear in both polarities (already pure/eliminated)
			if len(posClauses) == 0 || len(negClauses) == 0 {
				continue
			}
			
			// Compute all resolvents
			resolvents := make([]cnf.Clause, 0)
			resolventSet := make(map[string]bool)
			
			for _, posIdx := range posClauses {
				posClause := s.cnf.Clauses[posIdx]
				
				for _, negIdx := range negClauses {
					negClause := s.cnf.Clauses[negIdx]
					
					// Resolve posClause and negClause on varIdx
					resolvent := s.resolve(posClause, negClause, varIdx)
					if resolvent != nil {
						// Check for tautology (contains both x and ¬x for some x)
						if !s.isTautology(resolvent) {
							// Use a canonical representation to avoid duplicates
							key := s.clauseKey(resolvent)
							if !resolventSet[key] {
								resolventSet[key] = true
								resolvents = append(resolvents, *resolvent)
							}
						}
					}
				}
			}
			
			// Check if elimination is beneficial
			// Only eliminate if resolvents are fewer than original clauses
			originalCount := len(posClauses) + len(negClauses)
			if len(resolvents) < originalCount {
				// Variable elimination is beneficial
				if s.verbose {
					fmt.Printf("c [verbose] Eliminating var %d: %d clauses -> %d resolvents\n", 
						varIdx, originalCount, len(resolvents))
				}
				
				// Check if any resolvent is empty (UNSAT!)
				for _, resolvent := range resolvents {
					if len(resolvent.Literals) == 0 {
						if s.verbose {
							fmt.Printf("c [verbose] Variable elimination: empty clause created (UNSAT)\n")
						}
						return UNSAT
					}
				}
				
				// Remove original clauses containing x or ¬x
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
				
				// Add resolvents
				for _, resolvent := range resolvents {
					keepClauses = append(keepClauses, resolvent)
				}
				
				s.cnf.Clauses = keepClauses
				s.cnf.NumClauses = len(keepClauses)
				
				eliminated[varIdx] = true
				eliminatedCount++
				resolventCount += len(resolvents)
				changed = true
			}
		}
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Variable elimination: eliminated %d variables, added %d resolvents\n", 
			eliminatedCount, resolventCount)
	}
	
	// Check if all clauses are satisfied (formula is empty = SAT)
	if len(s.cnf.Clauses) == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Variable elimination: all clauses satisfied\n")
		}
		return SAT
	}
	
	return UNKNOWN
}

// resolve computes the resolvent of two clauses on a given variable
// Returns nil if resolution is not possible or produces a tautology
func (s *CDCLSolver) resolve(clause1, clause2 cnf.Clause, varIdx uint32) *cnf.Clause {
	// clause1 must contain x (positive), clause2 must contain ¬x (negative)
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
	
	// Build resolvent: all literals except x and ¬x
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
		// Empty clause - this means the original formula is UNSAT
		return &cnf.Clause{Literals: make([]cnf.Literal, 0), Learned: false}
	}
	
	return &cnf.Clause{Literals: resolventLits, Learned: false}
}

// isTautology checks if a clause contains both x and ¬x for some variable x
func (s *CDCLSolver) isTautology(clause *cnf.Clause) bool {
	seen := make(map[uint32]bool)
	
	for _, lit := range clause.Literals {
		varIdx := lit.Var()
		isNeg := lit.IsNegated()
		
		if prevNeg, exists := seen[varIdx]; exists {
			// Variable already seen - check if opposite polarity
			if prevNeg != isNeg {
				return true // Tautology!
			}
		} else {
			seen[varIdx] = isNeg
		}
	}
	
	return false
}

// clauseKey returns a canonical string representation of a clause for deduplication
func (s *CDCLSolver) clauseKey(clause *cnf.Clause) string {
	// Sort literals for canonical representation
	lits := make([]uint64, len(clause.Literals))
	for i, lit := range clause.Literals {
		lits[i] = uint64(lit)
	}
	
	// Simple bubble sort (clauses are typically small)
	for i := 0; i < len(lits)-1; i++ {
		for j := i + 1; j < len(lits); j++ {
			if lits[i] > lits[j] {
				lits[i], lits[j] = lits[j], lits[i]
			}
		}
	}
	
	// Build string key
	key := ""
	for _, lit := range lits {
		key += fmt.Sprintf("%d,", lit)
	}
	return key
}

// blockedClauseElimination removes clauses that are "blocked" by a literal
// A clause C is blocked by literal L ∈ C if all resolvents of C with clauses
// containing ¬L are tautologies. Blocked clauses can be safely removed.
// This is a powerful preprocessing technique that can reduce formula size.
// Returns UNSAT if empty clause detected, SAT if all clauses satisfied, UNKNOWN otherwise
func (s *CDCLSolver) blockedClauseElimination() SolveResult {
	// Skip BCE on large formulas to avoid excessive preprocessing time
	// O(n²) complexity makes it expensive for large instances
	if s.cnf.NumClauses > 5000 {
		if s.verbose {
			fmt.Printf("c [verbose] Blocked clause elimination: skipped (%d clauses, limit 5000)\n", s.cnf.NumClauses)
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

		// Check each clause for blocking
		for clauseIdx, clause := range s.cnf.Clauses {
			if len(clause.Literals) == 0 {
				// Empty clause - UNSAT
				return UNSAT
			}

			// Check if clause is blocked by any of its literals
			for _, blockingLit := range clause.Literals {
				if s.isClauseBlockedBy(clause, blockingLit) {
					blocked[clauseIdx] = true
					removedCount++
					changed = true
					break
				}
			}
		}

		// Remove blocked clauses
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

// isClauseBlockedBy checks if a clause is blocked by a specific literal
// A clause C is blocked by L ∈ C if all resolvents with clauses containing ¬L are tautologies
func (s *CDCLSolver) isClauseBlockedBy(clause cnf.Clause, blockingLit cnf.Literal) bool {
	opposite := blockingLit.Negate()

	// Find all clauses containing the opposite literal
	for _, other := range s.cnf.Clauses {
		// Check if 'other' contains the opposite literal
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

		// Compute resolvent on blockingLit's variable
		resolvent := s.resolveOnVar(clause, other, blockingLit.Var())

		// If resolvent is not a tautology, clause is not blocked by this literal
		if resolvent != nil && !s.isTautology(resolvent) {
			return false
		}
	}

	// All resolvents are tautologies (or no resolvents exist)
	return true
}

// resolveOnVar computes the resolvent of two clauses on a specific variable
// Returns nil if resolution is not possible (variable not in both clauses with opposite polarity)
// Returns empty clause if resolvent is empty (conflict)
func (s *CDCLSolver) resolveOnVar(clause1, clause2 cnf.Clause, varIdx uint32) *cnf.Clause {
	// Check if clause1 has positive literal and clause2 has negative (or vice versa)
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

	// Try opposite polarity
	if !hasPosX || !hasNegX {
		hasPosX = false
		hasNegX = false
		for _, lit := range clause1.Literals {
			if lit.Var() == varIdx && lit.IsNegated() {
				hasNegX = true
				break
			}
		}
		for _, lit := range clause2.Literals {
			if lit.Var() == varIdx && !lit.IsNegated() {
				hasPosX = true
				break
			}
		}
	}

	if !hasPosX || !hasNegX {
		return nil
	}

	// Build resolvent: all literals except x and ¬x
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
		// Empty clause
		return &cnf.Clause{Literals: make([]cnf.Literal, 0), Learned: false}
	}

	return &cnf.Clause{Literals: resolventLits, Learned: false}
}

func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

func (s *CDCLSolver) SolveWithResult() SolveResult {
	// Preprocessing: simplify formula before solving
	preprocessingResult := s.preprocess()
	if preprocessingResult != UNKNOWN {
		return preprocessingResult
	}
	
	for {
		s.iterations++
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
			if !s.backtrack() {
				if s.verbose {
					s.printStats()
				}
				return UNSAT
			}
			// Reset backjump level for next conflict
			s.backjumpLevel = 0
			
			// Check if we should restart (after backtracking)
			if s.shouldRestart() {
				s.restart()
			}
			
			// Check if we should run inprocessing
			if s.conflicts > 0 && s.conflicts%s.inprocInterval == 0 {
				s.inprocessing()
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

func (s *CDCLSolver) propagate() (bool, int) {
	trailIndex := s.trailHead[s.level]

	// For initial unit propagation at level 0, we need to check all clauses even with empty trail
	// Use a do-while pattern: always check at least once per level
	firstPass := true
	for firstPass || trailIndex < len(s.trail) {
		firstPass = false
		unitPropagated := false
		
		// Process trail using watched literals for binary clauses
		for trailIndex < len(s.trail) {
			litIdx := s.trail[trailIndex]
			trailIndex++
			
			varIdx := uint32(litIdx)
			assign := s.assignments[varIdx]
			
			// Get the FALSE literal (the one that just became false)
			// When x=false, we check clauses watching x
			// When x=true, we check clauses watching ¬x
			falseLit := cnf.NewLiteral(varIdx, !assign.Value)
			watchIdx := cnf.LitToIndex(falseLit)
			
			if watchIdx >= len(s.cnf.WatchList) {
				continue
			}
			
			// Get watch list for this literal
			wlist := s.cnf.WatchList[watchIdx]
			
			// Process all clauses watching this literal
			for i := 0; i < len(wlist); {
				binIdx := wlist[i]
				
				if binIdx >= len(s.cnf.BinaryClauses) {
					// Invalid index, skip
					i++
					continue
				}
				
				binClause := s.cnf.BinaryClauses[binIdx]
				lit1 := cnf.Literal(binClause.Lit1)
				lit2 := cnf.Literal(binClause.Lit2)
				
				// Get current watch indices for this clause
				watchA := s.cnf.BinaryWatchA[binIdx]
				watchB := s.cnf.BinaryWatchB[binIdx]
				
				// Determine which watch is the false one we're processing
				otherWatch := -1
				if watchA == watchIdx {
					otherWatch = watchB
				} else if watchB == watchIdx {
					otherWatch = watchA
				} else {
					// This clause doesn't watch this literal anymore (stale watch list entry)
					// Remove from watch list
					wlist = append(wlist[:i], wlist[i+1:]...)
					s.cnf.WatchList[watchIdx] = wlist
					continue
				}
				
				// Check the other watched literal
				otherLit := cnf.IndexToLit(otherWatch)
				otherAssign := s.assignments[otherLit.Var()]
				
				if otherAssign.Level != 0 {
					// Other watch is also assigned
					otherFalse := (!otherLit.IsNegated() && !otherAssign.Value) || (otherLit.IsNegated() && otherAssign.Value)
					if otherFalse {
						// Both watches are false = CONFLICT
						for clauseIdx, clause := range s.cnf.Clauses {
							if len(clause.Literals) == 2 && 
							   clause.Literals[0] == lit1 && 
							   clause.Literals[1] == lit2 {
								return true, clauseIdx
							}
						}
						return true, 0
					}
					// Other watch is true, clause is satisfied
					i++
					continue
				}
				
				// Other watch is unassigned - try to find a new watch
				// Check lit1 first
				newWatch := -1
				lit1Assign := s.assignments[lit1.Var()]
				if lit1Assign.Level == 0 {
					newWatch = cnf.LitToIndex(lit1)
				} else {
					lit1False := (!lit1.IsNegated() && !lit1Assign.Value) || (lit1.IsNegated() && lit1Assign.Value)
					if !lit1False {
						// lit1 is true, clause is satisfied
						i++
						continue
					}
				}
				
				// Check lit2
				if newWatch == -1 {
					lit2Assign := s.assignments[lit2.Var()]
					if lit2Assign.Level == 0 {
						newWatch = cnf.LitToIndex(lit2)
					} else {
						lit2False := (!lit2.IsNegated() && !lit2Assign.Value) || (lit2.IsNegated() && lit2Assign.Value)
						if !lit2False {
							// lit2 is true, clause is satisfied
							i++
							continue
						}
					}
				}
				
				if newWatch != -1 {
					// Found a new literal to watch
					// Update the watch that was false
					if watchA == watchIdx {
						s.cnf.BinaryWatchA[binIdx] = newWatch
					} else {
						s.cnf.BinaryWatchB[binIdx] = newWatch
					}
					
					// Add clause to new watch list
					if newWatch < len(s.cnf.WatchList) {
						s.cnf.WatchList[newWatch] = append(s.cnf.WatchList[newWatch], binIdx)
					}
					
					// Remove from old watch list
					wlist = append(wlist[:i], wlist[i+1:]...)
					s.cnf.WatchList[watchIdx] = wlist
					// Don't increment i, process next clause at same index
				} else {
					// Can't find new watch - other watch must be propagated
					assignLevel := s.level
					if assignLevel == 0 {
						assignLevel = 1
					}
					s.assignLiteral(otherLit, assignLevel, -binIdx-1)
					unitPropagated = true
					break
				}
			}
			
			if unitPropagated {
				break
			}
		}
		
		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		// OPTIMIZATION 2: Propagate ternary clauses (still faster than general case)
		for ternIdx, ternClause := range s.cnf.TernaryClauses {
			lit1 := cnf.Literal(ternClause.Lit1)
			lit2 := cnf.Literal(ternClause.Lit2)
			lit3 := cnf.Literal(ternClause.Lit3)
			
			var1 := lit1.Var()
			var2 := lit2.Var()
			var3 := lit3.Var()
			assign1 := s.assignments[var1]
			assign2 := s.assignments[var2]
			assign3 := s.assignments[var3]
			
			// Count false and unassigned
			falseCount := 0
			unassignedCount := 3
			var unassignedLit cnf.Literal
			
			if assign1.Level != 0 {
				unassignedCount--
				if (!lit1.IsNegated() && !assign1.Value) || (lit1.IsNegated() && assign1.Value) {
					falseCount++
				}
			}
			if assign2.Level != 0 {
				unassignedCount--
				if (!lit2.IsNegated() && !assign2.Value) || (lit2.IsNegated() && assign2.Value) {
					falseCount++
				}
			}
			if assign3.Level != 0 {
				unassignedCount--
				if (!lit3.IsNegated() && !assign3.Value) || (lit3.IsNegated() && assign3.Value) {
					falseCount++
				}
			} else {
				unassignedLit = lit3
			}
			
			if falseCount == 0 {
				continue // Not yet unit or conflicting
			}
			
			if unassignedCount == 0 && falseCount > 0 {
				// Conflict - all three false
				for clauseIdx, clause := range s.cnf.Clauses {
					if len(clause.Literals) == 3 && 
					   clause.Literals[0] == lit1 && 
					   clause.Literals[1] == lit2 &&
					   clause.Literals[2] == lit3 {
						return true, clauseIdx
					}
				}
				return true, 0
			}
			
			if unassignedCount == 1 && falseCount == 2 {
				// Unit - propagate the unassigned literal
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				s.assignLiteral(unassignedLit, assignLevel, -ternIdx-1)
				unitPropagated = true
				break
			}
		}
		
		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		// FALLBACK: Check long clauses (>3 literals) with general algorithm
		for clauseIdx := range s.cnf.Clauses {
			clause := &s.cnf.Clauses[clauseIdx]
			
			// Skip binary and ternary clauses (already handled)
			if len(clause.Literals) <= 3 {
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
		
		// Check learned clauses for conflicts and unit propagation
		for learnedIdx := range s.learnedClauses {
			clause := &s.learnedClauses[learnedIdx]
			
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
				return true, -learnedIdx - 1 // negative to distinguish from original clauses
			}
			
			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				// Unit clause - propagate the unassigned literal
				s.assignLiteral(unassignedLit, s.level, -learnedIdx-1)
				unitPropagated = true
				break
			}
		}
		
		// If we propagated a unit from learned clause, restart from beginning
		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		trailIndex++
	}

	return false, -1
}

func (s *CDCLSolver) decide() bool {
	if !s.vsids.hasUnassigned(s.assignments, s.cnf.NumVars) {
		return false
	}

	varIdx, phase := s.vsids.selectVariableWithPhase(s.assignments, s.savedPhase)

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
	}
	
	s.vsids.bumpClause(conflictLits)
	
	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	if s.conflicts%100 == 0 {
		s.vsids.decay()
	}
}

func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) int {
	// 1-UIP clause learning
	// Start with the conflicting clause and resolve with reason clauses
	// until we have exactly one literal at the current decision level
	
	// Track which literals are in the learned clause
	literalInClause := make([]bool, s.cnf.NumVars)
	literalIsNegated := make([]bool, s.cnf.NumVars)
	
	// Count literals at each level
	levelCount := make([]int, s.level+1)
	
	// Add all literals from the conflicting clause
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !literalInClause[varIdx] {
			literalInClause[varIdx] = true
			literalIsNegated[varIdx] = lit.IsNegated()
			lvl := s.assignments[varIdx].Level
			if lvl <= s.level {
				levelCount[lvl]++
			}
		}
	}
	
	// Resolve with reason clauses for literals at current level
	// Work backwards through the trail at current level
	for i := len(s.trail) - 1; i >= s.trailHead[s.level] && levelCount[s.level] > 1; i-- {
		varIdx := uint32(s.trail[i])
		
		if !literalInClause[varIdx] {
			continue // This variable is not in our learned clause
		}
		
		// Get the reason clause for this literal
		reasonIdx := s.implication[varIdx]
		if reasonIdx < 0 {
			continue // Decision variable, no reason clause
		}
		
		// Get the reason clause literals
		var reasonLits []cnf.Literal
		if reasonIdx >= 0 {
			reasonLits = s.cnf.Clauses[reasonIdx].Literals
		} else {
			learnedIdx := -reasonIdx - 1
			reasonLits = s.learnedClauses[learnedIdx].Literals
		}
		
		// Remove this literal from the learned clause (resolution)
		literalInClause[varIdx] = false
		levelCount[s.assignments[varIdx].Level]--
		
		// Add all other literals from the reason clause
		for _, lit := range reasonLits {
			v := lit.Var()
			if v == varIdx {
				continue // Skip the literal we're resolving on
			}
			if !literalInClause[v] {
				literalInClause[v] = true
				literalIsNegated[v] = lit.IsNegated()
				lvl := s.assignments[v].Level
				if lvl <= s.level {
					levelCount[lvl]++
				}
			}
		}
	}
	
	// Build the learned clause from remaining literals
	learnedLits := make([]cnf.Literal, 0)
	for varIdx, inClause := range literalInClause {
		if inClause {
			learnedLits = append(learnedLits, cnf.NewLiteral(uint32(varIdx), literalIsNegated[varIdx]))
		}
	}
	
	// Minimize learned clause via self-subsumption
	learnedLits = s.minimizeLearnedClause(learnedLits)
	
	// Only learn non-empty clauses
	if len(learnedLits) > 0 {
		// Check if we need to delete clauses
		if len(s.learnedClauses) >= s.maxLearned {
			s.deleteLearnedClauses()
		}
		
		s.learnedClauses = append(s.learnedClauses, cnf.Clause{
			Literals: learnedLits,
			Learned:  true,
		})
		s.clauseActivity = append(s.clauseActivity, 0.0) // Initial activity
		s.clauseAge = append(s.clauseAge, s.currentAge)
		s.currentAge++
	}
	
	// Calculate backjump level: second-highest level in learned clause
	// The 1-UIP clause has exactly one literal at current level
	// Backjump to the highest level among the other literals
	backjumpLevel := 0
	for varIdx, inClause := range literalInClause {
		if inClause {
			lvl := s.assignments[varIdx].Level
			if lvl > backjumpLevel && lvl < s.level {
				backjumpLevel = lvl
			}
		}
	}
	
	// If no other level found, backjump to level 0 (but we'll use level 1 minimum)
	if backjumpLevel == 0 {
		backjumpLevel = 1
	}
	
	// Calculate LBD (Literal Block Distance) for adaptive restarts
	// LBD = number of distinct decision levels in the learned clause
	lbd := s.calculateLBD(learnedLits)
	
	// Update LBD statistics for adaptive restarts
	s.lastConflictLBD = lbd
	s.lbdSum += lbd
	s.lbdCount++
	
	return backjumpLevel
}

// calculateLBD calculates the Literal Block Distance (LBD) of a clause
// LBD = number of distinct decision levels among the literals in the clause
// Lower LBD = better clause (fewer decision levels involved)
// Clauses with LBD=2 are called "glue clauses" and are very valuable
func (s *CDCLSolver) calculateLBD(literals []cnf.Literal) int {
	levelSeen := make(map[int]bool)
	for _, lit := range literals {
		varIdx := lit.Var()
		lvl := s.assignments[varIdx].Level
		levelSeen[lvl] = true
	}
	return len(levelSeen)
}

// minimizeLearnedClause reduces the size of a learned clause via self-subsumption
// A literal can be removed if there exists a learned clause that subsumes it
// Example: if we learned (a \/ b \/ c) and we already have (a \/ b), then c can be removed
func (s *CDCLSolver) minimizeLearnedClause(learnedLits []cnf.Literal) []cnf.Literal {
	if len(learnedLits) <= 2 {
		return learnedLits // No point minimizing small clauses
	}
	
	// Mark which literals are in the learned clause
	literalInLearned := make([]bool, s.cnf.NumVars)
	literalIsNegated := make([]bool, s.cnf.NumVars)
	for _, lit := range learnedLits {
		varIdx := lit.Var()
		literalInLearned[varIdx] = true
		literalIsNegated[varIdx] = lit.IsNegated()
	}
	
	// Check each learned clause for self-subsumption opportunities
	// A clause (l1 \/ l2 \/ ... \/ ln) can subsume literal li if all other literals are in our learned clause
	toRemove := make([]bool, len(learnedLits))
	
	for _, existingClause := range s.learnedClauses {
		if len(existingClause.Literals) >= len(learnedLits) {
			continue // Can't subsume with a longer or equal clause
		}
		
		// Check if all but one literal of existingClause are in learnedLits
		matchingCount := 0
		mismatchIdx := -1
		
		for _, lit := range existingClause.Literals {
			varIdx := lit.Var()
			if literalInLearned[varIdx] && literalIsNegated[varIdx] == lit.IsNegated() {
				matchingCount++
			} else {
				// Check if this literal's negation is in learnedLits (for self-subsumption)
				if literalInLearned[varIdx] && literalIsNegated[varIdx] != lit.IsNegated() {
					// Found a potential self-subsumption candidate
					// existingClause has lit, learnedLits has !lit
					// This means we can potentially remove !lit from learnedLits
					mismatchIdx = -1 // Mark as valid mismatch for self-subsumption
				} else {
					mismatchIdx = -2 // This clause doesn't help
					break
				}
			}
		}
		
		// If all but one literal match, we can potentially remove one literal
		if mismatchIdx == -1 && matchingCount == len(existingClause.Literals)-1 {
			// Self-subsumption: remove the mismatched literal from learnedLits
			for i, lit := range learnedLits {
				varIdx := lit.Var()
				if literalInLearned[varIdx] && literalIsNegated[varIdx] != existingClause.Literals[0].IsNegated() {
					// Found the literal to remove (simplified check)
					toRemove[i] = true
					break
				}
			}
		}
	}
	
	// Build minimized clause
	minimized := make([]cnf.Literal, 0, len(learnedLits))
	for i, lit := range learnedLits {
		if !toRemove[i] {
			minimized = append(minimized, lit)
		}
	}
	
	return minimized
}

func (s *CDCLSolver) deleteLearnedClauses() {
	// LBD-based clause deletion
	// Keep clauses with high activity or low LBD (learned block distance)
	// Delete clauses with low activity and high LBD
	
	// Calculate LBD for each learned clause
	type clauseInfo struct {
		idx   int
		lbd   int
		age   int
	}
	
	clauses := make([]clauseInfo, 0, len(s.learnedClauses))
	for i, clause := range s.learnedClauses {
		// Count unique decision levels in the clause
		levelSet := make(map[int]bool)
		for _, lit := range clause.Literals {
			lvl := s.assignments[lit.Var()].Level
			if lvl > 0 {
				levelSet[lvl] = true
			}
		}
		lbd := len(levelSet)
		
		clauses = append(clauses, clauseInfo{
			idx: i,
			lbd: lbd,
			age: s.currentAge - s.clauseAge[i],
		})
	}
	
	// Sort by LBD (primary) and age (secondary)
	// Lower LBD = more useful, keep it
	// Higher age = older, more likely to delete
	for i := 0; i < len(clauses); i++ {
		for j := i + 1; j < len(clauses); j++ {
			// Higher LBD and older age = delete first
			scoreI := clauses[i].lbd*100 + clauses[i].age
			scoreJ := clauses[j].lbd*100 + clauses[j].age
			if scoreI < scoreJ {
				clauses[i], clauses[j] = clauses[j], clauses[i]
			}
		}
	}
	
	// Delete bottom 50% of clauses (highest LBD + oldest)
	toDelete := len(s.learnedClauses) / 2
	if toDelete == 0 {
		toDelete = 1
	}
	
	// Mark clauses to delete
	keep := make([]bool, len(s.learnedClauses))
	for i := range keep {
		keep[i] = true
	}
	
	for i := 0; i < toDelete && i < len(clauses); i++ {
		keep[clauses[i].idx] = false
	}
	
	// Compact the slices
	newClauses := make([]cnf.Clause, 0, len(s.learnedClauses)-toDelete)
	newActivity := make([]float64, 0, len(s.learnedClauses)-toDelete)
	newAge := make([]int, 0, len(s.learnedClauses)-toDelete)
	
	for i := range s.learnedClauses {
		if keep[i] {
			newClauses = append(newClauses, s.learnedClauses[i])
			newActivity = append(newActivity, s.clauseActivity[i])
			newAge = append(newAge, s.clauseAge[i])
		}
	}
	
	s.learnedClauses = newClauses
	s.clauseActivity = newActivity
	s.clauseAge = newAge
}

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

	// Flip the decision at the backjump level
	s.assignments[decisionVar] = Assignment{
		Value: !decisionValue,
		Level: bjLevel,
	}
	s.trail = append(s.trail, int(decisionVar))
	
	return true
}

// luby returns the i-th value in the Luby sequence (1-indexed)
// Sequence: 1, 1, 2, 1, 1, 2, 4, 1, 1, 2, 1, 1, 2, 4, 8, ...
func luby(i int) int {
	k := 1
	for {
		ki := 1 << uint(k)
		if i == ki-1 {
			return 1 << uint(k-1)
		}
		if ki-1 > i {
			prevKi := 1 << uint(k-1)
			return luby(i - (ki - 1 - prevKi))
		}
		k++
	}
}

// shouldRestart checks if we should restart using adaptive strategy (Glucose-style)
// combined with Luby sequence as fallback
func (s *CDCLSolver) shouldRestart() bool {
	// Need enough data for adaptive restarts
	if s.lbdCount < 100 {
		// Fall back to Luby sequence until we have enough statistics
		threshold := s.restartBase * luby(s.lubyIndex + 1)
		conflictsSinceRestart := s.conflicts - s.restartCount
		return conflictsSinceRestart >= threshold
	}
	
	// Adaptive restart: restart if current LBD is much worse than average
	// This indicates we're in an unproductive search region
	avgLBD := float64(s.lbdSum) / float64(s.lbdCount)
	
	// Restart if current LBD > 1.5x average (Glucose-style threshold)
	// This is more aggressive than Luby and escapes bad search regions faster
	adaptiveThreshold := avgLBD * 1.5
	
	return float64(s.lastConflictLBD) > adaptiveThreshold
}

// restart performs a restart: clear the trail and reset to level 0
// but keep learned clauses and VSIDS activities
func (s *CDCLSolver) restart() {
	// Clear trail and implication array
	for i := len(s.trail) - 1; i >= 0; i-- {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.level = 0
	s.backjumpLevel = 0
	
	// Clear learned clauses - they reference old trail structure
	// This is necessary for soundness after restart
	s.learnedClauses = s.learnedClauses[:0]
	s.clauseActivity = s.clauseActivity[:0]
	s.clauseAge = s.clauseAge[:0]
	
	// Update restart count to current conflict count (for next threshold calculation)
	s.restartCount = s.conflicts
	
	// Reset LBD statistics for adaptive restarts
	// Start fresh after restart
	s.lbdSum = 0
	s.lbdCount = 0
	s.lastConflictLBD = 0
	
	// Increment Luby index for next restart
	s.lubyIndex++
	
	if s.verbose {
		fmt.Printf("c [verbose] Restart #%d at conflict %d, clearing trail and learned clauses\n", s.lubyIndex, s.conflicts)
	}
	
	// After restart, we need to re-propagate to rebuild the trail
	// This is done automatically by the main solve loop
}

// inprocessing applies simplification techniques during search
// Called periodically (every N conflicts) to clean up the formula
// Returns true if simplification was performed, false if skipped
func (s *CDCLSolver) inprocessing() bool {
	// Skip inprocessing if we're at a high decision level (deep in search)
	// Inprocessing is most effective at lower levels
	if s.level > 10 {
		return false
	}
	
	// Only apply lightweight inprocessing during search
	// Full preprocessing is too expensive
	
	if s.verbose {
		fmt.Printf("c [verbose] Inprocessing #%d at conflict %d, level %d\n", 
			s.inprocCount+1, s.conflicts, s.level)
	}
	
	// Apply subsumption elimination on original clauses
	// This is relatively cheap and can remove redundant clauses
	s.subsumptionElimination()
	
	// Apply limited variable elimination
	// Only try to eliminate variables with low occurrence count
	s.limitedVariableElimination(10) // Max 10 occurrences
	
	// Rebuild short clauses after modifications
	s.cnf.RebuildShortClauses()
	
	s.inprocCount++
	
	if s.verbose {
		fmt.Printf("c [verbose] After inprocessing: %d clauses (%d binary, %d ternary)\n",
			s.cnf.NumClauses, len(s.cnf.BinaryClauses), len(s.cnf.TernaryClauses))
	}
	
	return true
}

// limitedVariableElimination tries to eliminate variables with few occurrences
// maxOccurrences limits which variables to consider (higher = more expensive)
// Returns UNSAT if empty clause created, SAT if all clauses satisfied, UNKNOWN otherwise
func (s *CDCLSolver) limitedVariableElimination(maxOccurrences int) SolveResult {
	eliminatedCount := 0
	
	// Try to eliminate each variable with low occurrence count
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		// Skip assigned variables
		if s.assignments[varIdx].Level != 0 {
			continue
		}
		
		// Count occurrences
		posCount := 0
		negCount := 0
		
		for _, clause := range s.cnf.Clauses {
			if clause.Learned {
				continue // Only consider original clauses
			}
			for _, lit := range clause.Literals {
				if lit.Var() == varIdx {
					if lit.IsNegated() {
						negCount++
					} else {
						posCount++
					}
				}
			}
		}
		
		// Skip if variable appears too often
		if posCount+negCount > maxOccurrences {
			continue
		}
		
		// Skip if variable is pure (already satisfied)
		if posCount == 0 || negCount == 0 {
			continue
		}
		
		// Find clauses containing x and ¬x
		posClauses := make([]int, 0)
		negClauses := make([]int, 0)
		
		for i, clause := range s.cnf.Clauses {
			if clause.Learned {
				continue
			}
			for _, lit := range clause.Literals {
				if lit.Var() == varIdx {
					if !lit.IsNegated() {
						posClauses = append(posClauses, i)
					} else {
						negClauses = append(negClauses, i)
					}
					break
				}
			}
		}
		
		// Compute resolvents
		resolvents := make([]cnf.Clause, 0)
		resolventSet := make(map[string]bool)
		
		for _, posIdx := range posClauses {
			posClause := s.cnf.Clauses[posIdx]
			
			for _, negIdx := range negClauses {
				negClause := s.cnf.Clauses[negIdx]
				
				resolvent := s.resolve(posClause, negClause, varIdx)
				if resolvent != nil {
					if !s.isTautology(resolvent) {
						key := s.clauseKey(resolvent)
						if !resolventSet[key] {
							resolventSet[key] = true
							resolvents = append(resolvents, *resolvent)
						}
					}
				}
			}
		}
		
		// Check if elimination is beneficial
		originalCount := len(posClauses) + len(negClauses)
		if len(resolvents) < originalCount {
			// Check for empty clause
			for _, resolvent := range resolvents {
				if len(resolvent.Literals) == 0 {
					if s.verbose {
						fmt.Printf("c [verbose] Inprocessing: empty clause from var %d elimination\n", varIdx)
					}
					return UNSAT
				}
			}
			
			// Remove original clauses and add resolvents
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
				keepClauses = append(keepClauses, resolvent)
			}
			
			s.cnf.Clauses = keepClauses
			s.cnf.NumClauses = len(keepClauses)
			
			eliminatedCount++
			
			if s.verbose {
				fmt.Printf("c [verbose] Inprocessing: eliminated var %d (%d -> %d clauses)\n",
					varIdx, originalCount, len(resolvents))
			}
		}
	}
	
	if s.verbose && eliminatedCount > 0 {
		fmt.Printf("c [verbose] Inprocessing: eliminated %d variables\n", eliminatedCount)
	}
	
	// Check if all clauses satisfied
	if len(s.cnf.Clauses) == 0 {
		return SAT
	}
	
	return UNKNOWN
}

// Debug methods for testing
func (s *CDCLSolver) PropagateDebug() (bool, int) {
	return s.propagate()
}

func (s *CDCLSolver) AllAssignedDebug() bool {
	return s.allAssigned()
}

func (s *CDCLSolver) DecideDebug() bool {
	return s.decide()
}
