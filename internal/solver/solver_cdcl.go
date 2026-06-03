package solver

import (
	"satience/internal/cnf"
)

// CDCLSolver implements a CDCL solver with clause learning
type CDCLSolver struct {
	cnf          *cnf.CNF
	assignments  []Assignment
	trail        []int
	trailHead    []int
	level        int
	vsids        *VSIDS
	conflicts    int
	analyzer     *ConflictAnalyzer
	implication  []int
	restarts     *LubyRestarts
	db           *ClauseDatabase
	backjumpLevel int
}

// NewCDCLSolver creates a new CDCL solver with clause learning
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	solver := &CDCLSolver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		trailHead:   make([]int, 1),
		level:       0,
		vsids:       NewVSIDS(formula.NumVars),
		conflicts:   0,
		implication: make([]int, formula.NumVars),
		restarts:    NewLubyRestarts(1000000), // Disable restarts for debugging
		db:          NewClauseDatabase(10000),
	}

	solver.analyzer = NewConflictAnalyzer(formula, solver.assignments)

	return solver
}

func (s *CDCLSolver) Solve() bool {
	iterations := 0
	for {
		iterations++
		if iterations % 1000 == 0 {
		}
		if iterations > 10000 {
			return false
		}
		
		conflict, clauseIdx := s.propagate()
		if conflict {
			s.handleConflict(clauseIdx)
			if s.restarts.AddConflict() {
				s.restart()
			}
			if !s.backtrack() {
				return false
			}
			continue
		}

		if s.allAssigned() {
			return true
		}

		if !s.decide() {
			return false
		}
	}
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
		trailIndex++

		// Check all clauses for conflicts and unit propagation
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
				// After assigning, restart clause checking from the beginning
				// to catch any new unit clauses or conflicts
				break
			}
		}
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

	clause := &s.cnf.Clauses[clauseIdx]
	s.vsids.bumpClause(clause.Literals)

	learnedClause, backjumpLevel := s.analyzer.Analyze(
		clause,
		s.trail,
		s.trailHead,
		s.level,
	)

	if len(learnedClause.Literals) > 0 {
		s.db.Add(*learnedClause)
		s.cnf.Clauses = append(s.cnf.Clauses, *learnedClause)
		
		s.analyzer = NewConflictAnalyzer(s.cnf, s.assignments)
		
		if s.db.Len() > 10000 {
			s.db.Cleanup()
		}
	}

	if s.conflicts%100 == 0 {
		s.vsids.decay()
	}
	
	// Store the backjump level for use in backtrack
	s.backjumpLevel = backjumpLevel
}

func (s *CDCLSolver) backtrack() bool {
	if len(s.trailHead) <= 1 {
		return false
	}

	// Use backjump level if available, otherwise backtrack chronologically
	targetLevel := s.backjumpLevel
	if targetLevel >= s.level || targetLevel < 0 {
		// Invalid backjump level, use chronological backtracking
		targetLevel = s.level - 1
	}
	
	if targetLevel < 1 {
		targetLevel = 1
	}
	
	if targetLevel >= len(s.trailHead) {
		targetLevel = len(s.trailHead) - 1
	}

	decisionPoint := s.trailHead[targetLevel-1]

	// Get the decision variable and value from the decision point
	if decisionPoint >= len(s.trail) {
		return false
	}
	
	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value
	

	// Clear assignments from the decision point onward
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:targetLevel]
	s.level = targetLevel - 1

	// Try the opposite value for the decision variable
	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(decisionVar, !decisionValue), s.level, -1)
	return true
}

func (s *CDCLSolver) restart() {
	for i := 0; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.level = 0
}

func (s *CDCLSolver) GetModel() []bool {
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			return nil
		}
	}

	model := make([]bool, s.cnf.NumVars)
	for i, assign := range s.assignments {
		model[i] = assign.Value
	}
	return model
}
