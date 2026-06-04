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
	
	if s.verbose {
		fmt.Printf("c [verbose] After preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
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
		
		// Check original clauses for conflicts and unit propagation
		for clauseIdx := range s.cnf.Clauses {
			clause := &s.cnf.Clauses[clauseIdx]
			
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
	
	return backjumpLevel
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

// shouldRestart checks if we should restart based on Luby sequence
func (s *CDCLSolver) shouldRestart() bool {
	// Calculate restart threshold using Luby sequence
	threshold := s.restartBase * luby(s.lubyIndex + 1)
	// Count conflicts since last restart
	conflictsSinceRestart := s.conflicts - s.restartCount
	return conflictsSinceRestart >= threshold
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
	
	// Increment Luby index for next restart
	s.lubyIndex++
	
	if s.verbose {
		fmt.Printf("c [verbose] Restart #%d at conflict %d, clearing trail and learned clauses\n", s.lubyIndex, s.conflicts)
	}
	
	// After restart, we need to re-propagate to rebuild the trail
	// This is done automatically by the main solve loop
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
