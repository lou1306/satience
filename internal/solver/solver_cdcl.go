package solver

import (
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
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
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
	}
}

// SetMaxIter sets the maximum iteration limit (0 = unlimited)
func (s *CDCLSolver) SetMaxIter(limit int) {
	s.maxIter = limit
}

func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

func (s *CDCLSolver) SolveWithResult() SolveResult {
	for {
		s.iterations++
		if s.maxIter > 0 && s.iterations > s.maxIter {
			return UNKNOWN
		}
		
		conflict, clauseIdx := s.propagate()
		if conflict {
			s.handleConflict(clauseIdx)
			if !s.backtrack() {
				return UNSAT
			}
			continue
		}

		if s.allAssigned() {
			return SAT
		}

		if !s.decide() {
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

	for trailIndex < len(s.trail) {
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
				litLevel := s.assignments[lit.Var()].Level
				if litLevel == 0 {
					unassignedCount++
					unassignedLit = lit
				} else if s.literalIsTrue(lit) {
					satisfiedCount++
				} else {
					falseCount++
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
				s.assignLiteral(unassignedLit, s.level, clauseIdx)
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
				litLevel := s.assignments[lit.Var()].Level
				if litLevel == 0 {
					unassignedCount++
					unassignedLit = lit
				} else if s.literalIsTrue(lit) {
					satisfiedCount++
				} else {
					falseCount++
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

	varIdx := s.vsids.selectVariable(s.assignments)

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, false), s.level, -1)
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
	
	// Learn clause using 1-UIP analysis
	s.learnClause(conflictLits)

	if s.conflicts%100 == 0 {
		s.vsids.decay()
	}
}

func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) {
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
		s.learnedClauses = append(s.learnedClauses, cnf.Clause{
			Literals: learnedLits,
			Learned:  true,
		})
	}
}

func (s *CDCLSolver) backtrack() bool {
	if len(s.trailHead) <= 1 {
		return false
	}

	prevLevel := s.level - 1
	if prevLevel < 1 {
		return false
	}
	
	decisionPoint := s.trailHead[prevLevel]
	if decisionPoint >= len(s.trail) {
		return false
	}
	
	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value

	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:prevLevel+1]
	s.level = prevLevel

	s.assignments[decisionVar] = Assignment{
		Value: !decisionValue,
		Level: prevLevel,
	}
	s.trail = append(s.trail, int(decisionVar))
	
	return true
}


