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
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	maxLearned := 10000 // Initial limit on learned clauses
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

// EnableLRB enables LRB (Learning Rate Based) heuristic
func (s *CDCLSolver) EnableLRB() {
	s.vsids.EnableLRB()
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

func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

func (s *CDCLSolver) SolveWithResult() SolveResult {
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

func (s *CDCLSolver) deleteLearnedClauses() {
	// Tiered LBD-based clause deletion
	// Tier 1: Glue clauses (LBD ≤ 2) - NEVER delete
	// Tier 2: Useful clauses (LBD 3-6) - delete if old
	// Tier 3: Trash clauses (LBD > 6) - delete aggressively
	
	// Calculate LBD and tier for each learned clause
	type clauseInfo struct {
		idx   int
		lbd   int
		age   int
		tier  int
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
		
		// Determine tier
		var tier int
		if lbd <= 2 {
			tier = 1 // Glue - protect
		} else if lbd <= 6 {
			tier = 2 // Useful
		} else {
			tier = 3 // Trash
		}
		
		clauses = append(clauses, clauseInfo{
			idx:  i,
			lbd:  lbd,
			age:  s.currentAge - s.clauseAge[i],
			tier: tier,
		})
	}
	
	// Sort by tier (primary), then LBD, then age
	// Delete tier 3 first, then tier 2, never tier 1
	for i := 0; i < len(clauses); i++ {
		for j := i + 1; j < len(clauses); j++ {
			// Higher tier = delete first
			// Within same tier: higher LBD and older age = delete first
			if clauses[i].tier < clauses[j].tier {
				continue // i is better tier, keep order
			}
			if clauses[i].tier > clauses[j].tier {
				clauses[i], clauses[j] = clauses[j], clauses[i]
				continue
			}
			// Same tier: compare by score
			scoreI := clauses[i].lbd*100 + clauses[i].age
			scoreJ := clauses[j].lbd*100 + clauses[j].age
			if scoreI < scoreJ {
				continue // i is better, keep order
			}
			clauses[i], clauses[j] = clauses[j], clauses[i]
		}
	}
	
	// Determine how many to delete (target 50% reduction, but protect glues)
	toDelete := len(s.learnedClauses) / 2
	
	// Mark clauses to delete (skip glue clauses)
	keep := make([]bool, len(s.learnedClauses))
	deleted := 0
	
	for i := range keep {
		keep[i] = true
	}
	
	for i := 0; i < len(clauses) && deleted < toDelete; i++ {
		idx := clauses[i].idx
		// Never delete glue clauses (tier 1)
		if clauses[i].tier == 1 {
			continue
		}
		keep[idx] = false
		deleted++
	}
	
	// Compact the slices
	newClauses := make([]cnf.Clause, 0, len(s.learnedClauses)-deleted)
	newActivity := make([]float64, 0, len(s.learnedClauses)-deleted)
	newAge := make([]int, 0, len(s.learnedClauses)-deleted)
	
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


