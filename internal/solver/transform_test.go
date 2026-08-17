package solver

import (
	"math/rand"
	"satience/internal/cnf"
	"testing"
)

// copyCNF creates a deep copy of a CNF formula.
func copyCNF(c *cnf.CNF) *cnf.CNF {
	out := &cnf.CNF{
		NumVars:    c.NumVars,
		NumClauses: c.NumClauses,
		Clauses:    make([]cnf.Clause, len(c.Clauses)),
	}
	for i, clause := range c.Clauses {
		out.Clauses[i] = cnf.Clause{
			Literals: make([]cnf.Literal, len(clause.Literals)),
			Learned:  clause.Learned,
		}
		copy(out.Clauses[i].Literals, clause.Literals)
	}
	return out
}

// applyTransforms applies satisfiability-preserving transformations to a CNF
// formula, controlled by a random seed. The result is guaranteed to have the
// same satisfiability as the original.
func applyTransforms(c *cnf.CNF, seed int64) *cnf.CNF {
	rng := rand.New(rand.NewSource(seed))
	result := copyCNF(c)

	// 1. Variable permutation (bijection)
	perm := make([]uint32, result.NumVars)
	for i := range perm {
		perm[i] = uint32(i)
	}
	rng.Shuffle(len(perm), func(i, j int) {
		perm[i], perm[j] = perm[j], perm[i]
	})
	for i := range result.Clauses {
		for j, lit := range result.Clauses[i].Literals {
			newVar := perm[lit.Var()]
			result.Clauses[i].Literals[j] = cnf.NewLiteral(newVar, lit.IsNegated())
		}
	}

	// 2. Polarity flip for a random subset of variables
	flip := make([]bool, result.NumVars)
	for i := range flip {
		flip[i] = rng.Intn(2) == 0
	}
	for i := range result.Clauses {
		for j, lit := range result.Clauses[i].Literals {
			if flip[lit.Var()] {
				result.Clauses[i].Literals[j] = lit.Negate()
			}
		}
	}

	// 3. Clause reordering
	rng.Shuffle(len(result.Clauses), func(i, j int) {
		result.Clauses[i], result.Clauses[j] = result.Clauses[j], result.Clauses[i]
	})

	// 4. Literal reordering within each clause
	for i := range result.Clauses {
		lits := result.Clauses[i].Literals
		rng.Shuffle(len(lits), func(a, b int) {
			lits[a], lits[b] = lits[b], lits[a]
		})
	}

	// 5. Add tautological clauses (x ∨ ¬x)
	nTaut := rng.Intn(5)
	for i := 0; i < nTaut; i++ {
		if result.NumVars == 0 {
			break
		}
		v := uint32(rng.Intn(int(result.NumVars)))
		result.Clauses = append(result.Clauses, cnf.Clause{
			Literals: []cnf.Literal{
				cnf.NewLiteral(v, false),
				cnf.NewLiteral(v, true),
			},
		})
		result.NumClauses++
	}

	// 6. Duplicate a random existing clause
	if len(result.Clauses) > 0 && rng.Intn(2) == 0 {
		src := result.Clauses[rng.Intn(len(result.Clauses))]
		dup := cnf.Clause{
			Literals: make([]cnf.Literal, len(src.Literals)),
			Learned:  src.Learned,
		}
		copy(dup.Literals, src.Literals)
		result.Clauses = append(result.Clauses, dup)
		result.NumClauses++
	}

	return result
}

// testFormulas provides known SAT/UNSAT formulas for transformation testing.
var testFormulas = []struct {
	name    string
	sat     bool
	formula cnf.CNF
}{
	{
		name: "simple_sat",
		sat:  true,
		formula: cnf.CNF{
			NumVars: 3,
			Clauses: []cnf.Clause{
				newClause(1, 2),
				newClause(-1, 3),
			},
			NumClauses: 2,
		},
	},
	{
		name: "simple_unsat",
		sat:  false,
		formula: cnf.CNF{
			NumVars: 1,
			Clauses: []cnf.Clause{
				newClause(1),
				newClause(-1),
			},
			NumClauses: 2,
		},
	},
	{
		name: "3sat_sat",
		sat:  true,
		formula: cnf.CNF{
			NumVars: 4,
			Clauses: []cnf.Clause{
				newClause(1, 2, 3),
				newClause(1, -2, 4),
				newClause(-1, 2, -4),
				newClause(-1, -2, -3),
			},
			NumClauses: 4,
		},
	},
	{
		name: "3sat_unsat",
		sat:  false,
		formula: cnf.CNF{
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
		},
	},
	{
		name: "php_unsat",
		sat:  false,
		formula: cnf.CNF{
			NumVars: 6,
			Clauses: []cnf.Clause{
				newClause(1, 2),
				newClause(3, 4),
				newClause(5, 6),
				newClause(-1, -3),
				newClause(-1, -5),
				newClause(-3, -5),
				newClause(-2, -4),
				newClause(-2, -6),
				newClause(-4, -6),
			},
			NumClauses: 9,
		},
	},
	{
		name: "chain_unsat",
		sat:  false,
		formula: cnf.CNF{
			NumVars: 5,
			Clauses: []cnf.Clause{
				newClause(1),
				newClause(-1, 2),
				newClause(-2, 3),
				newClause(-3, 4),
				newClause(-4, 5),
				newClause(-5),
			},
			NumClauses: 6,
		},
	},
	{
		name: "empty_sat",
		sat:  true,
		formula: cnf.CNF{
			NumVars:    0,
			Clauses:    []cnf.Clause{},
			NumClauses: 0,
		},
	},
	{
		name: "tautology_sat",
		sat:  true,
		formula: cnf.CNF{
			NumVars: 2,
			Clauses: []cnf.Clause{
				newClause(1, -1),
			},
			NumClauses: 1,
		},
	},
}

// FuzzTransformInvariance tests that satisfiability-preserving transformations
// do not change the solver's verdict. Each seed produces a random combination
// of: variable permutation, polarity flip, clause reordering, literal
// reordering, tautological clause addition, and clause duplication.
func FuzzTransformInvariance(f *testing.F) {
	// Seeds: enough variety to cover different transformation combinations
	for i := int64(0); i < 50; i++ {
		f.Add(i)
	}

	f.Fuzz(func(t *testing.T, seed int64) {
		for _, tf := range testFormulas {
			transformed := applyTransforms(&tf.formula, seed)
			s := NewCDCLSolver(transformed)
			result := s.Solve()
			if result != tf.sat {
				t.Errorf("%s seed=%d: expected sat=%v, got sat=%v",
					tf.name, seed, tf.sat, result)
			}
		}
	})
}
