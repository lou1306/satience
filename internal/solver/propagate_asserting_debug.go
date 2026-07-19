//go:build debug
// +build debug

package solver

import (
	"fmt"

	"satience/internal/cnf"
)

// verifyAssertingInvariant checks that the asserting-clause invariant holds
// after backjump, before propagateAssertingLiteral assigns literals[0] directly.
//
// Asserting-clause invariant (established by learnClause):
//   - Position 0 = UIP (at s.level > bjLevel) → unassigned after backjump
//   - All other literals at levels <= bjLevel → stay assigned (false)
//
// After backtrack(bjLevel), exactly one literal (position 0) should be
// unassigned and all others should be false. If this is violated, the
// fast-path direct assignment in propagateAssertingLiteral would be
// unsound, so we panic to catch the bug during testing.
//
// Enable with: go build -tags debug
func verifyAssertingInvariant(s *CDCLSolver, learnedIdx int, literals []cnf.Literal) {
	unassignedCount := 0
	for _, lit := range literals {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level < 0 {
			unassignedCount++
		} else {
			litTrue := lit.IsNegated() != s.assignments[varIdx].Value
			if litTrue {
				panic(fmt.Sprintf(
					"[SOUNDNESS BUG] Asserting clause %d has a TRUE literal (var %d) after backjump — invariant violated",
					learnedIdx, varIdx+1))
			}
		}
	}
	if unassignedCount != 1 {
		panic(fmt.Sprintf(
			"[SOUNDNESS BUG] Asserting clause %d has %d unassigned literals after backjump (expected 1) — invariant violated",
			learnedIdx, unassignedCount))
	}
}
