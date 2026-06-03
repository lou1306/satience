package solver

import (
	"satience/internal/cnf"
)

// CDCLSolver implements a full CDCL solver with clause learning
type CDCLSolver struct {
	cnf           *cnf.CNF
	assignments   []Assignment
	trail         []int
	trailHead     []int
	level         int
	watches       [][][]int
	vsids         *VSIDS
	conflicts     int
	learned       []cnf.Clause
	analyzer      *ConflictAnalyzer
	implication   []int // implication[var] = clause that implied it
}

// NewCDCLSolver creates a new CDCL solver with clause learning
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	watches := make([][][]int, formula.NumVars)
	for i := range watches {
		watches[i] = make([][]int, 2)
		watches[i][0] = make([]int, 0)
		watches[i][1] = make([]int, 0)
	}

	solver := &CDCLSolver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		trailHead:   make([]int, 1),
		level:       0,
		watches:     watches,
		vsids:       NewVSIDS(formula.NumVars),
		conflicts:   0,
		learned:     make([]cnf.Clause, 0),
		implication: make([]int, formula.NumVars),
	}

	// Initialize watches
	for clauseIdx := range formula.Clauses {
		clause := &formula.Clauses[clauseIdx]
		for i := 0; i < 2 && i < len(clause.Literals); i++ {
			solver.addWatch(clauseIdx, i)
		}
	}

	solver.analyzer = NewConflictAnalyzer(formula, solver.assignments)

	return solver
}

func (s *CDCLSolver) addWatch(clauseIdx int, litIdx int) {
	clause := &s.cnf.Clauses[clauseIdx]
	lit := clause.Literals[litIdx]
	varIdx := lit.Var()
	polarity := 0
	if lit.IsNegated() {
		polarity = 1
	}
	s.watches[varIdx][polarity] = append(s.watches[varIdx][polarity], clauseIdx)
}

func (s *CDCLSolver) Solve() bool {
	for {
		conflict, clauseIdx := s.propagate()
		if conflict {
			s.handleConflict(clauseIdx)
			if !s.backtrack() {
				return false
			}
			continue
		}

		if len(s.trail) == int(s.cnf.NumVars) {
			return true
		}

		if !s.decide() {
			return false
		}
	}
}

func (s *CDCLSolver) propagate() (bool, int) {
	trailIndex := s.trailHead[s.level]

	for trailIndex < len(s.trail) {
		varIdx := uint32(s.trail[trailIndex])
		value := s.assignments[varIdx].Value
		trailIndex++

		polarity := 1
		if !value {
			polarity = 0
		}

		clauses := s.watches[varIdx][polarity]

		for _, clauseIdx := range clauses {
			clause := &s.cnf.Clauses[clauseIdx]

			// Check if clause is satisfied
			satisfied := false
			for _, lit := range clause.Literals {
				if s.assignments[lit.Var()].Level > 0 && s.literalIsTrue(lit) {
					satisfied = true
					break
				}
			}
			if satisfied {
				continue
			}

			// Count unassigned and find potential unit
			unassignedCount := 0
			var unassignedLit cnf.Literal

			for _, lit := range clause.Literals {
				if s.assignments[lit.Var()].Level == 0 {
					unassignedCount++
					unassignedLit = lit
				}
			}

			if unassignedCount == 0 {
				return true, clauseIdx // Conflict
			}

			if unassignedCount == 1 {
				s.assignLiteral(unassignedLit, s.level, clauseIdx)
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

	if s.assignments[varIdx].Level > 0 {
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

	// Bump activity for variables involved in conflict
	clause := &s.cnf.Clauses[clauseIdx]
	s.vsids.bumpClause(clause.Literals)

	// Perform conflict analysis and learn clause
	learnedClause, _ := s.analyzer.Analyze(
		clause,
		s.trail,
		s.trailHead,
		s.level,
	)

	if len(learnedClause.Literals) > 0 {
		s.learned = append(s.learned, *learnedClause)
		s.cnf.Clauses = append(s.cnf.Clauses, *learnedClause)
		
		// Add watches for learned clause
		clauseIdx := len(s.cnf.Clauses) - 1
		for i := 0; i < 2 && i < len(learnedClause.Literals); i++ {
			s.addWatch(clauseIdx, i)
		}
		
		// Update analyzer with new clause
		s.analyzer = NewConflictAnalyzer(s.cnf, s.assignments)
	}

	// Decay activity periodically
	if s.conflicts%100 == 0 {
		s.vsids.decay()
	}
}

func (s *CDCLSolver) backtrack() bool {
	for {
		if len(s.trailHead) <= 1 {
			return false
		}

		s.trailHead = s.trailHead[:len(s.trailHead)-1]
		decisionPoint := s.trailHead[len(s.trailHead)-1]

		var decisionVar uint32 = 0
		var decisionValue bool = false

		if decisionPoint < len(s.trail) {
			decisionVar = uint32(s.trail[decisionPoint])
			decisionValue = s.assignments[decisionVar].Value
		}

		for i := decisionPoint; i < len(s.trail); i++ {
			varIdx := uint32(s.trail[i])
			s.assignments[varIdx] = Assignment{}
			s.implication[varIdx] = -1
		}
		s.trail = s.trail[:decisionPoint]
		s.level--

		if decisionPoint >= len(s.trail) {
			continue
		}

		s.level++
		s.trailHead = append(s.trailHead, len(s.trail))
		s.assignLiteral(cnf.NewLiteral(decisionVar, !decisionValue), s.level, -1)
		return true
	}
}

// GetModel returns the satisfying assignment (true=positive, false=negative)
// Returns nil if no satisfying assignment exists
func (s *CDCLSolver) GetModel() []bool {
	if len(s.trail) != int(s.cnf.NumVars) {
		return nil
	}

	model := make([]bool, s.cnf.NumVars)
	for i, assign := range s.assignments {
		model[i] = assign.Value
	}
	return model
}
