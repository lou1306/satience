package solver

import (
	"fmt"
	"strings"
	"testing"

	"satience/internal/cnf"
	"satience/internal/parser"
)

// bruteForceEntailed reports whether clause C is a logical consequence of the
// (small) formula F: C is entailed by F iff every satisfying assignment of F
// also satisfies C. This is computed by exhaustive enumeration over the (tiny)
// variable space — deliberately independent of the solver under test, so we do
// not trust the very code path we are checking.
func bruteForceEntailed(F *cnf.CNF, C []cnf.Literal) bool {
	numVars := int(F.NumVars)
	for mask := 0; mask < (1 << numVars); mask++ {
		satF := true
		for _, cl := range F.Clauses {
			ok := false
			for _, lit := range cl.Literals {
				assigned := (mask>>int(lit.Var()))&1 == 1
				if lit.IsNegated() != assigned {
					ok = true
					break
				}
			}
			if !ok {
				satF = false
				break
			}
		}
		if !satF {
			continue
		}
		clauseSat := false
		for _, lit := range C {
			assigned := (mask>>int(lit.Var()))&1 == 1
			if lit.IsNegated() != assigned {
				clauseSat = true
				break
			}
		}
		if !clauseSat {
			// F satisfied but C false => C is NOT entailed by F.
			return false
		}
	}
	return true
}

// TestOneUIPFallbackIsFailSafe guards the 1-UIP non-convergence path in
// runOneUIPResolution (internal/solver/solver_cdcl.go). When resolution fails
// to converge, that path must NOT fabricate a 1-UIP by dropping the remaining
// current-level literals: the dropped literals are only implied if every reason
// clause is consistent, but non-convergence is itself the signature of
// *inconsistent* reason clauses, so pruning can emit an un-entailed learned
// clause that later rules out a satisfying assignment and drives a false UNSAT.
//
// The correct behavior is fail-safe: leave the conflict un-learned and backjump
// conservatively, so no clause enters the database and nothing is over-committed
// toward UNSAT. This test fabricates the forbidden state (two DECISION literals
// at the same decision level) and asserts those fail-safe invariants.
func TestOneUIPFallbackIsFailSafe(t *testing.T) {
	// Satisfiable formula: (¬a ∨ ¬b). A satisfying model: a=false, b=true.
	const dimacs = "p cnf 2 1\n-1 -2 0\n"
	formula, err := parser.Parse(strings.NewReader(dimacs))
	if err != nil {
		t.Fatal(err)
	}
	s := NewCDCLSolver(formula)

	// Fabricate an "inconsistent reason clause" search state: two DECISION
	// literals at the same decision level (level 1). This violates the
	// one-decision-per-level invariant and is the kind of inconsistent state
	// that makes 1-UIP fail to converge.
	a, b := uint32(0), uint32(1) // a = var 1, b = var 2
	s.level = 1
	s.assignments[a] = Assignment{Value: true, Level: 1, Reason: -1, SavedPhase: true}
	s.assignments[b] = Assignment{Value: true, Level: 1, Reason: -1, SavedPhase: true}
	s.litTrue[0] = true  // a true
	s.litTrue[1] = false // ¬a false
	s.litTrue[2] = true  // b true
	s.litTrue[3] = false // ¬b false
	s.trail = []uint32{a, b}
	s.trailHead = []int{0, 0} // trailHead[1] = 0: level 1 begins at trail position 0

	// Conflict clause (¬a ∨ ¬b): all literals false under the assignment.
	conflictLits := []cnf.Literal{
		cnf.NewLiteral(a, true),
		cnf.NewLiteral(b, true),
	}

	bjLevel := s.learnClause(conflictLits)

	// Fail-safe invariants:
	//  1. No clause was stored (conflict analysis never over-commits here).
	if s.lastLearnedClauseIdx >= 0 {
		stored := s.getLearnedClauseLiterals(s.lastLearnedClauseIdx)
		t.Fatalf("UNSOUND: conflict analysis stored learned clause %v despite "+
			"non-convergent (inconsistent) reasons; it would over-commit the "+
			"search toward a false UNSAT", cnfLitsToDimacs(stored))
	}
	//  2. The conflict was NOT declared UNSAT.
	if s.emptyClauseFound {
		t.Fatalf("non-convergent conflict was misreported as UNSAT")
	}
	//  3. The backjump target is safe: undoes the current decision level.
	if bjLevel < 0 || bjLevel >= s.level {
		t.Fatalf("backjump level %d out of range for level %d", bjLevel, s.level)
	}

	// Belt-and-suspenders: whatever clause would have been assembled from the
	// partial analysis must (independently) be entailed by the formula, so a
	// regression that resumes learning cannot silently ship an un-entailed one.
	var residual []cnf.Literal
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] && s.assignments[varIdx].Level > 0 {
			residual = append(residual, cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx]))
		}
	}
	if len(residual) > 0 && !bruteForceEntailed(formula, residual) {
		t.Fatalf("UNSOUND: residual conflict-analysis clause %v is NOT a logical "+
			"consequence of the formula", cnfLitsToDimacs(residual))
	}
}

func cnfLitsToDimacs(lits []cnf.Literal) string {
	var sb strings.Builder
	for i, l := range lits {
		if i > 0 {
			sb.WriteString(" ")
		}
		if l.IsNegated() {
			sb.WriteString("-")
		}
		sb.WriteString(fmt.Sprint(l.Var() + 1))
	}
	sb.WriteString(" 0")
	return sb.String()
}
