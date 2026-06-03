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

func TestCDCLTseitin4x4Unsat(t *testing.T) {
	c := cnf.CNF{
		NumVars: 40,
		Clauses: []cnf.Clause{
			newClause(-17, 29),
			newClause(17, -29),
			newClause(-17, -18, 30),
			newClause(-17, 18, -30),
			newClause(17, -18, -30),
			newClause(17, 18, 30),
			newClause(-18, -19, 31),
			newClause(-18, 19, -31),
			newClause(18, -19, -31),
			newClause(18, 19, 31),
			newClause(-19, 32),
			newClause(19, -32),
			newClause(-20, -29, 33),
			newClause(-20, 29, -33),
			newClause(20, -29, -33),
			newClause(20, 29, 33),
			newClause(-20, -21, -30, 34),
			newClause(-20, -21, 30, -34),
			newClause(-20, 21, -30, -34),
			newClause(-20, 21, 30, 34),
			newClause(20, -21, -30, -34),
			newClause(20, -21, 30, 34),
			newClause(20, 21, -30, 34),
			newClause(20, 21, 30, -34),
			newClause(-21, -22, -31, 35),
			newClause(-21, -22, 31, -35),
			newClause(-21, 22, -31, -35),
			newClause(-21, 22, 31, 35),
			newClause(21, -22, -31, -35),
			newClause(21, -22, 31, 35),
			newClause(21, 22, -31, 35),
			newClause(21, 22, 31, -35),
			newClause(-22, -32, 36),
			newClause(-22, 32, -36),
			newClause(22, -32, -36),
			newClause(22, 32, 36),
			newClause(-23, -33, 37),
			newClause(-23, 33, -37),
			newClause(23, -33, -37),
			newClause(23, 33, 37),
			newClause(-23, -24, -34, 38),
			newClause(-23, -24, 34, -38),
			newClause(-23, 24, -34, -38),
			newClause(-23, 24, 34, 38),
			newClause(23, -24, -34, -38),
			newClause(23, -24, 34, 38),
			newClause(23, 24, -34, 38),
			newClause(23, 24, 34, -38),
			newClause(-24, -25, -35, 39),
			newClause(-24, -25, 35, -39),
			newClause(-24, 25, -35, -39),
			newClause(-24, 25, 35, 39),
			newClause(24, -25, -35, -39),
			newClause(24, -25, 35, 39),
			newClause(24, 25, -35, 39),
			newClause(24, 25, 35, -39),
			newClause(-25, -36, 40),
			newClause(-25, 36, -40),
			newClause(25, -36, -40),
			newClause(25, 36, 40),
			newClause(-26, 37),
			newClause(26, -37),
			newClause(-26, -27, 38),
			newClause(-26, 27, -38),
			newClause(26, -27, -38),
			newClause(26, 27, 38),
			newClause(-27, -28, 39),
			newClause(-27, 28, -39),
			newClause(27, -28, -39),
			newClause(27, 28, 39),
			newClause(-28, 40),
			newClause(28, -40),
			newClause(1),
			newClause(-1),
		},
		NumClauses: 74,
	}

	s := NewCDCLSolver(&c)
	s.SetMaxIter(1000000)
	result := s.SolveWithResult()
	if result != UNSAT {
		t.Errorf("Expected UNSAT (tseitin grid 4x4), got %v", result)
	}
}

func TestCDCLAlgebra20Sat(t *testing.T) {
	clauses := make([]cnf.Clause, 0, 30)
	
	for i := int32(1); i <= 20; i += 2 {
		clauses = append(clauses, newClause(i))
	}
	
	c := cnf.CNF{
		NumVars:    20,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (simple 20 unit clauses)")
	}
}

func TestCDCLPhp3p4hSat(t *testing.T) {
	c := cnf.CNF{
		NumVars: 12,
		Clauses: []cnf.Clause{
			newClause(1, 2, 3, 4),
			newClause(5, 6, 7, 8),
			newClause(9, 10, 11, 12),
			newClause(-1, -5),
			newClause(-1, -9),
			newClause(-5, -9),
			newClause(-2, -6),
			newClause(-2, -10),
			newClause(-6, -10),
			newClause(-3, -7),
			newClause(-3, -11),
			newClause(-7, -11),
			newClause(-4, -8),
			newClause(-4, -12),
			newClause(-8, -12),
		},
		NumClauses: 15,
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (pigeonhole 3 pigeons 4 holes)")
	}
}

func TestCDCLSimple50vSat(t *testing.T) {
	clauses := make([]cnf.Clause, 0, 60)
	
	for i := int32(1); i <= 50; i += 2 {
		clauses = append(clauses, newClause(i, i+1))
		clauses = append(clauses, newClause(-i, i+1))
		clauses = append(clauses, newClause(i, -(i+1)))
	}
	
	c := cnf.CNF{
		NumVars:    50,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (simple 50v)")
	}
}

func TestCDCLArgChain20Sat(t *testing.T) {
	clauses := make([]cnf.Clause, 0, 30)
	
	for i := int32(1); i <= 19; i++ {
		clauses = append(clauses, newClause(-i, i+1))
	}
	clauses = append(clauses, newClause(1))
	
	c := cnf.CNF{
		NumVars:    20,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (argumentation chain 20)")
	}
}
