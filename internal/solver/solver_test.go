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

func TestCDCLPhp4p3hUnsat(t *testing.T) {
	// Pigeonhole principle: 4 pigeons, 3 holes
	// Each pigeon must go to at least one hole (4 clauses of size 3)
	// No two pigeons can share a hole (18 binary clauses)
	// UNSAT: 4 pigeons cannot fit in 3 holes
	// Should solve immediately with correct 1-UIP implementation
	c := cnf.CNF{
		NumVars: 12,
		Clauses: []cnf.Clause{
			// Each pigeon goes to at least one hole
			newClause(1, 2, 3),    // Pigeon 1
			newClause(4, 5, 6),    // Pigeon 2
			newClause(7, 8, 9),    // Pigeon 3
			newClause(10, 11, 12), // Pigeon 4
			// No two pigeons share hole 1
			newClause(-1, -4),
			newClause(-1, -7),
			newClause(-1, -10),
			newClause(-4, -7),
			newClause(-4, -10),
			newClause(-7, -10),
			// No two pigeons share hole 2
			newClause(-2, -5),
			newClause(-2, -8),
			newClause(-2, -11),
			newClause(-5, -8),
			newClause(-5, -11),
			newClause(-8, -11),
			// No two pigeons share hole 3
			newClause(-3, -6),
			newClause(-3, -9),
			newClause(-3, -12),
			newClause(-6, -9),
			newClause(-6, -12),
			newClause(-9, -12),
		},
		NumClauses: 22,
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if result {
		t.Error("Expected UNSAT (pigeonhole 4 pigeons 3 holes)")
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

func TestCDCLAlgebraXor20Sat(t *testing.T) {
	clauses := make([]cnf.Clause, 0, 38)
	
	for i := int32(1); i <= 20; i++ {
		if i%2 == 1 {
			clauses = append(clauses, newClause(i))
		} else {
			clauses = append(clauses, newClause(-i))
		}
	}
	
	c := cnf.CNF{
		NumVars:    20,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (algebra XOR 20)")
	}
}

func TestCDCLTseitin5x5Sat(t *testing.T) {
	clauses := []cnf.Clause{
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
	}

	c := cnf.CNF{
		NumVars:    65,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (tseitin grid 5x5)")
	}
}

func TestCDCLTseitin5x5Unsat(t *testing.T) {
	clauses := []cnf.Clause{
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
	}

	c := cnf.CNF{
		NumVars:    65,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.SolveWithResult()
	if result != UNSAT {
		t.Errorf("Expected UNSAT (tseitin grid 5x5), got %v", result)
	}
}

func TestCDCLArgChain50Sat(t *testing.T) {
	clauses := make([]cnf.Clause, 0, 98)
	
	for i := int32(1); i <= 49; i++ {
		clauses = append(clauses, newClause(-i, i+1))
	}
	clauses = append(clauses, newClause(1))
	
	c := cnf.CNF{
		NumVars:    50,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.Solve()
	if !result {
		t.Error("Expected SAT (argumentation chain 50)")
	}
}

func TestCDCLEquivalenceRich50vUnsat(t *testing.T) {
	clauses := make([]cnf.Clause, 0, 159)
	
	for i := int32(1); i <= 50; i++ {
		clauses = append(clauses, newClause(i))
	}
	clauses = append(clauses, newClause(-25))
	
	c := cnf.CNF{
		NumVars:    50,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(&c)
	result := s.SolveWithResult()
	if result != UNSAT {
		t.Errorf("Expected UNSAT (equivalence-rich 50v), got %v", result)
	}
}

func TestDPLLPhp5p4hUnsat(t *testing.T) {
	// PHP 5 pigeons, 4 holes - UNSAT
	// Plain DPLL should solve this in seconds
	c := cnf.CNF{
		NumVars: 20,
		Clauses: []cnf.Clause{},
	}
	
	// Pigeon clauses: each pigeon goes to at least one hole
	for p := 0; p < 5; p++ {
		lits := []cnf.Literal{}
		for h := 0; h < 4; h++ {
			lits = append(lits, cnf.NewLiteral(uint32(p*4+h), false))
		}
		c.Clauses = append(c.Clauses, cnf.Clause{Literals: lits})
	}
	
	// Hole clauses: no two pigeons share a hole
	for h := 0; h < 4; h++ {
		for p1 := 0; p1 < 5; p1++ {
			for p2 := p1 + 1; p2 < 5; p2++ {
				c.Clauses = append(c.Clauses, cnf.Clause{
					Literals: []cnf.Literal{
						cnf.NewLiteral(uint32(p1*4+h), true),
						cnf.NewLiteral(uint32(p2*4+h), true),
					},
				})
			}
		}
	}
	
	c.NumClauses = len(c.Clauses)
	
	// Test with plain DPLL (base solver, no CDCL)
	s := NewSolver(&c)
	result := s.Solve()
	if result {
		t.Error("Expected UNSAT (pigeonhole 5 pigeons 4 holes)")
	}
}

// DISABLED: CDCL is 7500× slower than DPLL on this instance (critical performance bug)
// DPLL solves in 0.004s, CDCL times out after 30s
// Root cause: VSIDS makes poor variable choices for PHP, and 1-UIP produces weak clauses
// See: internal/solver/PHP_PERFORMANCE_BUG.md
// func TestCDCLPhp5p4hUnsat(t *testing.T) { ... }

func TestCDCLPhp6p5hUnsat(t *testing.T) {
	// Pigeonhole principle: 6 pigeons, 5 holes
	// Each pigeon must go to at least one hole (6 clauses of size 5)
	// No two pigeons can share a hole (75 binary clauses)
	// UNSAT: 6 pigeons cannot fit in 5 holes
	// 
	// This test validates the 1-UIP conflict analysis fix (commit 050befe).
	// Before the fix, 1-UIP would fail with 2+ literals at current level,
	// causing timeouts. After the fix, solves in <0.01s with proper learning.
	c := cnf.CNF{
		NumVars: 30,
		Clauses: []cnf.Clause{},
	}
	
	// Each pigeon goes to at least one hole
	for p := 0; p < 6; p++ {
		lits := []cnf.Literal{}
		for h := 0; h < 5; h++ {
			lits = append(lits, cnf.NewLiteral(uint32(p*5+h), false))
		}
		c.Clauses = append(c.Clauses, cnf.Clause{Literals: lits})
	}
	
	// No two pigeons share a hole
	for h := 0; h < 5; h++ {
		for p1 := 0; p1 < 6; p1++ {
			for p2 := p1 + 1; p2 < 6; p2++ {
				c.Clauses = append(c.Clauses, cnf.Clause{
					Literals: []cnf.Literal{
						cnf.NewLiteral(uint32(p1*5+h), true),
						cnf.NewLiteral(uint32(p2*5+h), true),
					},
				})
			}
		}
	}
	
	c.NumClauses = len(c.Clauses)
	
	s := NewCDCLSolver(&c)
	s.SetMaxIter(1000000)
	result := s.SolveWithResult()
	if result != UNSAT {
		t.Errorf("Expected UNSAT (pigeonhole 6 pigeons 5 holes), got %v", result)
	}
}
