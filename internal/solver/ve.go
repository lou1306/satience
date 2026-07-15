package solver

import (
	"satience/internal/cnf"
)

// eliminatedVar stores a variable eliminated by BVE and the clauses that
// contained it, for model reconstruction after SAT.
type eliminatedVar struct {
	varIdx  uint32
	clauses [][]cnf.Literal
}

// maxVEPairCost is the maximum posCount*negCount product for a variable to
// be considered for elimination. Variables with higher cost produce too
// many resolvents relative to the clauses they remove.
const maxVEPairCost = 1000

// boundedVarElimination performs bounded variable elimination (BVE) on the
// original clause database. For each eliminated variable x, all clauses
// containing x and ¬x are replaced by their pairwise resolvents (standard
// Davis-Putnam elimination). Tautological resolvents are discarded; the
// clause-count gate ensures elimination only when it reduces total clauses.
//
// This is the standard sound VE algorithm — NOT the banned "pos=1 definitional"
// variant. Model reconstruction is by trying x=true then x=false (proven
// correct by the resolution argument).
//
// veBudget=0 means unlimited (small instances). For large instances, the
// budget bounds total resolvents generated to prevent explosion.
// Returns number of eliminated variables, or -1 if UNSAT detected (empty resolvent).
func (s *CDCLSolver) boundedVarElimination() int {
	numVars := int(s.cnf.NumVars)
	if numVars == 0 || s.cnf.NumClauses == 0 {
		return 0
	}

	numLits := numVars * 2

	// Reusable allocations (hoisted outside the loop to avoid per-iteration GC pressure)
	seenLit := make([]bool, numLits)
	var touched []int

	// removed[] accumulates across ALL iterations — compacted once at the end.
	// Grows as new resolvents are added; new entries default to false.
	removed := make([]bool, s.cnf.NumClauses)

	// Build occurrence lists ONCE — maintained incrementally across eliminations.
	// Occurrence lists contain stale entries (removed clauses); these are filtered
	// by checking removed[ci] during resolvent generation. posCount/negCount track
	// active (non-removed) counts for O(1) cheapest-variable scanning.
	posOcc := make([][]int, numVars)
	negOcc := make([][]int, numVars)
	posCount := make([]int, numVars)
	negCount := make([]int, numVars)
	for i, clause := range s.cnf.Clauses {
		for _, lit := range clause.Literals {
			v := lit.Var()
			if lit.IsNegated() {
				negOcc[v] = append(negOcc[v], i)
				negCount[v]++
			} else {
				posOcc[v] = append(posOcc[v], i)
				posCount[v]++
			}
		}
	}

	// Track eliminated variables to skip in the cheapest scan.
	eliminatedFlag := make([]bool, numVars)

	// Bucket queue: buckets[cost] = list of vars with that cost.
	// cost = posCount[v] * negCount[v], bounded by maxVEPairCost.
	// Lazy deletion: stale entries (eliminated or cost changed) are skipped on pop.
	// When counts change, push new entries to the appropriate bucket.
	buckets := make([][]uint32, maxVEPairCost+1)
	for v := uint32(0); v < uint32(numVars); v++ {
		pc := posCount[v]
		nc := negCount[v]
		if pc > 0 && nc > 0 {
			cost := pc * nc
			if cost <= maxVEPairCost {
				buckets[cost] = append(buckets[cost], v)
			}
		}
	}

	// Reusable scratch space for deduplicating affected variables after count updates
	seenVar := make([]bool, numVars)
	var touchedVars []int

	totalResolvents := 0
	eliminated := 0

	for {
		// Find the cheapest eliminatable variable using the bucket queue.
		// Scan from bucket 1 upward; pop from the end, skipping stale entries.
		v := uint32(0)
		found := false
		for c := 1; c <= maxVEPairCost; c++ {
			for len(buckets[c]) > 0 {
				cand := buckets[c][len(buckets[c])-1]
				buckets[c] = buckets[c][:len(buckets[c])-1]

				if eliminatedFlag[cand] {
					continue
				}
				pc := posCount[cand]
				nc := negCount[cand]
				if pc == 0 || nc == 0 {
					continue
				}
				actualCost := pc * nc
				if actualCost != c {
					// Stale entry — reinsert to correct bucket if still eligible
					if actualCost <= maxVEPairCost {
						buckets[actualCost] = append(buckets[actualCost], cand)
					}
					continue
				}
				v = cand
				found = true
				break
			}
			if found {
				break
			}
		}

		if !found {
			break // no eliminatable variables
		}

		// Budget check
		if s.veBudget > 0 && totalResolvents >= s.veBudget {
			break
		}

		// Collect active clauses for v (skip removed entries).
		var posClauses, negClauses []int
		for _, ci := range posOcc[v] {
			if !removed[ci] {
				posClauses = append(posClauses, ci)
			}
		}
		for _, ci := range negOcc[v] {
			if !removed[ci] {
				negClauses = append(negClauses, ci)
			}
		}

		// Generate resolvents
		var resolvents [][]cnf.Literal
		for _, posCi := range posClauses {
			posClause := s.cnf.Clauses[posCi].Literals
			for _, negCi := range negClauses {
				negClause := s.cnf.Clauses[negCi].Literals

				// Build resolvent: (A ∨ B) from (x∨A) and (¬x∨B)
				for _, idx := range touched {
					seenLit[idx] = false
				}
				touched = touched[:0]

				isTautology := false
				var resolvent []cnf.Literal
				for _, lit := range posClause {
					if lit.Var() == v {
						continue
					}
					idx := cnf.LitToIndex(lit)
					if seenLit[idx] {
						continue
					}
					seenLit[idx] = true
					touched = append(touched, idx)
					if seenLit[idx^1] {
						isTautology = true
						break
					}
					resolvent = append(resolvent, lit)
				}
				if isTautology {
					continue
				}
				for _, lit := range negClause {
					if lit.Var() == v {
						continue
					}
					idx := cnf.LitToIndex(lit)
					if seenLit[idx] {
						continue
					}
					seenLit[idx] = true
					touched = append(touched, idx)
					if seenLit[idx^1] {
						isTautology = true
						break
					}
					resolvent = append(resolvent, lit)
				}
				if isTautology {
					continue
				}
				// Empty resolvent (both clauses were units {x} and {¬x}) → UNSAT
				if len(resolvent) == 0 {
					return -1
				}
				resolvents = append(resolvents, resolvent)
				totalResolvents++
			}
			if s.veBudget > 0 && totalResolvents >= s.veBudget {
				break
			}
		}

		// Clause-count gate: only eliminate if resolvents < old clauses
		oldClauseCount := len(posClauses) + len(negClauses)
		if len(resolvents) >= oldClauseCount {
			break
		}

		// Save clauses for model reconstruction
		var savedClauses [][]cnf.Literal
		for _, ci := range posClauses {
			lits := make([]cnf.Literal, len(s.cnf.Clauses[ci].Literals))
			copy(lits, s.cnf.Clauses[ci].Literals)
			savedClauses = append(savedClauses, lits)
		}
		for _, ci := range negClauses {
			lits := make([]cnf.Literal, len(s.cnf.Clauses[ci].Literals))
			copy(lits, s.cnf.Clauses[ci].Literals)
			savedClauses = append(savedClauses, lits)
		}
		s.eliminatedVars = append(s.eliminatedVars, eliminatedVar{
			varIdx:  v,
			clauses: savedClauses,
		})

		// Mark old clauses as removed and decrement counts for their variables.
		// Collect affected variables for bucket queue updates.
		for _, ci := range posClauses {
			removed[ci] = true
			for _, lit := range s.cnf.Clauses[ci].Literals {
				lv := lit.Var()
				if lv == v {
					continue
				}
				if lit.IsNegated() {
					negCount[lv]--
				} else {
					posCount[lv]--
				}
				if !eliminatedFlag[lv] && !seenVar[lv] {
					seenVar[lv] = true
					touchedVars = append(touchedVars, int(lv))
				}
			}
		}
		for _, ci := range negClauses {
			removed[ci] = true
			for _, lit := range s.cnf.Clauses[ci].Literals {
				lv := lit.Var()
				if lv == v {
					continue
				}
				if lit.IsNegated() {
					negCount[lv]--
				} else {
					posCount[lv]--
				}
				if !eliminatedFlag[lv] && !seenVar[lv] {
					seenVar[lv] = true
					touchedVars = append(touchedVars, int(lv))
				}
			}
		}

		// Add resolvents to the clause database + occurrence lists
		for _, res := range resolvents {
			resCopy := make([]cnf.Literal, len(res))
			copy(resCopy, res)
			newCi := len(s.cnf.Clauses)
			s.cnf.Clauses = append(s.cnf.Clauses, cnf.Clause{Literals: resCopy})
			s.cnf.NumClauses++
			removed = append(removed, false)
			// Update occurrence lists + counts for the resolvent's variables
			for _, lit := range resCopy {
				lv := lit.Var()
				if lit.IsNegated() {
					negOcc[lv] = append(negOcc[lv], newCi)
					negCount[lv]++
				} else {
					posOcc[lv] = append(posOcc[lv], newCi)
					posCount[lv]++
				}
				if !eliminatedFlag[lv] && !seenVar[lv] {
					seenVar[lv] = true
					touchedVars = append(touchedVars, int(lv))
				}
			}
		}

		// Push affected variables to new buckets based on updated counts
		for _, lv := range touchedVars {
			pc := posCount[lv]
			nc := negCount[lv]
			if pc > 0 && nc > 0 {
				cost := pc * nc
				if cost <= maxVEPairCost {
					buckets[cost] = append(buckets[cost], uint32(lv))
				}
			}
			seenVar[lv] = false
		}
		touchedVars = touchedVars[:0]

		eliminatedFlag[v] = true
		eliminated++
	}

	// Clear seenLit for the last batch
	for _, idx := range touched {
		seenLit[idx] = false
	}

	if eliminated == 0 {
		return 0
	}

	// Batch compaction: compact all removed clauses at once
	s.compactClauses(removed)
	s.cnf.RebuildLiteralPool()
	s.originalUnitClauses = precomputeOriginalUnitClauses(s.cnf)

	s.Log("c [ve] Eliminated %d variables, %d resolvents added, budget=%d\n", eliminated, totalResolvents, s.veBudget)
	return eliminated
}

// reconstructEliminatedVars reconstructs values for variables eliminated by
// BVE. Processes in reverse elimination order (last-eliminated first).
//
// For each eliminated variable x: set x=true, check if all stored clauses are
// satisfied. If yes, x=true. If no, x=false (guaranteed correct by the
// resolution argument: if ¬x∨B is unsatisfied with x=true, then B is false, so
// every resolvent (A∨B) forces A true, satisfying every (x∨A) with x=false).
func (s *CDCLSolver) reconstructEliminatedVars() {
	for i := len(s.eliminatedVars) - 1; i >= 0; i-- {
		ev := &s.eliminatedVars[i]
		v := ev.varIdx

		// Try x=true
		xTrue := true
		for _, clause := range ev.clauses {
			satisfied := false
			for _, lit := range clause {
				if lit.Var() == v {
					if !lit.IsNegated() {
						satisfied = true // x=true satisfies positive literal
						break
					}
					continue // x=true makes ¬x false, skip
				}
				if int(lit.Var()) < len(s.assignments) && s.assignments[lit.Var()].Level >= 0 {
					if lit.IsNegated() != s.assignments[lit.Var()].Value {
						satisfied = true
						break
					}
				}
			}
			if !satisfied {
				xTrue = false
				break
			}
		}

		if xTrue {
			s.assignments[v] = Assignment{Value: true, Level: 0}
		} else {
			s.assignments[v] = Assignment{Value: false, Level: 0}
		}
	}
}
