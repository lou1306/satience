//go:build debug
// +build debug

package solver

import "satience/internal/cnf"

// verifyLearnedClause checks that a learned clause is sound.
// Checks 1-2 (all assigned, all false) always run. Check 3 (exactly 1 literal
// at current level) is skipped when allowMultipleAtCurrentLevel is true, which
// is the case for NON-CONVERGE clauses that are sound but non-asserting.
func (s *CDCLSolver) verifyLearnedClause(learnedLits []cnf.Literal, allowMultipleAtCurrentLevel bool) bool {
	if len(learnedLits) == 0 {
		return true
	}

	for _, lit := range learnedLits {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level < 0 {
			s.Log("c [SOUNDNESS BUG] Learned clause has unassigned literal: conflict=%d, var=%d\n",
				s.conflicts, varIdx+1)
			return false
		}
	}

	for _, lit := range learnedLits {
		varIdx := lit.Var()
		litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
		if litTrue {
			s.Log("c [SOUNDNESS BUG] Learned clause has TRUE literal: conflict=%d, var=%d\n",
				s.conflicts, varIdx+1)
			return false
		}
	}

	if !allowMultipleAtCurrentLevel {
		literalsAtCurrentLevel := 0
		for _, lit := range learnedLits {
			if int(s.assignments[lit.Var()].Level) == s.level {
				literalsAtCurrentLevel++
			}
		}
		if literalsAtCurrentLevel != 1 {
			s.Log("c [SOUNDNESS BUG] 1-UIP violation: conflict=%d, level=%d, literals_at_level=%d (expected exactly 1)\n",
				s.conflicts, s.level, literalsAtCurrentLevel)
			return false
		}
	}

	return true
}
