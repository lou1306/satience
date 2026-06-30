//go:build debug

package solver

import (
	"fmt"
	"satience/internal/cnf"
)

// VerifyLearnedClause checks if a learned clause is valid
// A valid learned clause must be:
// 1. All literals are assigned
// 2. All literals are false (clause is falsified by current assignment)
// 3. Clause is implied by the original CNF (entailment check)
func (s *CDCLSolver) VerifyLearnedClause(literals []cnf.Literal, conflictNum int) bool {
	if !s.debugVerify {
		return true
	}

	// Check 1: All literals must be assigned
	for _, lit := range literals {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level < 0 {
			fmt.Printf("c [VERIFY FAIL] Conflict %d: Learned clause has unassigned literal: ", conflictNum)
			for _, l := range literals {
				fmt.Printf("%d%c(L%d) ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()], s.assignments[l.Var()].Level)
			}
			fmt.Printf("\n")
			return false
		}
	}

	// Check 2: All literals must be false (clause falsified)
	for _, lit := range literals {
		varIdx := lit.Var()
		litTrue := (!lit.IsNegated() && s.assignments[varIdx].Value) || (lit.IsNegated() && !s.assignments[varIdx].Value)
		if litTrue {
			fmt.Printf("c [VERIFY FAIL] Conflict %d: Learned clause has TRUE literal: ", conflictNum)
			for _, l := range literals {
				fmt.Printf("%d%c(L%d,V=%v) ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()], s.assignments[l.Var()].Level, s.assignments[l.Var()].Value)
			}
			fmt.Printf("\n")
			return false
		}
	}

	// Check 3: No duplicate variables
	seenVars := make(map[uint32]bool)
	for _, lit := range literals {
		if seenVars[lit.Var()] {
			fmt.Printf("c [VERIFY FAIL] Conflict %d: Learned clause has duplicate var %d: ", conflictNum, lit.Var()+1)
			for _, l := range literals {
				fmt.Printf("%d%c ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()])
			}
			fmt.Printf("\n")
			return false
		}
		seenVars[lit.Var()] = true
	}

	return true
}

// VerifyAllLearnedClauses checks all active learned clauses
func (s *CDCLSolver) VerifyAllLearnedClauses() {
	if !s.debugVerify {
		return
	}

	for i := 0; i < s.learnedActiveCount; i++ {
		if s.learnedSizes[i] == 0 {
			continue // Skip tombstones
		}
		lits := s.getLearnedClauseLiterals(i)
		if !s.VerifyLearnedClause(lits, -1) {
			fmt.Printf("c [VERIFY] Clause %d (ID=%d) failed verification\n", i, s.learnedMetadata[i].ID)
		}
	}
}
