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
					resolvent = append(resolvent, lit)
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

		// SOUNDNESS: if the resolvent budget was exhausted while generating
		// resolvents for this variable, the resolvent set above is INCOMPLETE
		// (the break at the budget check truncated it mid-variable). Eliminating
		// now would mark this variable's clauses removed while adding only a
		// partial resolvent set, silently dropping the unsolved implications and
		// weakening the formula (an UNSAT instance can become satisfiable).
		// Budget is exhausted, so abort elimination entirely rather than produce
		// an unsound result.
		if s.veBudget > 0 && totalResolvents >= s.veBudget {
			break
		}

		// Clause-count gate: only eliminate if resolvents <= old clauses.
		// Allow equality (minisat does): eliminating a variable that keeps
		// the clause count the same still reduces the variable count.
		// For large instances (>10000 clauses), use continue (not break) so
		// variables after the first failure are still tried — significantly
		// increases elimination on structured instances like bb34f22f (24K
		// clauses): 2635→3905 vars eliminated, 29.6s→26.0s. For small instances,
		// strict < with break (original behavior).
		oldClauseCount := len(posClauses) + len(negClauses)
		if s.cnf.NumClauses > 10000 {
			if len(resolvents) > oldClauseCount {
				continue
			}
		} else {
			if len(resolvents) >= oldClauseCount {
				break
			}
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

// subsumptionPass performs forward subsumption and self-subsumption (strengthening)
// on the original clause database. Forward subsumption removes clause C if some other
// clause D ⊆ C exists (D subsumes C). Self-subsumption removes literal l from C if
// C\{l} subsumes some other clause (strengthening C). Both are sound simplifications
// that reduce clause count and length without changing satisfiability.
//
// Uses occurrence lists with the shortest-literal entry point for efficiency.
// Returns (subsumedCount, strengthenedCount).
func (s *CDCLSolver) subsumptionPass() (int, int) {
	numVars := int(s.cnf.NumVars)
	if numVars == 0 || s.cnf.NumClauses == 0 {
		return 0, 0
	}
	numLits := numVars * 2

	// Build occurrence lists: for each literal index, which clauses contain it.
	occ := make([][]int, numLits)
	for i, clause := range s.cnf.Clauses {
		for _, lit := range clause.Literals {
			idx := cnf.LitToIndex(lit)
			occ[idx] = append(occ[idx], i)
		}
	}

	removed := make([]bool, len(s.cnf.Clauses))
	subsumed := 0
	strengthened := 0

	// Signature prefilter for subsumption (hash/cache-based). Each clause gets
	// `sigWords` 64-bit membership masks: mask w has bit (hash(lit){w} % 64) set
	// for every literal. A *necessary* condition for D ⊆ C is that every
	// signature word of D is a bit-subset of the corresponding word of C
	// (lit ─→ same literal always sets the same bit, so a bit set in D that is
	// absent from C proves D ⊄ C). This lets us skip the expensive per-literal
	// containment loop for the vast majority of candidate pairs, leaving only
	// the full exact check for pairs that actually pass (collisions only loosen
	// the filter, never make it accept a non-subset, so soundness is preserved).
	// This is the classic multi-probe signature trick used in CaDiCaL/Kissat
	// subsume. numSigWords=2 gives 128 bits; enough discrimination while staying
	// cache-friendly to fetch per candidate.
	const sigWords = 2
	sig := make([][sigWords]uint64, len(s.cnf.Clauses))
	{
		numClauses := len(s.cnf.Clauses)
		pool := s.cnf.GetLiteralPool()
		locs := s.cnf.GetOriginalClauseLocs()
		for ci := 0; ci < numClauses; ci++ {
			off := int(locs[ci].Offset)
			sz := int(locs[ci].Size)
			var s [sigWords]uint64
			for _, lit := range pool[off : off+sz] {
				h := uint64(cnf.LitToIndex(lit)) * 0x9E3779B97F4A7C15
				for w := 0; w < sigWords; w++ {
					s[w] |= uint64(1) << ((h + uint64(w)*0x94D049BB133111EB) & 63)
				}
			}
			sig[ci] = s
		}
	}

	seenLit := make([]bool, numLits)
	var touched []int

	// Work budget: count clause-pair comparisons (the dominant O(n²)-ish cost).
	// Once exhausted, stop simplifying — subsumption is purely an optimization,
	// so stopping early is sound (it just does less). Budget 0 = unlimited.
	budget := s.subsumptionBudget
	comparisons := 0

	// Diminishing-returns abort: track comparisons since the last yield
	// (subsumed or strengthened). Subsumption finds most of its results in the
	// early part of a scan; on large structured instances the tail is pure
	// waste (e.g. 8202af80: 734K clauses burned 37M comparisons / ~690ms
	// yielding sub=0 str=0). If a long no-yield stretch elapses we abort the
	// pass entirely — subsumption is only an optimization, so doing less is
	// sound. The window scales with instance size and yields frequently reset
	// it, so instances that do benefit are unaffected.
	comparisonsSinceHit := 0
	noYieldWindow := len(s.cnf.Clauses)
	if noYieldWindow < 400000 {
		noYieldWindow = 400000
	}
	if s.subsumptionBudget > 0 && s.subsumptionBudget < noYieldWindow {
		noYieldWindow = s.subsumptionBudget
	}
	aborted := false

	// cleanupAndExit clears the current clause's seenLit marks before bailing on
	// a budget exhaustion mid-clause, then aborts the whole pass.
	cleanupAndExit := func() {
		for _, idx := range touched {
			seenLit[idx] = false
		}
		touched = touched[:0]
	}

	// checkAbort bails the pass when the total budget is exhausted or the
	// diminishing-returns window (comparisons since the last yield) is exceeded.
	checkAbort := func() bool {
		if budget > 0 && comparisons >= budget {
			cleanupAndExit()
			aborted = true
			return true
		}
		if comparisonsSinceHit >= noYieldWindow {
			cleanupAndExit()
			aborted = true
			return true
		}
		return false
	}

clauseLoop:
	for ci, clause := range s.cnf.Clauses {
		if aborted {
			break clauseLoop
		}
		if budget > 0 && comparisons >= budget {
			break clauseLoop
		}
		if removed[ci] {
			continue
		}
		clauseLits := clause.Literals

		// Skip empty clauses (can appear after tautology/duplicate removal)
		if len(clauseLits) == 0 {
			continue
		}

		// Mark C's literals for subsumption checking
		for _, lit := range clauseLits {
			idx := cnf.LitToIndex(lit)
			if !seenLit[idx] {
				seenLit[idx] = true
				touched = append(touched, idx)
			}
		}

		// Find the literal with the fewest occurrences (best entry point for scanning)
		bestIdx := -1
		bestCount := int(^uint(0) >> 1)
		for _, lit := range clauseLits {
			idx := cnf.LitToIndex(lit)
			count := len(occ[idx])
			if count < bestCount {
				bestCount = count
				bestIdx = idx
			}
		}

		// Forward subsumption: check if C is subsumed by any D (|D| <= |C|, D ⊆ C)
		isSubsumed := false
		for _, di := range occ[bestIdx] {
			comparisons++
			comparisonsSinceHit++
			if checkAbort() {
				break clauseLoop
			}
			if di == ci || removed[di] {
				continue
			}
			dLits := s.cnf.Clauses[di].Literals
			if len(dLits) > len(clauseLits) {
				continue
			}
			// Signature prefilter: D ⊆ C requires D's signature to be a
			// bit-subset of C's in every word. This skips the per-literal
			// containment scan for most non-subsuming candidates.
			sd := sig[di]
			sc := sig[ci]
			if sd[0]&^sc[0] != 0 || sd[1]&^sc[1] != 0 {
				continue
			}
			allIn := true
			for _, lit := range dLits {
				idx := cnf.LitToIndex(lit)
				if !seenLit[idx] {
					allIn = false
					break
				}
			}
			if allIn {
				isSubsumed = true
				break
			}
		}

		if isSubsumed {
			removed[ci] = true
			subsumed++
			comparisonsSinceHit = 0
			// Cleanup and continue to next clause
			for _, idx := range touched {
				seenLit[idx] = false
			}
			touched = touched[:0]
			continue
		}

		// Self-subsumption (strengthening): for each literal l in C, check if
		// some clause D contains ¬l and D\{¬l} ⊆ C. If so, the resolvent of
		// C and D on l is (C\{l} ∪ D\{¬l}) = D\{¬l} (since D\{¬l} ⊆ C ⊇ C\{l}),
		// which subsumes C. Removing l from C is sound because C ∧ D ⊨ resolvent
		// and resolvent subsumes C.
		//
		// SOUNDNESS:
		// - seenLit is built from C's original literals. After removing l from C,
		//   we clear seenLit[lIdx] so subsequent checks for other literals l'
		//   correctly see l ∉ C (prevents D\{¬l'} ⊆ C from passing when D
		//   contains the removed l).
		// - Occurrence lists are stale after any strengthening (D may have had
		//   ¬l removed by a prior strengthening). We verify D actually contains
		//   ¬l before using it.
		// - Index-based loop: after removing l at position i, the next literal
		//   shifts to position i, so we don't increment i (re-check position i).
		i := 0
		for i < len(clauseLits) {
			lit := clauseLits[i]
			lIdx := cnf.LitToIndex(lit)
			negIdx := lIdx ^ 1 // complement of l

			strengthenedHere := false
			for _, di := range occ[negIdx] {
				comparisons++
				comparisonsSinceHit++
				if checkAbort() {
					break clauseLoop
				}
				if di == ci || removed[di] {
					continue
				}
				dLits := s.cnf.Clauses[di].Literals
				// Verify D actually contains ¬l (occ may be stale after
				// strengthening D removed ¬l from it)
				containsNegL := false
				for _, dlit := range dLits {
					if cnf.LitToIndex(dlit) == negIdx {
						containsNegL = true
						break
					}
				}
				if !containsNegL {
					continue
				}
				// Signature prefilter for strengthening: D\{¬l} ⊆ C. Clear ¬l's
				// bit from D's signature in each word, then require the result
				// to be a bit-subset of C's signature (necessary condition;
				// collisions only loosen, never wrongly accept).
				{
					sd := sig[di]
					sc := sig[ci]
					ok := true
					for w := 0; w < sigWords; w++ {
						bitNeg := uint64(1) << ((uint64(negIdx)*0x9E3779B97F4A7C15 + uint64(w)*0x94D049BB133111EB) & 63)
						if (sd[w]&^bitNeg)&^sc[w] != 0 {
							ok = false
							break
						}
					}
					if !ok {
						continue
					}
				}
				// Check D\{¬l} ⊆ C: every literal in D except ¬l must be in C
				allIn := true
				for _, dlit := range dLits {
					dIdx := cnf.LitToIndex(dlit)
					if dIdx == negIdx {
						continue // skip ¬l
					}
					if !seenLit[dIdx] {
						allIn = false
						break
					}
				}
				if allIn {
					// Remove l from C (strengthen C). In-place shift avoids an
					// allocation+copy per strengthening (thousands per pass).
					// Clause literal order is irrelevant for subsumption.
					clauseLits = removeLiteralAt(clauseLits, i)
					s.cnf.Clauses[ci].Literals = clauseLits
					seenLit[lIdx] = false
					strengthened++
					comparisonsSinceHit = 0
					strengthenedHere = true
					break
				}
			}
			if !strengthenedHere {
				i++
			}
		}

		// Cleanup
		for _, idx := range touched {
			seenLit[idx] = false
		}
		touched = touched[:0]
	}

	if subsumed > 0 {
		s.compactClauses(removed)
	}

	if subsumed > 0 || strengthened > 0 {
		s.cnf.RebuildLiteralPool()
	}

	return subsumed, strengthened
}

// removeLiteralAt removes the literal at position i in-place by shifting the
// tail left, avoiding an allocation/copy per removal. Returns the shortened
// slice. Clause literal order is irrelevant for subsumption correctness.
func removeLiteralAt(lits []cnf.Literal, i int) []cnf.Literal {
	copy(lits[i:], lits[i+1:])
	return lits[:len(lits)-1]
}

// subsumptionResult records a single clause modification from
// runLearnedSubsumption. newLits == nil means forward subsumed (delete);
// non-nil means strengthened (rewrite literals + rebuild watches).
type subsumptionResult struct {
	idx     int
	newLits []cnf.Literal
}

// runLearnedSubsumption performs forward subsumption and self-subsumption
// (strengthening) on the learned clause database, using binary learned clauses
// as the subsumers. Runs at level-0 restart boundaries (same safety conditions
// as compactLearnedClauses and runVivification).
//
// Forward subsumption: a non-binary learned clause C is deleted if some binary
// learned clause D is a subset of C (both of D's literals are in C).
//
// Self-subsumption (strengthening): a literal l is removed from C if some binary
// learned clause (not-l or m) exists where m is also in C. The resolvent of C
// and (not-l or m) on l is (C minus l plus m) = C minus l (since m is in C),
// which subsumes C.
//
// Both operations are sound simplifications that reduce clause count and length
// without changing satisfiability. Strengthening also lowers LBD (updated to
// min(oldLBD, newSize)), potentially turning high-LBD clauses into glue clauses.
//
// Returns true if UNSAT was detected (not currently possible -- subsumption
// never derives empty clauses, but the return is kept for consistency with
// runVivification).
func (s *CDCLSolver) runLearnedSubsumption() bool {
	if s.learnedActiveCount == 0 {
		return false
	}

	numVars := int(s.cnf.NumVars)
	if numVars == 0 {
		return false
	}
	numLits := numVars * 2

	// Reusable occurrence lists: grow the outer slice if needed, clear inner
	// slices to [:0] (preserves backing arrays) so they can be re-appended
	// without re-allocation.
	if cap(s.tmpLearnedSubOcc) < numLits {
		s.tmpLearnedSubOcc = make([][]int, numLits)
	} else {
		s.tmpLearnedSubOcc = s.tmpLearnedSubOcc[:numLits]
		for i := range s.tmpLearnedSubOcc {
			s.tmpLearnedSubOcc[i] = s.tmpLearnedSubOcc[i][:0]
		}
	}
	occ := s.tmpLearnedSubOcc

	binaryCount := 0
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size != 2 {
			continue
		}
		lits := s.getLearnedClauseLiterals(i)
		idx0 := cnf.LitToIndex(lits[0])
		idx1 := cnf.LitToIndex(lits[1])
		occ[idx0] = append(occ[idx0], i)
		occ[idx1] = append(occ[idx1], i)
		binaryCount++
	}

	if binaryCount == 0 {
		return false
	}

	s.subsumptionRoundsRun++

	// Mark protected clauses (used as reasons). At level 0 after restart, no
	// learned clause is in use as a reason, so this is all false. Built for
	// safety in case the function is ever called at a non-level-0 state. The
	// bitmap is consumed ONLY by the protected check below, so skip the
	// O(learnedCapacity) zero + O(numVars) scan entirely unless some assignment
	// actually holds a learned reason.
	var protected []bool
	anyLearnedReason := false
	for _, asg := range s.assignments {
		if asg.Reason <= -5 {
			anyLearnedReason = true
			break
		}
	}
	if anyLearnedReason {
		protected = s.markProtectedClauses()
	}

	if cap(s.tmpLearnedSubSeen) < numLits {
		s.tmpLearnedSubSeen = make([]bool, numLits)
	}
	seenLit := s.tmpLearnedSubSeen[:numLits]
	touched := s.tmpLearnedSubTouched[:0]

	checkedCount := 0
	results := s.tmpLearnedSubResults[:0]

	for i := 0; i < s.learnedCapacity; i++ {
		size := int(s.learnedLoc[i].Size)
		if size <= 2 {
			continue
		}
		if protected != nil && protected[i] {
			continue
		}
		if checkedCount >= maxCheckPerRound {
			break
		}
		checkedCount++
		s.subsumptionClausesChecked++

		lits := s.getLearnedClauseLiterals(i)

		// Mark C's literals in seenLit for subsumption checking.
		for _, lit := range lits {
			idx := cnf.LitToIndex(lit)
			if !seenLit[idx] {
				seenLit[idx] = true
				touched = append(touched, idx)
			}
		}

		// Forward subsumption: find binary D that is a subset of C.
		// Use the literal with fewest occurrences as the entry point (standard
		// optimization -- the rarest literal is the most selective filter).
		bestIdx := -1
		bestCount := int(^uint(0) >> 1)
		for _, lit := range lits {
			idx := cnf.LitToIndex(lit)
			count := len(occ[idx])
			if count < bestCount {
				bestCount = count
				bestIdx = idx
			}
		}

		isSubsumed := false
		if bestIdx >= 0 {
			for _, di := range occ[bestIdx] {
				if di == i {
					continue
				}
				if s.learnedLoc[di].Size != 2 {
					continue // tombstoned since occ was built
				}
				dLits := s.getLearnedClauseLiterals(di)
				d0 := cnf.LitToIndex(dLits[0])
				var otherIdx int
				if d0 == bestIdx {
					otherIdx = cnf.LitToIndex(dLits[1])
				} else {
					otherIdx = d0
				}
				if seenLit[otherIdx] {
					isSubsumed = true
					break
				}
			}
		}

		if isSubsumed {
			results = append(results, subsumptionResult{idx: i})
			s.subsumptionClausesSubsumed++
			for _, idx := range touched {
				seenLit[idx] = false
			}
			touched = touched[:0]
			continue
		}

		// Self-subsumption (strengthening): for each literal l in C, scan
		// occ[not-l] for binary clause (not-l or m) where m is in C. If found,
		// remove l from C (the resolvent C-minus-l subsumes C). After removing
		// l, the next literal shifts to position j, so re-check position j.
		newLits := make([]cnf.Literal, len(lits))
		copy(newLits, lits)

		j := 0
		for j < len(newLits) {
			lit := newLits[j]
			lIdx := cnf.LitToIndex(lit)
			negLIdx := lIdx ^ 1

			strengthenedHere := false
			for _, di := range occ[negLIdx] {
				if di == i {
					continue
				}
				if s.learnedLoc[di].Size != 2 {
					continue
				}
				dLits := s.getLearnedClauseLiterals(di)
				d0 := cnf.LitToIndex(dLits[0])
				var otherIdx int
				if d0 == negLIdx {
					otherIdx = cnf.LitToIndex(dLits[1])
				} else {
					otherIdx = d0
				}
				// Guard against tautological binary clause (¬l ∨ l): the
				// resolvent of C and (¬l ∨ l) on l is C itself, not a
				// subsumption, so removing l would be unsound. Learned
				// clauses should never be tautologies, but defend anyway.
				if otherIdx == lIdx {
					continue
				}
				if seenLit[otherIdx] {
					newLits = append(newLits[:j], newLits[j+1:]...)
					seenLit[lIdx] = false
					strengthenedHere = true
					s.subsumptionClausesStrengthened++
					break
				}
			}
			if !strengthenedHere {
				j++
			}
		}

		if len(newLits) < len(lits) && len(newLits) >= 2 {
			results = append(results, subsumptionResult{idx: i, newLits: newLits})
		}

		// Cleanup seenLit
		for _, idx := range touched {
			seenLit[idx] = false
		}
		touched = touched[:0]
	}

	if len(results) == 0 {
		s.tmpLearnedSubTouched = touched
		s.tmpLearnedSubResults = results
		return false
	}

	// Apply results: deletions (tombstone + remove watches) and strengthenings
	// (remove watches, rewrite literals, update LBD, rebuild watches).
	// Same pattern as runVivification's apply phase.
	for _, r := range results {
		if r.newLits == nil {
			// Forward subsumed -- delete
			s.removeLearnedClauseWatches(r.idx)
			s.learnedLoc[r.idx].Size = 0
			s.learnedWatchIdx0[r.idx] = -1
			s.learnedWatchIdx1[r.idx] = -1
			s.learnedActiveCount--
		} else {
			// Strengthened
			offset := int(s.learnedLoc[r.idx].Offset)
			oldSize := int(s.learnedLoc[r.idx].Size)
			s.removeLearnedClauseWatches(r.idx)
			copy(s.learnedLiterals[offset:offset+oldSize], r.newLits)
			s.learnedLoc[r.idx].Size = int32(len(r.newLits))
			newSize := len(r.newLits)
			if newSize < int(s.learnedMetadata[r.idx].LBD) {
				s.learnedMetadata[r.idx].LBD = int32(newSize)
			}
			s.learnedSearchHint[r.idx] = 0
			if newSize >= 2 {
				lits := s.learnedLiterals[offset : offset+newSize]
				tmpClause := &cnf.Clause{Literals: lits, Learned: true}
				idx0, idx1 := s.addLearnedClauseToWatches(r.idx, tmpClause, lits)
				if r.idx < len(s.learnedWatchIdx0) {
					s.learnedWatchIdx0[r.idx] = idx0
					s.learnedWatchIdx1[r.idx] = idx1
				}
			}
		}
	}

	s.compactPending = true

	s.tmpLearnedSubTouched = touched
	s.tmpLearnedSubResults = results

	s.Log("c [subsumption] Subsumed %d, strengthened %d/%d clauses\n",
		s.subsumptionClausesSubsumed, s.subsumptionClausesStrengthened, checkedCount)

	return false
}
