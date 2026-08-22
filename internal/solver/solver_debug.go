//go:build debug
// +build debug

package solver

import (
	"fmt"

	"satience/internal/cnf"
)

// verifyClauseIndices validates that all clause references are consistent after deletion
// This is a DEBUG-ONLY verification function for validating swap-remove correctness
// Enabled with: go build -tags debug
func verifyClauseIndices(s *CDCLSolver) bool {
	errors := 0

	// Check 1: All reasons point to valid, non-deleted clauses
	for varIdx, asg := range s.assignments {
		impIdx := asg.Reason
		if impIdx <= -5 {
			learnedIdx := -impIdx - 5
			if int(learnedIdx) >= s.learnedCapacity {
				fmt.Printf("c [VERIFY ERROR] Implication var %d -> clause %d (>= learnedCapacity %d)\n",
					varIdx+1, learnedIdx, s.learnedCapacity)
				errors++
			} else if s.learnedLoc[learnedIdx].Size == 0 {
				fmt.Printf("c [VERIFY ERROR] Implication var %d -> deleted clause %d (size=0)\n",
					varIdx+1, learnedIdx)
				errors++
			}
		}
	}

	// Check 2: All watch lists reference existing clauses
	for litIdx := range s.watchLists {
		for i, watch := range s.watchLists[litIdx] {
			if watch.ClauseIdx < 0 {
				learnedIdx := -watch.ClauseIdx - 1
				if int(learnedIdx) >= s.learnedCapacity {
					fmt.Printf("c [VERIFY ERROR] Watch[%d][%d] -> clause %d (>= learnedCapacity %d)\n",
						litIdx, i, learnedIdx, s.learnedCapacity)
					errors++
				} else if s.learnedLoc[learnedIdx].Size == 0 {
					fmt.Printf("c [VERIFY ERROR] Watch[%d][%d] -> deleted clause %d (size=0)\n",
						litIdx, i, learnedIdx)
					errors++
				}
			}
		}
	}

	// Check 3 (removed): Binary watch lists reference existing clauses
	// The CDCLSolver has no watchListsBinary field; this check referenced a
	// nonexistent struct member and prevented `make debug` from compiling.

	// Check 4: unitLearnedList only contains size=1 clauses
	for i, learnedIdx := range s.unitLearnedList {
		if learnedIdx >= s.learnedCapacity {
			fmt.Printf("c [VERIFY ERROR] unitLearnedList[%d] -> clause %d (>= learnedCapacity %d)\n",
				i, learnedIdx, s.learnedCapacity)
			errors++
		} else if s.learnedLoc[learnedIdx].Size != 1 {
			fmt.Printf("c [VERIFY ERROR] unitLearnedList[%d] -> clause %d (size=%d, expected 1)\n",
				i, learnedIdx, s.learnedLoc[learnedIdx].Size)
			errors++
		}
	}

	if errors > 0 {
		fmt.Printf("c [VERIFY FAILED] %d errors found in clause index verification\n", errors)
		return false
	}

	if s.verbose {
		fmt.Printf("c [VERIFY OK] Clause indices verified: learned=%d, capacity=%d\n",
			s.learnedActiveCount, s.learnedCapacity)
	}
	return true
}

// verifyOriginalWatchNotBothFalse checks the watched-literal invariant for an
// ORIGINAL clause add. Original clauses are watched at level 0 (all variables
// unassigned), so both chosen watches must never be false. If they were, the
// clause would be watched with two false literals - a WB invariant violation
// that could desync propagation. Learned-clause adds legitimately watch two
// false literals pre-backjump (one turns true post-backjump), so this check is
// intentionally scoped to original clauses only. DEBUG-ONLY: no-op in release.
func verifyOriginalWatchNotBothFalse(s *CDCLSolver, literals []cnf.Literal, watch0, watch1 int) {
	for _, wi := range []int{watch0, watch1} {
		if wi < 0 || wi >= len(literals) {
			fmt.Printf("c [VERIFY ERROR] original watch index %d out of range (len=%d)\n", wi, len(literals))
			return
		}
	}
	falseCount := 0
	for _, wi := range []int{watch0, watch1} {
		lit := literals[wi]
		vi := lit.Var()
		asg := s.assignments[vi]
		if asg.Level >= 0 && (lit.IsNegated() != asg.Value) {
			falseCount++
		}
	}
	if falseCount == 2 {
		fmt.Printf("c [VERIFY ERROR] original clause watching two false literals (pos %d,%d)\n", watch0, watch1)
	}
}
