package solver

import (
	"sort"

	"satience/internal/cnf"
)

// Learned-clause database reduction (LBD-tiered deletion, compaction, and
// protection). Split out of solver_cdcl.go for structure; same CDCLSolver
// state, no behavior change.

// compactLearnedClauses rebuilds all learned clause arrays to remove tombstones
// This is called periodically when tombstone ratio exceeds threshold
func (s *CDCLSolver) compactLearnedClauses() {
	s.Log("c [compact] Compacting learned clauses: capacity=%d, active=%d\n",
		s.learnedCapacity, s.learnedActiveCount)

	// Build clause index mapping (old -> new compacted index)
	if cap(s.tmpClauseIndexMap) < s.learnedCapacity {
		s.tmpClauseIndexMap = make([]int, s.learnedCapacity)
	}
	clauseIndexMap := s.tmpClauseIndexMap[:s.learnedCapacity]
	for i := range clauseIndexMap {
		clauseIndexMap[i] = -1
	}

	// Compact clauses: move active clauses to contiguous positions
	writeIdx := 0
	nextOffset := 0

	for readIdx := 0; readIdx < s.learnedCapacity; readIdx++ {
		if s.learnedLoc[readIdx].Size == 0 {
			continue // Skip tombstones
		}

		// Record mapping
		clauseIndexMap[readIdx] = writeIdx

		// Move clause metadata
		oldStart := int(s.learnedLoc[readIdx].Offset)
		oldSize := int(s.learnedLoc[readIdx].Size)
		newStart := nextOffset

		s.learnedLoc[writeIdx] = LearnedClauseLoc{Offset: int32(newStart), Size: int32(oldSize)}
		s.learnedMetadata[writeIdx] = s.learnedMetadata[readIdx]
		s.learnedSearchHint[writeIdx] = s.learnedSearchHint[readIdx]
		s.learnedWatchIdx0[writeIdx] = s.learnedWatchIdx0[readIdx]
		s.learnedWatchIdx1[writeIdx] = s.learnedWatchIdx1[readIdx]

		// Copy literals
		copy(s.learnedLiterals[newStart:newStart+oldSize], s.learnedLiterals[oldStart:oldStart+oldSize])

		writeIdx++
		nextOffset += oldSize
	}

	// Update Reason fields using the mapping
	for varIdx := range s.assignments {
		if s.assignments[varIdx].Reason <= -5 {
			learnedIdx := -s.assignments[varIdx].Reason - 5
			if int(learnedIdx) < len(clauseIndexMap) && clauseIndexMap[learnedIdx] >= 0 {
				s.assignments[varIdx].Reason = int32(-clauseIndexMap[learnedIdx] - 5)
			} else if int(learnedIdx) < len(clauseIndexMap) {
				// Clause was deleted - reset to decision
				s.assignments[varIdx].Reason = -1
			}
		}
	}

	// REBUILD WATCH LISTS — keep original-clause watches, replace learned.
	// Original clauses don't move during learned-clause compaction, so their
	// watches (ClauseIdx >= 0) are still valid. Only learned-clause watches
	// (ClauseIdx < 0) have stale indices after the clauseIndexMap remapping.
	// Removing the O(total original literals) re-scan of chooseWatchPositions
	// that the old full-rebuild did on every compaction.
	for litIdx := range s.watchLists {
		wl := s.watchLists[litIdx]
		writeIdx := 0
		for _, watch := range wl {
			if watch.ClauseIdx >= 0 {
				wl[writeIdx] = watch
				writeIdx++
			}
		}
		s.watchLists[litIdx] = wl[:writeIdx]

		bl := s.watchListsBinary[litIdx]
		writeIdx = 0
		for _, watch := range bl {
			if watch.ClauseIdx >= 0 {
				bl[writeIdx] = watch
				writeIdx++
			}
		}
		s.watchListsBinary[litIdx] = bl[:writeIdx]
	}

	// Re-add learned clauses (with freshly chosen watch positions)
	for i := 0; i < writeIdx; i++ {
		if s.learnedLoc[i].Size < 2 {
			continue
		}
		literals := s.getLearnedClauseLiterals(i)
		if len(literals) < 2 {
			continue
		}
		watch0, watch1 := s.chooseWatchPositions(literals)
		if watch0 < 0 || watch1 < 0 {
			s.learnedWatchIdx0[i] = -1
			s.learnedWatchIdx1[i] = -1
			continue
		}
		idx0 := cnf.LitToIndex(literals[watch0])
		idx1 := cnf.LitToIndex(literals[watch1])
		// Swap watched literals into positions 0 and 1 (MiniSat-style)
		lit0 := literals[watch0]
		lit1 := literals[watch1]
		literals[0], literals[watch0] = lit0, literals[0]
		if watch1 == 0 {
			literals[1], literals[watch0] = lit1, literals[1]
		} else {
			literals[1], literals[watch1] = lit1, literals[1]
		}
		clauseIdx0 := int32(watchLearnedBit | uint32(i))
		clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)

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

		// Update stored watch indices to the chosen literals
		s.learnedWatchIdx0[i] = idx0
		s.learnedWatchIdx1[i] = idx1
		// Reset search hint — compaction re-ran chooseWatchPositions which
		// reordered literals, invalidating any position-based hint.
		s.learnedSearchHint[i] = 0
	}

	// Mark watches as initialized
	s.watchInitialized = true

	// Truncate arrays to new capacity
	s.learnedLiterals = s.learnedLiterals[:nextOffset]
	s.learnedLoc = s.learnedLoc[:writeIdx]
	s.learnedMetadata = s.learnedMetadata[:writeIdx]
	s.learnedSearchHint = s.learnedSearchHint[:writeIdx]
	s.learnedWatchIdx0 = s.learnedWatchIdx0[:writeIdx]
	s.learnedWatchIdx1 = s.learnedWatchIdx1[:writeIdx]
	s.learnedCapacity = writeIdx
	s.learnedActiveCount = writeIdx

	// Rebuild unit list
	s.unitLearnedList = s.unitLearnedList[:0]
	for i := 0; i < writeIdx; i++ {
		if s.learnedLoc[i].Size == 1 {
			s.unitsDirty = true
			s.unitLearnedList = append(s.unitLearnedList, i)
		}
	}

	s.Log("c [compact] Compaction complete: new capacity=%d, literals=%d\n",
		writeIdx, nextOffset)

	// Debug-build only: validate that all clause references (implications,
	// watches, unit list) are consistent after the rebuild. No-op in release.
	verifyClauseIndices(s)
}

// deleteLearnedClauses removes low-quality learned clauses to control memory usage.
//
// Strategy: two-pass LBD-tiered deletion.
//   - Pass 1: delete LBD > 5 candidates first (lowest quality).
//   - Pass 2: if still over budget, delete LBD > 2 candidates.
//   - Glue clauses (LBD ≤ 2) are NEVER deleted.
//   - Reason clauses (currently used in the implication graph) are protected.
//   - Within a tier, candidates are in ascending clause-index order, so deletion
//     is FIFO (oldest learned first). This is deterministic and simple; a
//     decayed-activity sort was tested (B9) and regressed +33% PAR-2.
//
// Deletion trigger: when learnedActiveCount > deletionTriggerRatio × dynamicLimit
// (dynamicLimit = maxLearned + conflicts/dbGrowthDivisor). Target after deletion: dynamicLimit.
func (s *CDCLSolver) deleteLearnedClauses() {
	// LAZY LBD-BASED DELETION (Glucose-style)
	// Key insight: LBD is the best predictor of clause usefulness
	// - Keep all "glue" clauses (LBD ≤ 2) permanently
	// - Delete clauses with high LBD when database grows too large

	dynamicLimit := s.maxLearned + s.conflicts/s.dbGrowthDivisor
	if s.dbCapFactor > 0 && s.dbCapFactor != 1.0 {
		dynamicLimit = int(float64(dynamicLimit) * s.dbCapFactor)
	}
	targetCount := dynamicLimit

	currentActive := s.learnedActiveCount

	toDelete := currentActive - targetCount
	if toDelete <= 0 {
		return // Nothing to delete
	}

	// Mark protected clauses (used as implications) using bitmap
	// This avoids O(n*m) scanning
	protected := s.markProtectedClauses()

	// Use tmp buffer for deletion marks
	if cap(s.tmpDeleted) < s.learnedCapacity {
		s.tmpDeleted = make([]bool, s.learnedCapacity)
	}
	deleted := s.tmpDeleted[:s.learnedCapacity]
	for i := range deleted {
		deleted[i] = false
	}

	deletedCount := 0

	// Two-pass LBD-tiered deletion. Within each tier, candidates are sorted by
	// Activity ascending (lowest = delete first) with FIFO index tiebreak, so
	// the lowest-activity clauses are deleted first. Glue clauses (LBD ≤ 2)
	// are never deleted. When claActivityEnabled=false all Activity fields
	// are 0 and the index tieback reduces the sort to pure ascending index
	// order = FIFO (identical to pre-activity behavior).
	candidates := s.tmpDeletionCandidates[:0]

	// Pass 0 (length gate): if dbMaxLen>0, evict the longest clauses first.
	// The LBD passes below can never remove long near-glue clauses (LBD ≤ tier2
	// never deleted), so on long-clause instances the DB fills with them and
	// propagation gets slow. This pass prioritizes evicting any clause longer
	// than dbMaxLen. It is a deletion-time-only preference: the asserting learned
	// clause is still always added after a conflict, so search always progresses.
	//
	// Gated on binary-heavy formulas (binaryRatio > 0.5): these are formulas
	// (e.g., pigeonhole, parity) whose ORIGINAL clauses are small but whose
	// LEARNED DB bloats with long redundant near-glue clauses — pruning them is
	// pure win. Long-clause formulas (e.g. 274099073, ~99% long originals) need
	// their long learned clauses to drive search, so the gate must not touch
	// them. binaryRatio is cached by classifyInstance (runs before solving).
	if s.dbMaxLen > 0 && s.binaryRatio > 0.5 {
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedLoc[i].Size == 0 || protected[i] {
				continue
			}
			if int(s.learnedLoc[i].Size) > s.dbMaxLen {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) > 1 {
			sort.Slice(candidates, func(a, b int) bool {
				ia, ib := candidates[a], candidates[b]
				sa, sb := int(s.learnedLoc[ia].Size), int(s.learnedLoc[ib].Size)
				if sa != sb {
					return sa > sb // longest first
				}
				return ia < ib
			})
		}
		for _, idx := range candidates {
			if deletedCount >= toDelete {
				break
			}
			deleted[idx] = true
			deletedCount++
		}
		candidates = candidates[:0]
	}

	// Pass 1: collect LBD > tier1Threshold candidates (lowest quality), delete lowest-activity first
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size == 0 || protected[i] {
			continue
		}
		if int(s.learnedMetadata[i].LBD) > s.lbdTier1Threshold {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) > 1 {
		s.sortDeletionCandidates(candidates)
	}
	for _, idx := range candidates {
		if deletedCount >= toDelete {
			break
		}
		deleted[idx] = true
		deletedCount++
	}

	// Pass 2: lower threshold to LBD > tier2Threshold (glue clauses are LBD ≤ tier2Threshold, never deleted)
	if deletedCount < toDelete {
		candidates = candidates[:0]
		for i := 0; i < s.learnedCapacity; i++ {
			if s.learnedLoc[i].Size == 0 || protected[i] || deleted[i] {
				continue
			}
			if int(s.learnedMetadata[i].LBD) > s.lbdTier2Threshold {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) > 1 {
			s.sortDeletionCandidates(candidates)
		}
		for _, idx := range candidates {
			if deletedCount >= toDelete {
				break
			}
			deleted[idx] = true
			deletedCount++
		}
	}

	s.tmpDeletionCandidates = candidates

	// Apply tombstones: mark size=0 and remove watches
	tombstoneCount := 0
	for i := 0; i < s.learnedCapacity; i++ {
		if deleted[i] && s.learnedLoc[i].Size > 0 {
			// Remove watches BEFORE marking as tombstone
			s.removeLearnedClauseWatches(i)
			// Mark as tombstone
			s.learnedLoc[i].Size = 0
			s.learnedWatchIdx0[i] = -1
			s.learnedWatchIdx1[i] = -1
			tombstoneCount++
		}
	}

	// Update active count (excludes tombstones)
	activeCount := currentActive - deletedCount
	s.learnedActiveCount = activeCount

	// Rebuild unit clause list from scratch
	s.unitLearnedList = s.unitLearnedList[:0]
	for i := 0; i < s.learnedCapacity; i++ {
		if s.learnedLoc[i].Size == 1 {
			s.unitsDirty = true
			s.unitLearnedList = append(s.unitLearnedList, i)
		}
	}

	// Mark LBD order as dirty

	// Track tombstone ratio for logging
	tombstoneRatio := float64(tombstoneCount) / float64(s.learnedCapacity)

	// Request compaction at the next restart (level 0, where no learned
	// clause is in use as a reason) when too many tombstones have accumulated.
	// compactLearnedClauses reclaims the literal storage that tombstones leave
	// behind, bounding learnedLiterals growth on long-running instances.
	// Use ACCUMULATED tombstones (capacity - active), not just this call's
	// deletions: capacity grows monotonically while tombstones pile up, so the
	// per-call count understates the real waste.
	accumulated := s.learnedCapacity - s.learnedActiveCount
	if s.learnedCapacity > 0 && accumulated*3 >= s.learnedCapacity*2 { // >= ~33% tombstones
		s.compactPending = true
	}

	s.Log("c [verbose] Deleted %d learned clauses via LBD, kept %d active (tombstones=%d, ratio=%.1f%%)\n",
		deletedCount, activeCount, tombstoneCount, tombstoneRatio*100)
}

// sortDeletionCandidates sorts learned-clause deletion candidates in place by
// ascending activity (lowest quality first), with ascending clause index as the
// FIFO tiebreak (oldest learned first). See deleteLearnedClauses.
func (s *CDCLSolver) sortDeletionCandidates(candidates []int) {
	sort.Slice(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		aiAct := s.learnedMetadata[ai].Activity
		ajAct := s.learnedMetadata[aj].Activity
		if aiAct != ajAct {
			return aiAct < ajAct
		}
		return ai < aj
	})
}

// markProtectedClauses returns a bitmap over [0, learnedCapacity) flagging
// learned clauses currently used as reasons (implication sources). Such
// clauses must not be vivified, subsumed, or deleted while in use. The bitmap
// is backed by s.tmpClauseUsedAsReason and is zeroed before marking.
func (s *CDCLSolver) markProtectedClauses() []bool {
	if cap(s.tmpClauseUsedAsReason) < s.learnedCapacity {
		s.tmpClauseUsedAsReason = make([]bool, s.learnedCapacity)
	}
	protected := s.tmpClauseUsedAsReason[:s.learnedCapacity]
	for i := range protected {
		protected[i] = false
	}
	for _, asg := range s.assignments {
		impIdx := asg.Reason
		if impIdx <= -5 {
			learnedIdx := -impIdx - 5
			if int(learnedIdx) < s.learnedCapacity {
				protected[learnedIdx] = true
			}
		}
	}
	return protected
}
