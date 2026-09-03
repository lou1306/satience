package solver

import (
	"testing"

	"satience/internal/cnf"
)

// cat concatenates clause groups into one slice.
func cat(groups ...[]cnf.Clause) []cnf.Clause {
	var out []cnf.Clause
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// TestBVEAssignedVarModelSoundness exercises the preprocessing path where
// boundedVarElimination(false) may eliminate an already-assigned variable (pure
// literal / unit / parity-derived unit). BVE is sound under elimination and
// reconstructEliminatedVars overwrites the eliminated var with the resolution-
// determined value, but a regression here could yield a wrong model or false
// UNSAT. Belt-and-suspenders: for every case (except a genuine UNSAT), the
// returned model must satisfy ALL the ORIGINAL clauses. We keep a deep copy of
// the input clauses and check the entire model, not just a subset.
func TestBVEAssignedVarModelSoundness(t *testing.T) {
	// Each case mixes root pure-literal/unit assignments (which preprocessing
	// forces at level 0 before BVE) with VE-friendly structure, so BVE is likely
	// to interact with assigned variables on the real SolveWithResult path.
	cases := []struct {
		name    string
		clauses []cnf.Clause
	}{
		{
			// Pure negative var0 (PLE assigns 0=false at level 0), then an
			// XOR/parity family over {1,2,3} that parity+VE chew on.
			name: "pure-plus-parity",
			clauses: append(
				[]cnf.Clause{
					{Literals: []cnf.Literal{lit(0, true), lit(1, false)}}, // ¬0 ∨ 1
					{Literals: []cnf.Literal{lit(0, true), lit(2, false)}}, // ¬0 ∨ 2
				},
				xorFamilyClauses([]uint32{1, 2, 3}, false)..., // 1⊕2⊕3 = 0
			),
		},
		{
			// A root unit (0) plus parity over {1,2,3}.
			name: "unit-plus-parity",
			clauses: append(
				[]cnf.Clause{{Literals: []cnf.Literal{lit(0, false)}}}, // 0 (unit)
				xorFamilyClauses([]uint32{1, 2, 3}, false)...,
			),
		},
		{
			// Two parity equations with a shared var + a unit: forces determinism
			// and an intersection for Gaussian/VE to chew on.
			name: "two-parity-plus-unit",
			clauses: cat(
				[]cnf.Clause{{Literals: []cnf.Literal{lit(0, false)}}}, // 0 (unit)
				xorFamilyClauses([]uint32{1, 2, 3}, false),
				xorFamilyClauses([]uint32{3, 4, 5}, false),
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustSolver(t, tc.clauses)
			res := s.SolveWithResult()
			if res == UNSAT {
				return // genuinely UNSAT is acceptable; soundness check applies to SAT
			}
			if res != SAT {
				t.Fatalf("expected SAT, got %v", res)
			}
			assignments := s.GetAssignments()
			for _, c := range tc.clauses {
				sat := false
				for _, l := range c.Literals {
					v := l.Var()
					if int(v) >= len(assignments) {
						t.Fatalf("model missing variable %d", v+1)
					}
					a := assignments[v]
					if a.Level < 0 {
						t.Fatalf("var %d unassigned in model", v+1)
					}
					if l.IsNegated() != a.Value {
						sat = true
						break
					}
				}
				if !sat {
					t.Fatalf("unsatisfied original clause in returned model: %v", c.Literals)
				}
			}
		})
	}
}
