package solver

// hle.go — Hidden Literal Elimination (P2). Gated: -hle (default off -> bit-identical).
//
// Given a clause C and a literal l in C, l is a HIDDEN literal — removable from C —
// exactly when C\{l} is implied by the rest of the formula (F minus C). Replacing C
// by the (weaker) C\{l} preserves satisfiability in that case:
//
//	F\{C} |= C\{l}  ⟹  C is also implied (C ⊇ C\{l})  ⟹  F ≡ F\{C} ≡ F\{C} ∪ {C\{l}}
//
// Detection via a non-destructive unit-propagation PROBE: assume the negation of every
// OTHER literal of C (¬(C\{l})). If those assumptions alone make the formula (with C
// excluded) inconsistent, then C\{l} is an implicate → remove l. C MUST be excluded
// (skipClause): otherwise the probe merely unit-propagates l through C itself and never
// observes a conflict, detecting nothing.
//
// SOUNDNESS: the probe ignores root unit assignments (only asserts the clause's own
// other literals), which only ever makes us do strictly LESS (conservative, sound); a
// conflict reached purely from the asserted literals is a genuine formula consequence.
// Strengthening is monotone in the database, and occurrence lists are re-read lazily,
// so in-place strengthening mid-pass stays consistent with the occurrence map.
//
// Bounded: only clauses of size in [3, hleMaxSize] are probed and a hard probe budget
// caps total work (probes are the expensive non-watch BCP scans). Only the first
// removable literal per clause is taken. Sound when aborted by the budget (does less).

import "satience/internal/cnf"

// hiddenLiteralElimination scans the original clause database and removes redundant
// (hidden) literals, shrinking clauses. Runs during preprocessing (gated -hle).
// Returns the number of literals removed, or -1 if UNSAT was detected (a unit only
// possible if a clause collapsed; not currently triggered since removals stop at
// size-2, but the contract mirrors other preprocessing passes).
func (s *CDCLSolver) hiddenLiteralElimination() int {
	if !s.hleEnabled || s.hleMaxSize < 3 || s.hleBudget <= 0 {
		return 0
	}
	numVars := int(s.cnf.NumVars)
	if numVars == 0 || s.cnf.NumClauses == 0 {
		return 0
	}
	numLits := numVars * 2

	occ := make([][]int, numLits)
	for ci, clause := range s.cnf.Clauses {
		for _, lit := range clause.Literals {
			occ[cnf.LitToIndex(lit)] = append(occ[cnf.LitToIndex(lit)], ci)
		}
	}

	tmpValue := make([]bool, numVars)
	tmpAssigned := make([]bool, numVars)
	s.probeTrail = s.probeTrail[:0]

	removed := 0
	probes := 0

clauseLoop:
	for ci, clause := range s.cnf.Clauses {
		if probes >= s.hleBudget {
			break clauseLoop
		}
		clauseLits := clause.Literals
		d := len(clauseLits)
		if d < 3 || d > s.hleMaxSize {
			continue
		}

		// Try removing each literal in turn; take the first that is hidden.
		for li := 0; li < d; li++ {
			if probes >= s.hleBudget {
				break clauseLoop
			}

			// Assumptions: assert the negation of every literal EXCEPT clauseLits[li].
			base := len(s.probeTrail)
			for m := 0; m < d; m++ {
				if m == li {
					continue
				}
				litIdx := cnf.LitToIndex(clauseLits[m])
				v := clauseLits[m].Var()
				// ¬lit is true when tmpValue[v] == !(negated of ¬lit). ¬lit index =
				// litIdx^1 flips the neg bit, so make ¬lit true by setting
				// tmpValue[v] = lit.IsNegated() and pushing litIdx^1 onto the trail.
				tmpAssigned[v] = true
				tmpValue[v] = clauseLits[m].IsNegated()
				s.probeTrail = append(s.probeTrail, litIdx^1)
			}
			conflict := s.probePropagate(occ, tmpValue, tmpAssigned, base, ci)
			probes++

			// Restore probe state: clear tmpAssigned for EVERY var assigned during
			// this probe — both the assumption vars and any vars that probePropagate
			// DERIVED via unit propagation. The trail from base..end holds exactly
			// the assigned vars (assumptions followed by derived units). Leaving a
			// derived var marked assigned would leak stale values into the NEXT
			// probe, producing spurious conflicts (unsound removals -> false UNSAT).
			for k := base; k < len(s.probeTrail); k++ {
				tmpAssigned[uint32(s.probeTrail[k])>>1] = false
			}
			s.probeTrail = s.probeTrail[:base]
			if conflict {
				// ¬(C\{li}) inconsistent with F\{C} → C\{li} implied → remove li.
				newLits := removeLiteralAt(clauseLits, li)
				// Guard: don't leave a tautology/duplicate or an empty clause.
				if !hiddenResultUsable(newLits) {
					continue
				}
				s.cnf.Clauses[ci].Literals = newLits
				removed++
				s.hleRemoved++
				break // one removal per clause
			}
		}
	}

	if removed > 0 {
		s.cnf.RebuildLiteralPool()
	}
	return removed
}

// hiddenResultUsable rejects a strengthened clause that would be tautological
// (contains a complementary pair) or contain duplicates — such clauses are handled
// by other preprocessing and are never produced by a clean 3..max clause, but this
// is a defensive guard so a removal can never corrupt the database.
func hiddenResultUsable(lits []cnf.Literal) bool {
	if len(lits) < 2 || len(lits) > 4 { // be conservative
		return false
	}
	seen := make([]bool, 4)
	for i, l := range lits {
		if seen[i] {
			return false
		}
		for j := i - 1; j >= 0; j-- {
			if l.Var() == lits[j].Var() {
				if l.IsNegated() != lits[j].IsNegated() {
					return false // tautology
				}
				return false // duplicate
			}
		}
	}
	return true
}
