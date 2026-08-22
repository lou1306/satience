package solver

import (
	"testing"

	"satience/internal/cnf"
)

func mkClause(lits ...cnf.Literal) cnf.Clause {
	return cnf.Clause{Literals: lits}
}

// A literal l is hidden in C=(l∨a∨b) when (a∨b) is already implied by the rest
// of the formula. Here clause0=(a∨b) makes l in clause1 removable.
func TestHiddenLiteralRemoval(t *testing.T) {
	s := &CDCLSolver{hleEnabled: true, hleMaxSize: 5, hleBudget: 1000}
	s.cnf = &cnf.CNF{NumVars: 3, NumClauses: 3}
	a, b, x := lit(0, false), lit(1, false), lit(2, false)
	s.cnf.Clauses = []cnf.Clause{
		mkClause(a, b),        // (a∨b) implies the body
		mkClause(x, a, b),     // (x∨a∨b) -> x is hidden
		mkClause(lit(0, true), lit(1, true), lit(2, true)), // unrelated filler
	}
	removed := s.hiddenLiteralElimination()
	if removed < 1 {
		t.Fatalf("expected >=1 hidden literal removed, got %d", removed)
	}
	size3 := 0
	for _, c := range s.cnf.Clauses {
		if len(c.Literals) == 3 {
			size3++
		}
	}
	// Only the unrelated filler (¬a∨¬b∨¬x) may remain size-3; (x∨a∨b) → (a∨b).
	if size3 != 1 {
		t.Errorf("expected exactly 1 size-3 clause (filler) to remain, got %d", size3)
	}
}

// If the body is NOT implied, no literal is hidden and nothing is removed.
func TestHiddenLiteralNoRemoval(t *testing.T) {
	s := &CDCLSolver{hleEnabled: true, hleMaxSize: 5, hleBudget: 1000}
	s.cnf = &cnf.CNF{NumVars: 3, NumClauses: 3}
	a, b, x := lit(0, false), lit(1, false), lit(2, false)
	s.cnf.Clauses = []cnf.Clause{
		mkClause(x, a, b),
		mkClause(lit(0, false), lit(2, true)), // unrelated, does not imply (a∨b)
	}
	removed := s.hiddenLiteralElimination()
	if removed != 0 {
		t.Fatalf("expected 0 removals (body not implied), got %d", removed)
	}
}

// Disabled gate must be a no-op.
func TestHiddenLiteralDisabled(t *testing.T) {
	s := &CDCLSolver{hleEnabled: false, hleMaxSize: 5, hleBudget: 1000}
	s.cnf = &cnf.CNF{NumVars: 3, NumClauses: 3}
	a, b, x := lit(0, false), lit(1, false), lit(2, false)
	s.cnf.Clauses = []cnf.Clause{
		mkClause(a, b),
		mkClause(x, a, b),
	}
	if r := s.hiddenLiteralElimination(); r != 0 {
		t.Fatalf("disabled HLE removed %d", r)
	}
}
