package solver

import (
	"testing"

	"satience/internal/cnf"
)

func lit(v uint32, neg bool) cnf.Literal { return cnf.NewLiteral(v, neg) }

// A single 3-XOR gate x⊕y⊕z = 0 is encoded by the 4 clauses forbidding the
// odd-true assignments. Builds s with just those 4 clauses and checks the
// detector returns exactly one parity row over {x,y,z}.
func TestDetectParityRows_ThreeXorGate(t *testing.T) {
	// vars 1,2,3 ; forbid odd assignments -> parity family p=odd
	clauses := [][3]cnf.Literal{
		{lit(1, false), lit(2, false), lit(3, true)}, // x∨y∨¬z
		{lit(1, false), lit(2, true), lit(3, false)}, // x∨¬y∨z
		{lit(1, true), lit(2, false), lit(3, false)}, // ¬x∨y∨z
		{lit(1, true), lit(2, true), lit(3, true)},   // ¬x∨¬y∨¬z
	}
	s := &CDCLSolver{parityEnabled: true, parityMaxArity: 6}
	s.cnf = &cnf.CNF{}
	for _, c := range clauses {
		var lits []cnf.Literal
		lits = append(lits, c[:]...)
		s.cnf.Clauses = append(s.cnf.Clauses, cnf.Clause{Literals: lits})
	}
	rows, ok := s.detectParityRows()
	if !ok {
		t.Fatal("detectParityRows returned UNSAT")
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 parity row, got %d", len(rows))
	}
	if len(rows[0].vars) != 3 {
		t.Fatalf("expected support size 3, got %d", len(rows[0].vars))
	}
}

// Gaussian: rows x⊕y=0 and y⊕z=1 must yield binary equivalences and no UNSAT.
func TestGaussDerivesBinaries(t *testing.T) {
	g := newGaussState()
	g.insert([]uint32{1, 2}, false) // x⊕y=0 -> x≡y
	g.insert([]uint32{2, 3}, true)  // y⊕z=1 -> y≡¬z
	if g.unsat {
		t.Fatal("unexpected UNSAT")
	}
	g.finalize()
	if len(g.units) != 0 {
		t.Fatalf("expected 0 units, got %d", len(g.units))
	}
	if len(g.binars) == 0 {
		t.Fatal("expected derived binary clauses")
	}
	// x≡y must appear: (x∨¬y) or (¬x∨y)
	gotXY := false
	for i := 0; i < len(g.binars); i += 2 {
		a, b := g.binars[i], g.binars[i+1]
		// pair (x∨¬y),(¬x∨y)
		if (a[0].Var() == 1 && a[1].Var() == 2) || (a[0].Var() == 2 && a[1].Var() == 1) {
			gotXY = true
		}
		_ = b
	}
	if !gotXY {
		t.Fatal("expected a binary clause involving vars {1,2}")
	}
}

// Gaussian detects a parity contradiction only when genuinely inconsistent.
func TestGaussDetectUnsat(t *testing.T) {
	g := newGaussState()
	g.insert([]uint32{1, 2, 3}, false)
	g.insert([]uint32{1, 2, 3}, true) // same rows, opposite parity -> 0 = parity(1)
	if !g.unsat {
		t.Fatal("expected UNSAT detection from contradictory parity rows")
	}
}

// Gaussian consistency: x⊕y⊕z=0, x=1, y=0 must force z=1 (a unit).
func TestGaussDerivesUnit(t *testing.T) {
	g := newGaussState()
	g.insert([]uint32{1, 2, 3}, false) // x⊕y⊕z = 0
	g.insert([]uint32{1}, false)       // x = 0  -> lit ¬1
	g.insert([]uint32{2}, true)        // y = 1  -> lit 2
	// expect z = 1 (unit lit 3)
	if g.unsat {
		t.Fatal("unexpected UNSAT")
	}
	found3 := false
	g.finalize()
	for _, u := range g.units {
		if u.Var() == 3 {
			found3 = true
		}
	}
	if !found3 {
		t.Fatalf("expected a unit on var 3; units=%v", g.units)
	}
}
