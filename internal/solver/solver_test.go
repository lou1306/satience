package solver

import (
	"satience/internal/cnf"
	"testing"
)

func newClause(lits ...int32) cnf.Clause {
	clause := cnf.Clause{
		Literals: make([]cnf.Literal, 0, len(lits)),
	}
	for _, l := range lits {
		negated := l < 0
		varIdx := uint32(l)
		if negated {
			varIdx = uint32(-l)
		}
		clause.Literals = append(clause.Literals, cnf.NewLiteral(varIdx-1, negated))
	}
	return clause
}

func TestSolveUnsat(t *testing.T) {
	c := cnf.CNF{
		NumVars: 1,
		Clauses: []cnf.Clause{
			newClause(1),
			newClause(-1),
		},
		NumClauses: 2,
	}

	s := NewSolver(&c)
	if s.Solve() {
		t.Error("Expected UNSAT")
	}
}

func TestSolveSat(t *testing.T) {
	c := cnf.CNF{
		NumVars: 3,
		Clauses: []cnf.Clause{
			newClause(1, 2),
			newClause(-1, 3),
		},
		NumClauses: 2,
	}

	s := NewSolver(&c)
	if !s.Solve() {
		t.Error("Expected SAT")
	}
}

func TestSolveEmpty(t *testing.T) {
	c := cnf.CNF{
		NumVars:    3,
		Clauses:    []cnf.Clause{},
		NumClauses: 0,
	}

	s := NewSolver(&c)
	if !s.Solve() {
		t.Error("Expected SAT (empty formula)")
	}
}

func TestSolveUnitPropagation(t *testing.T) {
	// x1 must be true, and (-x1 OR x2) means x2 must be true
	c := cnf.CNF{
		NumVars: 2,
		Clauses: []cnf.Clause{
			newClause(1),
			newClause(-1, 2),
		},
		NumClauses: 2,
	}

	s := NewSolver(&c)
	if !s.Solve() {
		t.Error("Expected SAT")
	}
}

func TestSolve3SAT(t *testing.T) {
	c := cnf.CNF{
		NumVars: 4,
		Clauses: []cnf.Clause{
			newClause(1, 2, 3),
			newClause(1, -2, 4),
			newClause(-1, 2, -4),
			newClause(-1, -2, -3),
		},
		NumClauses: 4,
	}

	s := NewSolver(&c)
	if !s.Solve() {
		t.Error("Expected SAT")
	}
}

func TestSolveUnsat3SAT(t *testing.T) {
	// Create an unsatisfiable 3-SAT instance
	c := cnf.CNF{
		NumVars: 3,
		Clauses: []cnf.Clause{
			newClause(1, 2, 3),
			newClause(1, 2, -3),
			newClause(1, -2, 3),
			newClause(1, -2, -3),
			newClause(-1, 2, 3),
			newClause(-1, 2, -3),
			newClause(-1, -2, 3),
			newClause(-1, -2, -3),
		},
		NumClauses: 8,
	}

	s := NewSolver(&c)
	if s.Solve() {
		t.Error("Expected UNSAT (all combinations covered)")
	}
}

func TestCDCLSolveUnsat(t *testing.T) {
	c := cnf.CNF{
		NumVars: 1,
		Clauses: []cnf.Clause{
			newClause(1),
			newClause(-1),
		},
		NumClauses: 2,
	}

	s := NewCDCLSolver(&c)
	if s.Solve() {
		t.Error("Expected UNSAT")
	}
}

func TestCDCLSolveSat(t *testing.T) {
	c := cnf.CNF{
		NumVars: 3,
		Clauses: []cnf.Clause{
			newClause(1, 2),
			newClause(-1, 3),
		},
		NumClauses: 2,
	}

	s := NewCDCLSolver(&c)
	if !s.Solve() {
		t.Error("Expected SAT")
	}
}

func TestCDCLSolve3SAT(t *testing.T) {
	c := cnf.CNF{
		NumVars: 4,
		Clauses: []cnf.Clause{
			newClause(1, 2, 3),
			newClause(1, -2, 4),
			newClause(-1, 2, -4),
			newClause(-1, -2, -3),
		},
		NumClauses: 4,
	}

	s := NewCDCLSolver(&c)
	if !s.Solve() {
		t.Error("Expected SAT")
	}
}

func TestCDCLSolveUnsat3SAT(t *testing.T) {
	c := cnf.CNF{
		NumVars:  3,
		Clauses: []cnf.Clause{
			newClause(1, 2, 3),
			newClause(1, 2, -3),
			newClause(1, -2, 3),
			newClause(1, -2, -3),
			newClause(-1, 2, 3),
			newClause(-1, 2, -3),
			newClause(-1, -2, 3),
			newClause(-1, -2, -3),
		},
		NumClauses: 8,
	}

	s := NewCDCLSolver(&c)
	s.SetMaxIter(1000000)
	result := s.SolveWithResult()
	if result != UNSAT && result != UNKNOWN {
		t.Errorf("Expected UNSAT or UNKNOWN (all combinations covered), got %v (iterations=%d)", result, s.iterations)
	}
}
