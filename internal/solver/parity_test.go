package solver

import (
	"math/bits"
	"testing"

	"satience/internal/cnf"
)

func lit(v uint32, neg bool) cnf.Literal { return cnf.NewLiteral(v, neg) }

// xorFamilyClauses returns the CNF clauses encoding the complete parity family
// XOR(vars) == constant: exactly 2^(len(vars)-1) clauses, each forbidding one
// corner whose true-count parity is != constant.
func xorFamilyClauses(vars []uint32, constant bool) []cnf.Clause {
	d := len(vars)
	var out []cnf.Clause
	n := 1 << d
	for i := 0; i < n; i++ {
		if (bits.OnesCount(uint(i))&1 == 1) == constant {
			continue // allowed corner
		}
		lits := make([]cnf.Literal, 0, d)
		for b := 0; b < d; b++ {
			if (i>>b)&1 == 1 {
				lits = append(lits, cnf.NewLiteral(vars[b], true)) // var true -> negate
			} else {
				lits = append(lits, cnf.NewLiteral(vars[b], false))
			}
		}
		out = append(out, cnf.Clause{Literals: lits})
	}
	return out
}

func mustSolver(t *testing.T, clauses []cnf.Clause) *CDCLSolver {
	t.Helper()
	maxVar := uint32(0)
	for _, c := range clauses {
		for _, l := range c.Literals {
			if l.Var() > maxVar {
				maxVar = l.Var()
			}
		}
	}
	numVars := maxVar + 1
	cnfF := cnf.NewCNF(numVars, len(clauses))
	for _, c := range clauses {
		lits := make([]cnf.Literal, len(c.Literals))
		copy(lits, c.Literals)
		cnfF.AddClause(lits, false)
	}
	return NewCDCLSolver(cnfF)
}

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

// TestVerifyParityRows guards the detectParityRows `!p` constant inversion
// (parity.go detectParityRows: parity: !p). verifyParityRows independently
// re-derives each family's XOR constant straight from the clause semantics. A
// row whose constant is correct passes; the SAME family row whose constant is
// inverted (the `!p` sign error) must FAIL — that is exactly the regression
// that would otherwise feed a wrong equation into Gaussian elimination.
func TestVerifyParityRows(t *testing.T) {
	// Family: x⊕y⊕z = 0 over vars 1,2,3.
	clauses := xorFamilyClauses([]uint32{1, 2, 3}, false)
	s := mustSolver(t, clauses)

	rows, ok := s.detectParityRows()
	if !ok || len(rows) != 1 {
		t.Fatalf("expected exactly one detected parity row, got %d (ok=%v)", len(rows), ok)
	}
	// The detector should report the correct (non-inverted) constant false.
	if rows[0].parity {
		t.Fatalf("detector reported wrong base constant (want false)")
	}
	if !s.verifyParityRows(rows) {
		t.Fatalf("verifyParityRows rejected a correct row constant")
	}

	// The `!p` sign error: pass the SAME family but with the constant inverted.
	bad := []parityRow{{vars: []uint32{1, 2, 3}, parity: true}}
	if s.verifyParityRows(bad) {
		t.Fatalf("verifyParityRows did NOT catch an inverted (!p) parity constant")
	}
}

// TestParityIntegratedSAT is a SAT parity (XOR) system. It must solve SAT (never
// a false UNSAT) with a model that verifies against the original clauses. This
// exercises detectParityRows + verifyParityRows + Gaussian insertion through the
// real SolveWithResult path.
func TestParityIntegratedSAT(t *testing.T) {
	// x⊕y⊕z=0 and x⊕y⊕w=0  =>  z=w, satisfiable (e.g. all zero).
	clauses := xorFamilyClauses([]uint32{0, 1, 2}, false)
	clauses = append(clauses, xorFamilyClauses([]uint32{0, 1, 3}, false)...)
	s := mustSolver(t, clauses)

	res := s.SolveWithResult()
	if res != SAT {
		t.Fatalf("satisfiable parity system returned %v, expected SAT", res)
	}
	if err := VerifySolution(s.cnf, s.assignments, false); err != nil {
		t.Fatalf("SAT parity model does not verify: %v", err)
	}
}

// TestParityIntegratedUNSAT is an odd-cycle parity system: x⊕y⊕z=0, y⊕z⊕w=0
// imply x⊕w=0, which contradicts x⊕w=1 (encoded as the two clauses (x∨w),
// (¬x∨¬w)). It must solve UNSAT.
func TestParityIntegratedUNSAT(t *testing.T) {
	clauses := xorFamilyClauses([]uint32{0, 1, 2}, false) // x⊕y⊕z=0
	clauses = append(clauses, xorFamilyClauses([]uint32{1, 2, 3}, false)...) // y⊕z⊕w=0
	// x⊕w = 1 : forbid x==w with two binary clauses.
	clauses = append(clauses,
		cnf.Clause{Literals: []cnf.Literal{lit(0, false), lit(3, false)}}, // x∨w
		cnf.Clause{Literals: []cnf.Literal{lit(0, true), lit(3, true)}},   // ¬x∨¬w
	)
	s := mustSolver(t, clauses)

	res := s.SolveWithResult()
	if res != UNSAT {
		t.Fatalf("unsatisfiable parity system returned %v, expected UNSAT", res)
	}
}
