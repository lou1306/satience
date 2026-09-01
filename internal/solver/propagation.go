package solver

import (
	"fmt"
	"os"
	"unsafe"

	"satience/internal/cnf"
)

// watch propagation hot path and watch-list management.
// Split out of solver_cdcl.go for structure; same CDCLSolver state, no
// behavior change.

// propagateWatched performs unit propagation using watched literals
// Returns (conflict, conflictClauseIndex) where conflictClauseIndex is:
// - >= 0 for original clauses
// - < 0 for learned clauses (encoded as -learnedIdx-1)
//
// CRITICAL: Process all trail elements from s.qhead to end-of-trail, not just
// current level. s.qhead is set to decisionPoint after backjump (see backtrack),
// so skipped elements were already processed before the conflict.

func (s *CDCLSolver) propagateWatched() (bool, *cnf.Clause) {
	if s.verbose && s.conflicts <= 10 {
		s.Log("c [PROPAGATE] qhead=%d, trail len=%d, level=%d\n", s.qhead, len(s.trail), s.level)
	}

	// OPTIMIZATION #1: Use unitLearnedList for O(1) unit propagation
	// Previously scanned ALL learned clauses (O(n)), now only scans unit clauses.
	// Gated on unitsDirty: only scan when new units were learned or backtrack
	// occurred, avoiding the O(units) scan on every propagation call.
	if s.unitsDirty && !(s.inVivification && s.level > 0) {
		s.unitsDirty = false
		for _, learnedIdx := range s.unitLearnedList {
			// Skip deleted/tombstone entries (can happen after swap-remove)
			if learnedIdx >= s.learnedCapacity || s.learnedLoc[learnedIdx].Size != 1 {
				continue
			}
			literals := s.getLearnedClauseLiterals(learnedIdx)
			if len(literals) != 1 {
				continue
			}
			lit := literals[0]
			varIdx := lit.Var()
			litValue := !lit.IsNegated()
			if s.verbose {
				s.Log("c [UNIT SCAN] idx=%d, var=%d, level=%d\n", learnedIdx, varIdx+1, s.assignments[varIdx].Level)
			}
			if s.assignments[varIdx].Level < 0 {
				// Root-level learned units propagate at s.level (0 after restart).
				// Level 0 is correct: 1-UIP skips level-0 literals (always-true),
				// so they neither pollute the working clause nor appear as
				// candidates. They sit at the front of s.trail (before trailHead[1])
				// and survive cancelUntil(0).
				// Do NOT update trailHead — it tracks decisions only.
				propLevel := s.level
				if s.verbose {
					s.Log("c [UNIT PROP] var=%d, value=%v, level=%d (s.level=%d)\n", varIdx+1, litValue, propLevel, s.level)
				}
				s.assignments[varIdx] = Assignment{Value: litValue, Level: int32(propLevel), Reason: int32(-learnedIdx - 5)}
				s.litTrue[int(varIdx)*2] = litValue
				s.litTrue[int(varIdx)*2+1] = !litValue
				s.trail = append(s.trail, varIdx)
				s.numUnassigned--
				s.propagations++
			} else if s.assignments[varIdx].Value != litValue {
				// Conflict: unit learned clause conflicts with existing assignment
				existingIdx := s.assignments[varIdx].Reason
				existingLevel := s.assignments[varIdx].Level
				existingValue := s.assignments[varIdx].Value
				if s.verbose {
					s.Log("c [UNIT CONFLICT] var=%d, new=%v (idx=%d), existing=%v (level=%d, idx=%d)\n",
						varIdx+1, litValue, learnedIdx, existingValue, existingLevel, existingIdx)
				}
				if existingIdx != -1 {
					// Existing assignment is from propagation (not decision) - UNSAT!
					if existingIdx <= -5 {
						existingLearnedIdx := -existingIdx - 5
						existingLits := s.getLearnedClauseLiterals(int(existingLearnedIdx))
						if s.verbose {
							s.Log("c   Existing from learned clause %d: ", existingLearnedIdx)
							for _, l := range existingLits {
								s.Log("%d%c ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()])
								s.Log("\n")
							}
							s.Log("c   New unit clause %d: ", learnedIdx)
							for _, l := range literals {
								s.Log("%d%c ", l.Var()+1, map[bool]byte{true: '-', false: '+'}[l.IsNegated()])
							}
							s.Log("\n")
						}
					}
				}
				s.emptyClauseFound = true
				s.conflictClauseBuf.Literals = literals
				s.conflictClauseBuf.Learned = true
				return true, &s.conflictClauseBuf
			}
		}
	}

	// qhead is positioned by the trail-appenders, not reset here:
	//   - decide()  -> successful propagateWatched sets s.qhead = len(trail)
	//                 (:4792), which equals trailHead[level], covering the new
	//                 decision literal.
	//   - backtrack() -> s.qhead = decisionPoint (:6537), covering the asserting
	//                 literal and any unit-scan-appended elements.
	//   - cancelUntil() -> s.qhead = len(trail) (:3154) (restart/level-cap).
	//   - compactLearnedClauses() -> s.qhead = 0 (:3348), the watch-DB rebuild
	//                 safety net that forces full re-propagation.
	// Previously this function rewound qhead to trailHead[level] on entry, which
	// re-scanned the entire already-processed kept decision level after every
	// backjump (redundant re-propagation on every conflict). It was a no-op for
	// the decision path (where qhead already == trailHead[level]). Removing it
	// relies on the four setters above to cover all newly-appended trail
	// elements; correctness is asserted by the debugCC watch-completeness canary.
	if s.qhead >= len(s.trail) {
		return false, nil
	}

	// Cache slice headers as locals — the compiler can't prove s.assignments
	// isn't aliased through method calls, so it reloads the slice header on every
	// access. The backing array is never reallocated (allocated once in the
	// constructor), so the cached header stays valid for the whole call.
	assignments := s.assignments

	// Cache watchLists outer slice header. The outer slice is allocated once in
	// initWatches and never grows (length is always 2*numVars). Only inner slices
	// grow via append, and the cached header shares the backing array, so inner-
	// slice updates are visible to s.watchLists automatically. This eliminates
	// the s → s.watchLists pointer chase on every append (930ms) and write-back
	// (370ms) in the hot loop.
	watchLists := s.watchLists
	watchListsBinary := s.watchListsBinary

	// litTrue cache: backing array never reallocated, writes through local visible
	// to s.litTrue automatically. litValueBase provides bounds-check-free access
	// via unsafe.Add — the invariant watch.Blit < len(litValue) always holds
	// (Blit = varIdx*2+negated, varIdx < NumVars, litValue has 2*NumVars entries),
	// so the CMPQ+JLS the compiler emits for litValue[watch.Blit] is pure overhead.
	litValue := s.litTrue
	var litValueBase unsafe.Pointer
	if len(litValue) > 0 {
		litValueBase = unsafe.Pointer(&litValue[0])
	}
	// Cache scalar counters as locals — incremented/decremented on every propagation,
	// writing through s pointer each time. Write back at returns.
	propagations := s.propagations
	numUnassigned := s.numUnassigned
	blitFast := s.blitFastHits
	binarySlow := s.binarySlow
	generalSlow := s.generalSlow
	generalMoves := s.generalMoves

	// Cache the trail slice header. trail grows via append below; the cached
	// header must be written back to s.trail at every return so subsequent
	// calls see the grown trail. Same aliasing rationale as assignments above.
	// Also cache s.level (constant for the whole call — no decisions/conflicts
	// occur inside this loop) and derive propLevel once. Root-level propagations
	// (s.level==0) get Level 0: 1-UIP correctly skips them as always-true.
	trail := s.trail
	level := s.level
	propLevel := level
	// Slow-path DB slice headers are cached once at function entry. Serving the
	// slow path (after the blit check fails) from these locals removes the per
	// slow-path-entry method calls (GetOriginalClauseLocs/GetLiteralPool) and
	// slice-header re-fetches. Empirically bit-identical (~5.5% PAR2 on the fast
	// suite) — the Go compiler keeps the fast-path registers live (litValueBase,
	// watchList, readIdx), so the extra locals do NOT spill them here (the old
	// all-at-unsafe-entry hoist was a different, regressing arrangement).
	origClauseLocs := s.cnf.GetOriginalClauseLocs()
	origLiteralPool := s.cnf.GetLiteralPool()
	numOriginalClauses := len(origClauseLocs)
	origSearchHint := s.originalSearchHint
	lrnLoc := s.learnedLoc
	lrnLiterals := s.learnedLiterals
	lrnSearchHint := s.learnedSearchHint

	for trailIndex := s.qhead; trailIndex < len(trail); trailIndex++ {
		lit := trail[trailIndex]

		// CRITICAL FIX: Skip unassigned variables (level < 0)
		// Unassigned variables have Value=false by default, which incorrectly triggers watches
		litAsg := assignments[lit]
		if litAsg.Level < 0 {
			continue // Unassigned - skip watch processing
		}
		value := litAsg.Value

		// OPTIMIZATION: Inline LitToIndex - avoids function call overhead
		// lit index = varIdx * 2 + (1 if negated else 0)
		watchIdx := lit << 1
		if value {
			watchIdx |= 1 // negated literal watches false when var is true
		}

		// Process watches for this literal using write-pointer compaction: each
		// surviving watch is copied forward to wIdx as the scan advances; moved
		// watches are skipped. On a conflict-return the never-scanned suffix is
		// shifted down (never dropped) so no live watch is lost. Bounds-check-free
		// via pointer base: reads span [0,len), writes only to wIdx<=readIdx.
		//
		// Binary (size-2) and general (size>=3) clauses live in SEPARATE per-
		// literal lists (watchListsBinary vs watchLists). Region membership is
		// immutable (a clause never changes size), so a watch never crosses lists
		// and each region is scanned with a specialized body: the binary region
		// needs no clause-data load and no replacement search; the general region
		// needs no binary-bit test or size-2 handling.
		const wSize = 8 // cnf.Watch{ClauseIdx int32; Blit uint32}

		// ---- BINARY PASS ----
		binList := watchListsBinary[watchIdx]
		bLen := len(binList)
		bIdx := 0
		var binBase unsafe.Pointer
		if bLen > 0 {
			binBase = unsafe.Pointer(&binList[0])
		}

		for readIdx := 0; readIdx < bLen; readIdx++ {
			watch := *(*cnf.Watch)(unsafe.Add(binBase, readIdx*wSize))

			// FAST PATH: Blit stores the litTrue index directly (varIdx*2 + negated).
			// unsafe.Add skips the bounds check — the invariant Blit < len(litValue)
			// always holds (Blit = varIdx*2+negated, varIdx < NumVars).
			if *(*bool)(unsafe.Add(litValueBase, watch.Blit)) {
				blitFast++
				*(*cnf.Watch)(unsafe.Add(binBase, bIdx*wSize)) = watch
				bIdx++
				continue
			}

			// BINARY SLOW PATH: For a size-2 clause the Blit field already holds the
			// blocking literal. No replacement search is possible (only 2 literals)
			// and no clause-data load is needed except on the rare conflict branch.
			binarySlow++
			// Bit 31: learned flag, bit 30: myPos, bit 29: binary, bits 0-28: index.
			clauseIdxRaw := uint32(watch.ClauseIdx)
			isLearned := clauseIdxRaw&watchLearnedBit != 0
			clauseID := int(clauseIdxRaw & watchIdxMask)
			blitVarIdx := int(watch.Blit >> 1)
			blitNegated := watch.Blit&1 == 1
			blitAsg := assignments[blitVarIdx]

			if blitAsg.Level < 0 {
				// Unassigned — propagate the blocking literal
				reasonIdx := clauseID
				if isLearned {
					reasonIdx = -clauseID - 5
				}
				blitValue := !blitNegated
				assignments[blitVarIdx] = Assignment{Value: blitValue, Level: int32(propLevel), Reason: int32(reasonIdx), SavedPhase: blitNegated}
				litValue[blitVarIdx*2] = blitValue
				litValue[blitVarIdx*2+1] = !blitValue
				trail = append(trail, uint32(blitVarIdx))
				numUnassigned--
				propagations++
				*(*cnf.Watch)(unsafe.Add(binBase, bIdx*wSize)) = watch
				bIdx++
				continue
			}

			// Assigned — check for conflict (blit is false since fast path already
			// filtered out the true case). Build conflict clause (rare path — clause
			// data load OK here).
			if level == 0 {
				s.emptyClauseFound = true
			}
			if !isLearned {
				originalClauseLocs := origClauseLocs
				originalLiteralPool := origLiteralPool
				loc := originalClauseLocs[clauseID]
				s.conflictClauseBuf.Literals = originalLiteralPool[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
				s.conflictClauseBuf.Learned = false
			} else {
				learnedLoc := s.learnedLoc
				learnedLiterals := s.learnedLiterals
				loc := learnedLoc[clauseID]
				s.conflictClauseBuf.Literals = learnedLiterals[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
				s.conflictClauseBuf.Learned = true
			}
			// The conflicting watch survives — write it into the compacted list.
			*(*cnf.Watch)(unsafe.Add(binBase, bIdx*wSize)) = watch
			bIdx++
			// Never-scanned suffix [readIdx+1, bLen) preserved on early return:
			// shift it down after the compacted survivors so no live watch drops.
			// copy() is a single memmove (dst starts at bIdx <= readIdx+1).
			n := bIdx + copy(binList[bIdx:], binList[readIdx+1:bLen])
			binList = binList[:n]
			watchListsBinary[watchIdx] = binList
			s.propagations = propagations
			s.numUnassigned = numUnassigned
			s.trail = trail
			s.blitFastHits = blitFast
			s.binarySlow = binarySlow
			s.generalSlow = generalSlow
			s.generalMoves = generalMoves
			return true, &s.conflictClauseBuf
		}
		// Normal completion: whole binary list scanned, compacted [0,bIdx) complete.
		watchListsBinary[watchIdx] = binList[:bIdx]

		// ---- GENERAL PASS ----
		watchList := watchLists[watchIdx]
		wlLen := len(watchList)
		wIdx := 0
		var wlBase unsafe.Pointer
		if wlLen > 0 {
			wlBase = unsafe.Pointer(&watchList[0])
		}

		for readIdx := 0; readIdx < wlLen; readIdx++ {
			watch := *(*cnf.Watch)(unsafe.Add(wlBase, readIdx*wSize))

			// FAST PATH: Blit stores the litTrue index directly (varIdx*2 + negated).
			// unsafe.Add skips the bounds check — the invariant Blit < len(litValue)
			// always holds (Blit = varIdx*2+negated, varIdx < NumVars).
			if *(*bool)(unsafe.Add(litValueBase, watch.Blit)) {
				blitFast++
				*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
				wIdx++
				continue
			}

			// SLOW PATH: Blocking literal is not true (or unassigned).
			// Access clause data for replacement search / conflict detection.
			//
			// A1: Slow-path-only slice headers are loaded lazily (see below), not
			// at function entry. This keeps the fast-path inner loop free of the
			// extra slice headers that would otherwise force litValueBase/watchList/
			// readIdx to spill to the stack.
			//
			// Decode ClauseIdx (no header loads — pure bit ops). This is a GENERAL
			// (size>=3) watch — binary clauses live in the separate watchListsBinary
			// pass above — so no binary-bit test and no size-2 propagation body.
			// Bit 31: learned flag, bit 30: myPos, bit 29: binary, bits 0-28: clause index.
			clauseIdxRaw := uint32(watch.ClauseIdx)
			isLearned := clauseIdxRaw&watchLearnedBit != 0
			clauseID := int(clauseIdxRaw & watchIdxMask)

			generalSlow++
			// NON-BINARY clause: all six slow-path headers are already cached in
			// function-entry locals (origClauseLocs, origLiteralPool, ...).
			originalClauseLocs := origClauseLocs
			originalLiteralPool := origLiteralPool
			originalSearchHint := origSearchHint
			learnedLoc := lrnLoc
			learnedLiterals := lrnLiterals
			learnedSearchHint := lrnSearchHint

			var clauseLits []cnf.Literal
			if !isLearned {
				if clauseID >= numOriginalClauses {
					*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
					wIdx++
					continue
				}
				loc := originalClauseLocs[clauseID]
				clauseLits = originalLiteralPool[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
			} else {
				loc := learnedLoc[clauseID]
				if int(loc.Size) == 0 {
					*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
					wIdx++
					continue
				}
				clauseLits = learnedLiterals[int(loc.Offset) : int(loc.Offset)+int(loc.Size)]
			}

			// Re-read the actual blocking literal from clause data (Blit may be stale).
			// The fast-path Blit check already filtered out the true case; here we
			// need the actual literal for the replacement guard, propagation, and
			// conflict detection. myPos/blitPos are derived only from clauseIdxRaw
			// and are consumed ONLY on this non-binary path, so decode them here
			// (not before the binary fast-path check above).
			myPos := int((clauseIdxRaw >> 30) & 1)
			blitPos := 1 - myPos
			blitLit := clauseLits[blitPos]
			blitVarIdx := int(blitLit.Var())
			blitNegated := blitLit.IsNegated()

			// Look for replacement watch
			foundReplacement := false
			var trueReplacementLit uint32 // 0 = none found; else litTrue index of true literal to cache as Blit
			foundJ := -1
			var newWatchIdx int

			var hint int32
			if !isLearned {
				if clauseID < len(originalSearchHint) {
					hint = originalSearchHint[clauseID]
				}
			} else {
				if clauseID < len(learnedSearchHint) {
					hint = learnedSearchHint[clauseID]
				}
			}
			if hint >= 2 && int(hint) < len(clauseLits) {
				clauseLit := clauseLits[hint]
				clauseLitVar := int(clauseLit.Var())
				clauseAsg := assignments[clauseLitVar]
				litNegated := clauseLit.IsNegated()
				if clauseAsg.Level < 0 {
					foundJ = int(hint)
					newWatchIdx = clauseLitVar << 1
					if litNegated {
						newWatchIdx |= 1
					}
				} else {
					litTrue := litNegated != clauseAsg.Value
					if litTrue {
						trueReplacementLit = litToBlit(clauseLit)
						foundJ = int(hint)
						newWatchIdx = clauseLitVar << 1
						if litNegated {
							newWatchIdx |= 1
						}
					}
				}
			}

			if foundJ < 0 {
				s.moveHintMiss++
				// Resume the scan past the known-non-good frontier instead of
				// restarting at 2. searchHint records the frontier: everything in
				// [2, hint) was verified non-good when the last move happened, so
				// [hint, n) is scanned first, then wraps to cover [2, hint) for
				// completeness (slots 0/1 are the two watched positions and are
				// never part of the replacement domain).
				nLits := len(clauseLits)
				start := 2
				if int(hint) >= 2 && int(hint) < nLits {
					start = int(hint)
				}

				// Bounded prefer-true hunt, restricted to learned clauses: learned
				// clauses are short so the cap cost is trivially bounded (no long
				// original-clause scan bloat), and they are the propagation hot
				// ones where parking a satisfied watch pays. For original clauses
				// keep the classic first-non-false policy. Within preferTrueCap
				// positions (wrap order) prefer a currently-TRUE literal: a true
				// watch is "parked" (satisfied, stable until backtrack) so it is
				// not re-visited/re-scanned. Track the first unassigned literal
				// (the classic live unit candidate) as a fallback; it is used once
				// the hunt budget is spent.
				var firstUnassignedJ int
				firstSet := false
				searchN := nLits - 2
				limit := 0
				if s.preferTrueCap > 0 && isLearned {
					limit = searchN
					if s.preferTrueCap < limit {
						limit = s.preferTrueCap
					}
				}
				for k := 0; k < limit; k++ {
					j := start + k
					if j >= nLits {
						j -= searchN // wrap within [2, nLits)
					}
					s.moveScanLits++
					clauseLit := clauseLits[j]
					clauseLitVar := int(clauseLit.Var())

					clauseAsg := assignments[clauseLitVar]
					litNegated := clauseLit.IsNegated()
					if clauseAsg.Level >= 0 {
						litTrue := litNegated != clauseAsg.Value
						if !litTrue {
							continue
						}
						trueReplacementLit = litToBlit(clauseLit)
						foundJ = j
						newWatchIdx = clauseLitVar << 1
						if litNegated {
							newWatchIdx |= 1
						}
						break
					}
					if !firstSet {
						firstSet = true
						firstUnassignedJ = j
					}
				}
				// Any unassigned seen (within or past the budget for short
				// clauses) is a valid replacement; prefer true was already
				// exhaustively checked above.
				if foundJ < 0 && firstSet {
					foundJ = firstUnassignedJ
					newWatchIdx = int(clauseLits[firstUnassignedJ].Var()) << 1
					if clauseLits[firstUnassignedJ].IsNegated() {
						newWatchIdx |= 1
					}
				}
				// Completeness: if the budget ran out before reaching any
				// unassigned/true literal (a long clause whose leading literals are
				// all false), resume the classic first-non-false full-domain scan so
				// a valid replacement is found wherever it lives, and so that a
				// clause with no non-false replacement still falls through to
				// unit/conflict handling below.
				if foundJ < 0 && !firstSet && limit < searchN {
					for j := start; j < nLits; j++ {
						s.moveScanLits++
						clauseLit := clauseLits[j]
						clauseLitVar := int(clauseLit.Var())

						clauseAsg := assignments[clauseLitVar]
						litNegated := clauseLit.IsNegated()
						if clauseAsg.Level >= 0 {
							litTrue := litNegated != clauseAsg.Value
							if !litTrue {
								continue
							}
							trueReplacementLit = litToBlit(clauseLit)
						}
						foundJ = j
						newWatchIdx = clauseLitVar << 1
						if litNegated {
							newWatchIdx |= 1
						}
						break
					}
					if foundJ < 0 {
						for j := 2; j < start; j++ {
							s.moveScanLits++
							clauseLit := clauseLits[j]
							clauseLitVar := int(clauseLit.Var())

							clauseAsg := assignments[clauseLitVar]
							litNegated := clauseLit.IsNegated()
							if clauseAsg.Level >= 0 {
								litTrue := litNegated != clauseAsg.Value
								if !litTrue {
									continue
								}
								trueReplacementLit = litToBlit(clauseLit)
							}
							foundJ = j
							newWatchIdx = clauseLitVar << 1
							if litNegated {
								newWatchIdx |= 1
							}
							break
						}
					}
				}
			}

			if foundJ >= 0 {
				// Swap replacement literal into position myPos
				clauseLits[myPos], clauseLits[foundJ] = clauseLits[foundJ], clauseLits[myPos]

				// Cache the true literal as Blit if found; otherwise cache the other
				// watched literal (at 1-myPos) as before.
				var newBlit uint32
				if trueReplacementLit != 0 {
					newBlit = trueReplacementLit
				} else {
					newBlit = litToBlit(clauseLits[1-myPos])
				}

				dst := watchLists[newWatchIdx]
				if cap(dst) == len(dst) {
					s.moveReallocCast++
				}
				watchLists[newWatchIdx] = append(dst, cnf.Watch{
					ClauseIdx: watch.ClauseIdx,
					Blit:      newBlit,
				})
				if isLearned {
					if clauseID < len(s.learnedWatchIdx0) {
						if myPos == 0 {
							s.learnedWatchIdx0[clauseID] = newWatchIdx
						} else {
							s.learnedWatchIdx1[clauseID] = newWatchIdx
						}
					}
				}

				if !isLearned {
					if clauseID < len(originalSearchHint) {
						originalSearchHint[clauseID] = int32(foundJ)
					}
				} else {
					if clauseID < len(learnedSearchHint) {
						learnedSearchHint[clauseID] = int32(foundJ)
					}
				}

				foundReplacement = true
			}

			if foundReplacement {
				s.numWatchMoves++
				generalMoves++
				// Watch moved to another literal's list — drop it here (slot skipped).
				continue
			}

			// No replacement found - check if we can propagate or have conflict
			blitAsg := assignments[blitVarIdx]

			if blitAsg.Level < 0 {
				// Unassigned blit - propagate it (inlined assignLiteralByClause)
				if s.debugCC {
					// Completeness canary: propagating var blitVarIdx via clauseID is
					// sound ONLY if every other literal is already false. If any is not
					// false (unassigned or true), a watch was missed upstream and the
					// reason clause would be inconsistent -> 1-UIP fails to converge.
					for j, lit := range clauseLits {
						if j == blitPos {
							continue
						}
						asg := assignments[lit.Var()]
						if asg.Level < 0 || (lit.IsNegated() != asg.Value) {
							fmt.Fprintf(os.Stderr, "CC-HOLE: prop var %d via clause %d (lrn=%v) but lit %v (var %d lvl %d val %v) not false; lits=%v myPos=%d blitPos=%d\n",
								blitVarIdx, clauseID, isLearned, lit, lit.Var(), asg.Level, asg.Value, clauseLits, myPos, blitPos)
							panic("completeness hole: unit-implied literal has a non-false antecedent")
						}
					}
				}
				if blitAsg.Reason == -1 {
					reasonIdx := clauseID
					if isLearned {
						reasonIdx = -clauseID - 5
					}
					blitValue := !blitNegated
					assignments[blitVarIdx] = Assignment{Value: blitValue, Level: int32(propLevel), Reason: int32(reasonIdx), SavedPhase: blitNegated}
					litValue[blitVarIdx*2] = blitValue
					litValue[blitVarIdx*2+1] = !blitValue
					trail = append(trail, uint32(blitVarIdx))
					numUnassigned--
				}
				propagations++
				*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
				wIdx++
				continue
			}

			// Re-check blit value
			blitTrue := blitNegated != blitAsg.Value

			if !blitTrue {
				// Build conflict clause
				var conflictClause *cnf.Clause
				if !isLearned {
					loc := originalClauseLocs[clauseID]
					s.conflictClauseBuf.Literals = originalLiteralPool[loc.Offset : loc.Offset+loc.Size]
					s.conflictClauseBuf.Learned = false
					conflictClause = &s.conflictClauseBuf
				} else {
					literals := s.getLearnedClauseLiterals(clauseID)
					s.conflictClauseBuf.Literals = literals
					s.conflictClauseBuf.Learned = true
					conflictClause = &s.conflictClauseBuf
				}

				if level == 0 {
					s.emptyClauseFound = true
				}
				if s.verbose {
					s.Log("c [PROP CONFLICT] Watch idx=%d, clauseIdx=%d, level=%d\n",
						watchIdx, clauseID, level)
				}
				// The conflicting watch survives — write it into the compacted list.
				*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
				wIdx++
				// Never-scanned suffix [readIdx+1, wlLen) preserved on early return:
				// shift it down after the compacted survivors so no live watch drops.
				// copy() is a single memmove (dst starts at wIdx <= readIdx+1).
				n := wIdx + copy(watchList[wIdx:], watchList[readIdx+1:wlLen])
				watchList = watchList[:n]
				watchLists[watchIdx] = watchList
				s.propagations = propagations
				s.numUnassigned = numUnassigned
				s.trail = trail
				s.blitFastHits = blitFast
				s.binarySlow = binarySlow
				s.generalSlow = generalSlow
				s.generalMoves = generalMoves
				return true, conflictClause
			}
			// Actual blit literal evaluated TRUE (top-of-loop cached Blit was stale)
			// -> clause satisfied, this watch survives silently. Must be written or
			// loop-end truncation drops a live watch (missed unit).
			*(*cnf.Watch)(unsafe.Add(wlBase, wIdx*wSize)) = watch
			wIdx++
		}
		// Normal completion: whole list scanned, compacted [0,wIdx) is complete.
		watchLists[watchIdx] = watchList[:wIdx]
	}

	// Update qhead to end of trail
	s.qhead = len(trail)
	s.trail = trail
	s.propagations = propagations
	s.numUnassigned = numUnassigned
	s.blitFastHits = blitFast
	s.binarySlow = binarySlow
	s.generalSlow = generalSlow
	s.generalMoves = generalMoves

	return false, nil
}

// removeLearnedClauseWatches removes all watches for a deleted learned clause.
// With MiniSat-style watched literals (positions 0/1), no Blit updates needed.
func (s *CDCLSolver) removeLearnedClauseWatches(learnedIdx int) {
	if learnedIdx < 0 || learnedIdx >= s.learnedCapacity {
		return
	}

	if s.learnedLoc[learnedIdx].Size < 2 {
		return
	}

	// Region is immutable: a size-2 learned clause lives in watchListsBinary
	// for both its watched literals, a size>=3 clause in watchLists.
	isBinary := s.learnedLoc[learnedIdx].Size == 2

	// Clause identity for comparison: mask out myPos bit (bit 30) and binary bit
	// (bit 29) since the stored watches may have either myPos value.
	clauseID := uint32(watchLearnedBit | uint32(learnedIdx))

	idx0 := s.learnedWatchIdx0[learnedIdx]
	idx1 := s.learnedWatchIdx1[learnedIdx]

	if idx0 < 0 || idx1 < 0 || idx0 >= len(s.watchLists) || idx1 >= len(s.watchLists) {
		return
	}

	// Remove watch from lit0's watch list (swap-remove, no Blit update needed)
	wl0 := s.watchListOf(idx0, isBinary)
	for i := range wl0 {
		if uint32(wl0[i].ClauseIdx)&watchMyPosMask == clauseID {
			lastIdx := len(wl0) - 1
			if i != lastIdx {
				wl0[i] = wl0[lastIdx]
			}
			wl0 = wl0[:lastIdx]
			break
		}
	}
	s.setWatchList(idx0, isBinary, wl0)

	// Remove watch from lit1's watch list
	wl1 := s.watchListOf(idx1, isBinary)
	for i := range wl1 {
		if uint32(wl1[i].ClauseIdx)&watchMyPosMask == clauseID {
			lastIdx := len(wl1) - 1
			if i != lastIdx {
				wl1[i] = wl1[lastIdx]
			}
			wl1 = wl1[:lastIdx]
			break
		}
	}
	s.setWatchList(idx1, isBinary, wl1)
}

// addLearnedClauseToWatches adds a learned clause to the watch lists.
// Watches the first two literals that are not both false (shared helper).
// Returns the literal indices of the two watched literals.
func (s *CDCLSolver) addLearnedClauseToWatches(learnedIdx int, clause *cnf.Clause, literals []cnf.Literal) (int, int) {
	if len(literals) < 2 {
		return -1, -1
	}

	watch0, watch1 := s.chooseWatchPositions(literals)
	if watch0 < 0 || watch1 < 0 {
		return -1, -1
	}

	// Swap watched literals into positions 0 and 1 (MiniSat-style).
	lit0 := literals[watch0]
	lit1 := literals[watch1]
	literals[0], literals[watch0] = lit0, literals[0]
	if watch1 == 0 {
		literals[1], literals[watch0] = lit1, literals[1]
	} else {
		literals[1], literals[watch1] = lit1, literals[1]
	}

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Pack learned flag (bit 31) + myPos (bit 30) into ClauseIdx. myPos is packed
	// only for GENERAL (size>=3) watches; binary watches live in their own region
	// and never decode it.
	// Bit layout: 31=learned, 30=myPos, bits 0-28=clause index.
	if learnedIdx < 0 || learnedIdx >= int(watchIdxMask) {
		panic(fmt.Sprintf("satience: learned clause index %d exceeds the %d-entry watch packing budget (bits 0-28); watch layout cannot represent more clauses", learnedIdx, int(watchIdxMask)))
	}
	clauseIdx0 := int32(watchLearnedBit | uint32(learnedIdx)) // myPos=0
	clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)           // myPos=1

	if len(literals) == 2 {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit1),
		}, true)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit0),
		}, true)
	} else {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: clauseIdx0,
			Blit:      litToBlit(lit1),
		}, false)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: clauseIdx1,
			Blit:      litToBlit(lit0),
		}, false)
	}

	return idx0, idx1
}

// addOriginalClauseToWatches adds an original clause to the watch lists.
// Original clauses are watched during initWatches at level 0, where every
// variable is unassigned, so both chosen watches are always unassigned
// (never false). The generic chooseWatchPositions prioritizes unassigned/true
// over false but, for LEARNED-clause adds, may legitimately return two false
// watches pre-backjump (one becomes true post-backjump; WB self-corrects).
// That two-false state does not occur here because all literals are unassigned.
func (s *CDCLSolver) addOriginalClauseToWatches(clauseIdx int, clause *cnf.Clause, literals []cnf.Literal) {
	if len(literals) < 2 {
		return
	}

	watch0, watch1 := s.chooseWatchPositions(literals)
	if watch0 < 0 || watch1 < 0 {
		return
	}
	verifyOriginalWatchNotBothFalse(s, literals, watch0, watch1)

	// Swap watched literals into positions 0 and 1 (MiniSat-style).
	// This lets us find the blocking literal at position 1-WatchPos without storing Blit.
	lit0 := literals[watch0]
	lit1 := literals[watch1]
	literals[0], literals[watch0] = lit0, literals[0]
	if watch1 == 0 {
		literals[1], literals[watch0] = lit1, literals[1]
	} else {
		literals[1], literals[watch1] = lit1, literals[1]
	}

	idx0 := cnf.LitToIndex(lit0)
	idx1 := cnf.LitToIndex(lit1)

	// Pack myPos into ClauseIdx bit 30 for GENERAL (size>=3) clauses: the watch
	// on idx0 (lit0 at position 0) has myPos=0, the watch on idx1 (lit1 at
	// position 1) has myPos=1. Binary (size==2) watches live in their own
	// structural region and never decode myPos, so neither watch packs it.
	// Bit layout: 31=learned(0 here), 30=myPos, bits 0-28=clause index.
	if clauseIdx < 0 || clauseIdx >= int(watchIdxMask) {
		panic(fmt.Sprintf("satience: original clause index %d exceeds the %d-entry watch packing budget (bits 0-28); watch layout cannot represent more clauses", clauseIdx, int(watchIdxMask)))
	}
	if len(literals) == 2 {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: int32(clauseIdx),
			Blit:      litToBlit(lit1),
		}, true)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: int32(clauseIdx),
			Blit:      litToBlit(lit0),
		}, true)
	} else {
		s.appendWatch(idx0, cnf.Watch{
			ClauseIdx: int32(clauseIdx),
			Blit:      litToBlit(lit1),
		}, false)
		s.appendWatch(idx1, cnf.Watch{
			ClauseIdx: int32(clauseIdx) | int32(watchMyPosBit),
			Blit:      litToBlit(lit0),
		}, false)
	}
}

// setWatchList writes back a (possibly re-sliced/swapped) watch list.
func (s *CDCLSolver) setWatchList(litIdx int, isBinary bool, wl []cnf.Watch) {
	if isBinary {
		s.watchListsBinary[litIdx] = wl
	} else {
		s.watchLists[litIdx] = wl
	}
}

// watchListOf returns the general or binary watch list for a literal.
func (s *CDCLSolver) watchListOf(litIdx int, isBinary bool) []cnf.Watch {
	if isBinary {
		return s.watchListsBinary[litIdx]
	}
	return s.watchLists[litIdx]
}

// appendWatch literally appends a watch to lit's general (size>=3) or binary

// appendWatch literally appends a watch to lit's general (size>=3) or binary
// (size==2) watch list by clause size. The region a clause belongs to is
// immutable — a clause never changes size across its lifetime — so appends
// always target a stable region (never crossing between lists). This is what
// the write-pointer compaction in propagateWatched relies on: surviving
// watches keep their in-list relative order, and a clause is always in exactly
// one of the two per-literal lists.
func (s *CDCLSolver) appendWatch(litIdx int, w cnf.Watch, isBinary bool) {
	if isBinary {
		s.watchListsBinary[litIdx] = append(s.watchListsBinary[litIdx], w)
	} else {
		s.watchLists[litIdx] = append(s.watchLists[litIdx], w)
	}
}

// chooseWatchPositions selects two literal positions (indices into literals)
// to watch, preferring non-false (unassigned or true) literals so the
// watched-literal invariant (at most one watched literal is false) holds
// after setup. Returns positions (-1 if fewer than two candidates found).
// Shared by original/learned clause watch setup and compaction rebuild.
// chooseWatchPositions picks two literal positions to watch, preferring
// unassigned then true over false literals. When every literal is already
// assigned false it returns two false watches — valid for LEARNED clauses
// added mid-conflict (one watch turns true after backjump and the WB scheme
// self-corrects on the next scan), and impossible for original clauses, which
// are only watched at level 0 where all literals are unassigned.
func (s *CDCLSolver) chooseWatchPositions(literals []cnf.Literal) (int, int) {
	watch0 := -1
	watch1 := -1

	for i, lit := range literals {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level >= 0 {
			// Assigned - check if true
			litTrue := lit.IsNegated() != s.assignments[varIdx].Value
			if !litTrue {
				// False - skip unless we have no other choice
				if watch0 < 0 {
					watch0 = i
				} else if watch1 < 0 {
					watch1 = i
				}
				continue
			}
		}
		// Unassigned or true - prefer this
		if watch0 < 0 {
			watch0 = i
		} else if watch1 < 0 {
			watch1 = i
			break
		}
	}

	return watch0, watch1
}

// initWatches initializes watched literals for all clauses
// Called after preprocessing completes (preprocessing modifies clauses)
func (s *CDCLSolver) initWatches() {
	if s.watchInitialized {
		return
	}

	numLits := int(s.cnf.NumVars) * 2
	if numLits == 0 {
		s.watchInitialized = true
		return
	}
	s.watchLists = make([][]cnf.Watch, numLits)
	s.watchListsBinary = make([][]cnf.Watch, numLits)

	// Pre-allocate each watch list to its actual original-clause load rather
	// than a uniform estimate. At init (post-preprocessing) most literals are
	// unassigned, so chooseWatchPositions picks each clause's first two literals
	// (positions 0 and 1); count those per literal. Capacity is purely a memory/
	// reallocation concern and never affects search order (append handles any
	// under-count, e.g. clauses containing level-0-assigned literals whose watch
	// position differs). Headroom covers learned-clause growth during the solve.
	occ := make([]int, numLits)
	occBin := make([]int, numLits)
	for clauseID := 0; clauseID < s.cnf.NumClauses; clauseID++ {
		lits := s.cnf.Clauses[clauseID].Literals
		if len(lits) < 2 {
			continue
		}
		if len(lits) == 2 {
			occBin[cnf.LitToIndex(lits[0])]++
			occBin[cnf.LitToIndex(lits[1])]++
		} else {
			occ[cnf.LitToIndex(lits[0])]++
			occ[cnf.LitToIndex(lits[1])]++
		}
	}
	for i := range s.watchLists {
		capEst := occ[i]*2 + 16
		if capEst < 8 {
			capEst = 8
		}
		if capEst > 1024 {
			capEst = 1024
		}
		s.watchLists[i] = make([]cnf.Watch, 0, capEst)

		capEst = occBin[i]*2 + 16
		if capEst < 8 {
			capEst = 8
		}
		if capEst > 1024 {
			capEst = 1024
		}
		s.watchListsBinary[i] = make([]cnf.Watch, 0, capEst)
	}

	for clauseID := 0; clauseID < s.cnf.NumClauses; clauseID++ {
		clause := &s.cnf.Clauses[clauseID]
		s.addOriginalClauseToWatches(clauseID, clause, clause.Literals)
	}

	for learnedIdx := 0; learnedIdx < s.learnedCapacity; learnedIdx++ {
		if s.learnedLoc[learnedIdx].Size == 0 {
			continue // Skip tombstones
		}
		literals := s.getLearnedClauseLiterals(learnedIdx)
		// Create a temporary Clause struct for addLearnedClauseToWatches
		tmpClause := &cnf.Clause{Literals: literals, Learned: true}
		s.addLearnedClauseToWatches(learnedIdx, tmpClause, literals)
	}

	// CRITICAL: Set watchInitialized AFTER all clauses are watched.
	// Guards initWatches() idempotency (re-init after preprocessing resets it to false).
	s.watchInitialized = true

	// Initialize per-original-clause search hints (0 = no hint, scan from pos 2).
	// Allocated after preprocessing is complete (clause count is final).
	s.originalSearchHint = make([]int32, s.cnf.NumClauses)

	if s.verbose {
		totalWatches := 0
		for _, wl := range s.watchLists {
			totalWatches += len(wl)
		}
		for _, wl := range s.watchListsBinary {
			totalWatches += len(wl)
		}
		avgWatches := float64(totalWatches) / float64(numLits)
		s.Log("c [verbose] Watched literals enabled: %d watch lists, %d total watches, %.1f avg per lit\n",
			len(s.watchLists)+len(s.watchListsBinary), totalWatches, avgWatches)
	}
}
