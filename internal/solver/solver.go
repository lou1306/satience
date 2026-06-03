package solver

import (
	"satience/internal/cnf"
)

// Assignment tracks variable assignments
type Assignment struct {
	Value bool // true = positive, false = negative
	Level int  // Decision level (0 = unassigned)
}

// Solver implements a basic DPLL algorithm with backtracking
type Solver struct {
	cnf         *cnf.CNF
	assignments []Assignment
	trail       []int // Stack of assigned variable indices
	decisions   []int // Stack of decision points in trail
	level       int   // Current decision level
}

// NewSolver creates a new SAT solver
func NewSolver(formula *cnf.CNF) *Solver {
	return &Solver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		decisions:   make([]int, 0),
		level:       0,
	}
}

// Solve returns true if the formula is satisfiable
func (s *Solver) Solve() bool {
	for {
		// Unit propagation
		if conflict := s.propagate(); conflict {
			// Backtrack (returns false if no more backtracking possible)
			if !s.backtrack() {
				return false // UNSAT
			}
			continue
		}

		// Check if all variables are assigned
		if len(s.trail) == int(s.cnf.NumVars) {
			return true // SAT
		}

		// Make a decision
		if !s.decide() {
			return false // No unassigned variables
		}
	}
}

// propagate performs unit propagation
// Returns true if a conflict is detected
func (s *Solver) propagate() bool {
	trailIndex := 0
	for trailIndex < len(s.trail) {
		trailIndex++

		// Check all clauses for unit propagation
		for i := range s.cnf.Clauses {
			clause := &s.cnf.Clauses[i]
			result := s.checkClause(clause)
			switch result {
			case clauseConflict:
				return true
			case clauseUnit:
				// Find and assign the unit literal
				for _, lit := range clause.Literals {
					if s.assignments[lit.Var()].Level == 0 {
						s.assignLiteral(lit, s.level)
						break
					}
				}
			}
		}
	}

	return false
}

type clauseResult int

const (
	clauseSatisfied clauseResult = iota
	clauseUnit
	clauseConflict
	clauseUnresolved
)

// checkClause checks the status of a clause
func (s *Solver) checkClause(clause *cnf.Clause) clauseResult {
	unassignedCount := 0
	satisfied := false

	for _, lit := range clause.Literals {
		assign := s.assignments[lit.Var()]
		if assign.Level == 0 {
			unassignedCount++
		} else if s.literalIsTrue(lit) {
			satisfied = true
		}
	}

	if satisfied {
		return clauseSatisfied
	}
	if unassignedCount == 0 {
		return clauseConflict // All literals false
	}
	if unassignedCount == 1 {
		return clauseUnit
	}
	return clauseUnresolved
}

// literalIsTrue checks if a literal evaluates to true under current assignment
func (s *Solver) literalIsTrue(lit cnf.Literal) bool {
	assign := s.assignments[lit.Var()]
	if lit.IsNegated() {
		return !assign.Value
	}
	return assign.Value
}

// assignLiteral assigns a truth value to a literal
func (s *Solver) assignLiteral(lit cnf.Literal, level int) {
	varIdx := lit.Var()

	// Check if already assigned
	if s.assignments[varIdx].Level > 0 {
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value: value,
		Level: level,
	}
	s.trail = append(s.trail, int(varIdx))
}

// decide makes a decision on an unassigned variable
func (s *Solver) decide() bool {
	// Find first unassigned variable (simple heuristic)
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			s.level++
			s.decisions = append(s.decisions, len(s.trail))
			// Decide positive polarity (simple heuristic)
			s.assignLiteral(cnf.NewLiteral(i, false), s.level)
			return true
		}
	}
	return false
}

// backtrack undoes assignments and tries alternative
// Returns false if no more backtracking possible (UNSAT)
func (s *Solver) backtrack() bool {
	// Undo assignments at current level and track what we've tried
	for {
		if len(s.decisions) == 0 {
			return false // No more backtracking possible
		}

		// Get the decision point
		decisionPoint := s.decisions[len(s.decisions)-1]
		s.decisions = s.decisions[:len(s.decisions)-1]

		// Find the decision variable at this level
		var decisionVar uint32 = 0
		var decisionValue bool = false
		foundDecision := false
		
		for i := decisionPoint; i < len(s.trail); i++ {
			varIdx := uint32(s.trail[i])
			if s.assignments[varIdx].Level == s.level {
				decisionVar = varIdx
				decisionValue = s.assignments[varIdx].Value
				foundDecision = true
				break
			}
		}

		// Undo all assignments at current level and higher
		newTrail := make([]int, 0, decisionPoint)
		for i := 0; i < decisionPoint; i++ {
			varIdx := uint32(s.trail[i])
			if s.assignments[varIdx].Level < s.level {
				newTrail = append(newTrail, s.trail[i])
			} else {
				s.assignments[varIdx] = Assignment{}
			}
		}
		for i := decisionPoint; i < len(s.trail); i++ {
			varIdx := uint32(s.trail[i])
			s.assignments[varIdx] = Assignment{}
		}
		s.trail = newTrail
		s.level--

		if !foundDecision {
			continue
		}

		// If the decision was positive (false = negative literal), try negative
		// If the decision was negative, we need to backtrack further
		if !decisionValue {
			// Was positive, now try negative
			s.level++
			s.decisions = append(s.decisions, len(s.trail))
			s.assignLiteral(cnf.NewLiteral(decisionVar, true), s.level)
			return true
		}
		// Was already negative, continue backtracking to parent level
	}
}
