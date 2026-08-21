package solver

import (
	"satience/internal/cnf"
)

// detectEquivalences finds equivalent literals via SCC on the binary implication
// graph and merges them. Two literals l1, l2 are equivalent iff l1 → l2 and
// l2 → l1 in the implication graph. For a binary clause (a ∨ b), the graph has
// edges ¬a→b and ¬b→a.
//
// After finding SCCs:
//   - If l and ¬l are in the same SCC → UNSAT (contradiction).
//   - Otherwise, all literals in an SCC are replaced by a single representative.
//   - Substitution may produce tautologies (removed) and duplicates (deduped).
//
// Model reconstruction: after solving, equivalent variables get their values
// from their representative via extendModel().
//
// This is the standard SCC-based equivalence detection used in CaDiCaL/Kissat.
// It replaces the previously-banned pattern-matching variant that produced
// false equivalences.
func (s *CDCLSolver) detectEquivalences() SolveResult {
	if s.cnf.NumVars == 0 || s.cnf.NumClauses == 0 {
		return UNKNOWN
	}

	numLits := int(s.cnf.NumVars) * 2

	// Build adjacency list for the binary implication graph.
	adj := make([][]int, numLits)
	binaryCount := 0
	for i := range s.cnf.Clauses {
		lits := s.cnf.Clauses[i].Literals
		if len(lits) != 2 {
			continue
		}
		a := cnf.LitToIndex(lits[0])
		b := cnf.LitToIndex(lits[1])
		if a == b {
			continue // tautology (a ∨ a) — skip
		}
		if a == b^1 {
			continue // tautology (a ∨ ¬a) — skip
		}
		// ¬a → b, ¬b → a
		adj[a^1] = append(adj[a^1], b)
		adj[b^1] = append(adj[b^1], a)
		binaryCount++
	}

	if binaryCount == 0 {
		return UNKNOWN // no binary clauses → no equivalences
	}

	// Tarjan's SCC (iterative to avoid stack overflow on large instances).
	indices := make([]int, numLits)
	lowlinks := make([]int, numLits)
	onStack := make([]bool, numLits)
	for i := range indices {
		indices[i] = -1
	}

	var sccStack []int
	index := 0

	// repLit maps each literal index to its representative literal index.
	repLit := make([]int, numLits)
	for i := range repLit {
		repLit[i] = i
	}

	// Reused scratch for "is this literal in the current SCC" membership, used
	// solely to detect l and ¬l in the same SCC. Allocated once and reset via
	// touched entries only, so detection stays O(V+E) rather than O(#SCCs × V).
	inSCC := make([]bool, numLits)

	mergedCount := 0

	type frame struct {
		node int
		next int
	}
	var callStack []frame

	for start := 0; start < numLits; start++ {
		if indices[start] != -1 {
			continue
		}

		callStack = append(callStack, frame{node: start, next: 0})

		for len(callStack) > 0 {
			f := &callStack[len(callStack)-1]
			v := f.node

			if f.next == 0 {
				indices[v] = index
				lowlinks[v] = index
				index++
				sccStack = append(sccStack, v)
				onStack[v] = true
			}

			if f.next < len(adj[v]) {
				w := adj[v][f.next]
				f.next++
				if indices[w] == -1 {
					callStack = append(callStack, frame{node: w, next: 0})
				} else if onStack[w] {
					if indices[w] < lowlinks[v] {
						lowlinks[v] = indices[w]
					}
				}
			} else {
				// All neighbors processed — pop frame.
				callStack = callStack[:len(callStack)-1]

				// Update parent's lowlink.
				if len(callStack) > 0 {
					pv := callStack[len(callStack)-1].node
					if lowlinks[v] < lowlinks[pv] {
						lowlinks[pv] = lowlinks[v]
					}
				}

				// Root of SCC?
				if lowlinks[v] == indices[v] {
					var scc []int
					for {
						w := sccStack[len(sccStack)-1]
						sccStack = sccStack[:len(sccStack)-1]
						onStack[w] = false
						scc = append(scc, w)
						if w == v {
							break
						}
					}

					if len(scc) <= 1 {
						continue
					}

					// Check for l and ¬l in same SCC → UNSAT.
					for _, lit := range scc {
						inSCC[lit] = true
					}
					unsat := false
					for _, lit := range scc {
						if inSCC[lit^1] {
							unsat = true
							break
						}
					}
					for _, lit := range scc {
						inSCC[lit] = false
					}
					if unsat {
						s.Log("c [equiv] Contradiction detected: l and ¬l in same SCC\n")
						return UNSAT
					}

					// Pick representative: smallest literal index in the SCC.
					rep := scc[0]
					for _, lit := range scc[1:] {
						if lit < rep {
							rep = lit
						}
					}

					// Map all literals in the SCC to the representative.
					for _, lit := range scc {
						repLit[lit] = rep
					}
					mergedCount += len(scc) - 1
				}
			}
		}
	}

	if mergedCount == 0 {
		return UNKNOWN
	}

	// Skip merging if the merge is insignificant (< 1% of variables).
	// Small merges change the clause structure (and thus the search trajectory)
	// without meaningful reduction. For example, daf59d67 (222406 vars, 200 merged)
	// has a 0.09% merge rate — the trajectory shift causes SAT@13s → TIMEOUT.
	// bb34f22f (7807 vars, 2790 merged = 35.7%) is well above the threshold.
	// Note: the UNSAT check (l and ¬l in same SCC) already ran inside the SCC
	// loop above, so we only skip the substitution, not the contradiction detection.
	if mergedCount*100 < int(s.cnf.NumVars) {
		s.Log("c [equiv] Skipping insignificant merge: %d < %d/100 (0.01%% threshold)\n",
			mergedCount, s.cnf.NumVars)
		return UNKNOWN
	}

	// Substitute literals in all clauses, deduping duplicates.
	removed := make([]bool, s.cnf.NumClauses)
	removedCount := 0
	seenLit := make([]bool, numLits)

	for i := range s.cnf.Clauses {
		lits := s.cnf.Clauses[i].Literals
		var touched []int
		writeIdx := 0
		isTautology := false

		for _, lit := range lits {
			litIdx := cnf.LitToIndex(lit)
			repIdx := repLit[litIdx]
			if !seenLit[repIdx] {
				seenLit[repIdx] = true
				touched = append(touched, repIdx)
				if seenLit[repIdx^1] {
					isTautology = true
				}
				// Write non-duplicate substituted literal in-place.
				lits[writeIdx] = cnf.IndexToLit(repIdx)
				writeIdx++
			}
			// else: duplicate after substitution — skip
		}

		// Reset seenLit for touched entries.
		for _, idx := range touched {
			seenLit[idx] = false
		}

		if isTautology {
			removed[i] = true
			removedCount++
			continue
		}

		if writeIdx < len(lits) {
			s.cnf.Clauses[i].Literals = lits[:writeIdx]
		}
	}

	if removedCount > 0 {
		s.compactClauses(removed)
	}

	// Store equivalence mapping for model reconstruction.
	s.equivRep = make([]uint32, s.cnf.NumVars)
	s.equivNeg = make([]bool, s.cnf.NumVars)
	s.hasEquivalences = true
	for v := uint32(0); v < s.cnf.NumVars; v++ {
		repIdx := repLit[int(v)*2]
		s.equivRep[v] = uint32(repIdx / 2)
		s.equivNeg[v] = (repIdx % 2) == 1
	}

	s.Log("c [equiv] Merged %d equivalent literals, removed %d tautological clauses\n",
		mergedCount, removedCount)

	s.cnf.RebuildLiteralPool()

	return UNKNOWN
}

// extendModel sets values for variables that were merged during equivalence
// detection. Each non-representative variable gets its value from its
// representative (possibly negated). Must be called after solving returns SAT.
func (s *CDCLSolver) extendModel() {
	if !s.hasEquivalences {
		return
	}
	for v := uint32(0); v < s.cnf.NumVars; v++ {
		if s.equivRep[v] != v {
			repVar := s.equivRep[v]
			if int(repVar) < len(s.assignments) && s.assignments[repVar].Level >= 0 {
				val := s.assignments[repVar].Value
				if s.equivNeg[v] {
					val = !val
				}
				s.assignments[v] = Assignment{Value: val, Level: 0}
			}
		}
	}
}
