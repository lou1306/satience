package solver

import (
	"satience/internal/cnf"
)

// Conflict analysis, clause learning, and learned-clause
// minimization/LBD computation. Split out of solver_cdcl.go for structure;
// same CDCLSolver state, no behavior change.

// exploreRemovable is the recursive core of Bier's algorithm. It does NOT roll
// back on failure — that is the caller's job (recursiveTryRemove) via the
// snapshot. Returns true iff every non-resolved literal in v's reason clause is
// covered (tmpSeenVar) or recursively removable within minimizeMaxDepth. Sound
// via the DAG property of the implication graph (reason clauses only reference
// earlier trail literals, preventing cycles).
func (s *CDCLSolver) exploreRemovable(v uint32, depth int) bool {
	if s.minimizeMaxDepth > 0 && depth > s.minimizeMaxDepth {
		return false
	}
	reasonLits := s.getReasonLitsForVar(v)
	if reasonLits == nil {
		return false
	}
	if len(reasonLits) <= 1 {
		return false
	}
	for _, rl := range reasonLits {
		rv := rl.Var()
		if rv == v {
			continue
		}
		if s.tmpSeenVar[rv] {
			continue
		}
		// Level-0 literals are globally assigned (forced during preprocessing,
		// never cleared by backjump). A level-0 literal in a reason clause is
		// permanently false, so it is automatically "covered" — skip it without
		// failing. Returning false here (the old behavior) devastated long-clause
		// instances: reason clauses averaging 50-150 literals almost always contain
		// a level-0 literal, causing ~99% of minimization attempts to fail. Skipping
		// matches Cadical/MiniSat (`if (!level(tmp)) continue;`). Soundness holds:
		// the reason clause still propagates v after backjump because the level-0
		// literal remains false. No marking needed (nothing is pushed to
		// tmpMinSeenVars), so the snapshot/rollback in recursiveTryRemove is
		// unaffected. Level-0 literals are still protected from *removal* in
		// minimizeLearnedClause (line 3931) — this only affects whether a reason
		// clause *containing* a level-0 literal blocks removability of another var.
		if s.assignments[rv].Level == 0 {
			continue
		}
		if s.assignments[rv].Reason == -1 {
			return false
		}
		s.tmpSeenVar[rv] = true
		s.tmpMinSeenVars = append(s.tmpMinSeenVars, rv)
		if !s.exploreRemovable(rv, depth+1) {
			return false
		}
	}
	return true
}

// recursiveTryRemove attempts to prove that literal v (currently in the learned
// clause) is redundant: every other literal in v's reason clause is either
// already covered (in the clause or proven removable) or recursively removable.
// On success returns true (v may be dropped). On failure returns false and
// rolls back ALL marks pushed during this call via a snapshot of tmpMinSeenVars,
// so failed explorations leave no stale "covered" marks (this correctly handles
// the diamond case where a prior successful sub-exploration is invalidated by a
// later sibling failure).
func (s *CDCLSolver) recursiveTryRemove(v uint32) bool {
	snapshot := len(s.tmpMinSeenVars)
	result := s.exploreRemovable(v, 0)
	if !result {
		// Roll back: unmark everything pushed during this call.
		for i := snapshot; i < len(s.tmpMinSeenVars); i++ {
			s.tmpSeenVar[s.tmpMinSeenVars[i]] = false
		}
		s.tmpMinSeenVars = s.tmpMinSeenVars[:snapshot]
	}
	return result
}

// minimizeLearnedClause reduces the size of a learned clause via recursive
// self-subsumption (MiniSat/Bier-style). A literal is removable if every other
// literal in its reason clause is itself either already in the learned clause
// or recursively removable. The UIP (single current-level literal) and level-0
// literals are never removed — the clause must stay asserting and level-0
// literals are global.
//
// Uses two mark arrays:
//   - tmpLiteralInClause[v]: literal is currently in the clause (drives rebuild)
//   - tmpSeenVar[v]: literal is "covered" (in clause OR proven-removable OR
//     being-explored). Set during exploration; cleared on failed branches via
//     snapshot rollback; cleared in full at the end via tmpMinSeenVars.
//
// Soundness rests on the CDCL resolution invariant: a literal removable via its
// reason clause yields a valid resolution consequence, so dropping it produces a
// strictly stronger (or equal) clause. Termination is guaranteed by the DAG
// property of the implication graph within a decision level (reason clauses only
// reference earlier trail literals). minimizeMaxDepth is a defensive cap.
func (s *CDCLSolver) minimizeLearnedClause(learnedLits []cnf.Literal) []cnf.Literal {
	if len(learnedLits) <= 2 {
		return learnedLits
	}

	s.minimizeCalls++
	s.minimizeLiteralsIn += uint64(len(learnedLits))
	// Phase A: mark clause literals in both arrays and record for cleanup.
	// Also set tmpLiteralIsNegated for each literal — needed by exploreRemovable
	// to look up binary implications for the correct polarity.
	s.tmpMinSeenVars = s.tmpMinSeenVars[:0]
	for _, lit := range learnedLits {
		v := lit.Var()
		s.tmpLiteralInClause[v] = true
		s.tmpLiteralIsNegated[v] = lit.IsNegated()
		if !s.tmpSeenVar[v] {
			s.tmpSeenVar[v] = true
			s.tmpMinSeenVars = append(s.tmpMinSeenVars, v)
		}
	}

	// Phase B: try to remove each non-protected literal.
	reductionAchieved := 0
	for _, lit := range learnedLits {
		v := lit.Var()
		if !s.tmpLiteralInClause[v] {
			continue // already removed in this pass
		}
		// Protect the UIP / any current-level literal: removing it would make
		// the clause non-asserting.
		if s.assignments[v].Level == int32(s.level) {
			continue
		}
		// Protect level-0 literals: they are global and never removable.
		if s.assignments[v].Level == 0 {
			continue
		}
		// Skip decisions (no reason clause to resolve against).
		if s.assignments[v].Reason == -1 {
			continue
		}
		// BIG fast-path: check if lit is removable via a chain of binary
		// clauses. A forward BIG path lit → m1 → ... → mk with mk currently in
		// the clause yields a valid resolution derivation of C\{lit} from C
		// (resolve C with each (¬mi ∨ mi+1) on the path), so lit is removable.
		// Soundness does NOT require the intermediate literals to be assigned:
		// the derivation uses only the binary clauses, which are in the formula.
		// Intermediate tautologies in the resolvent are harmless — only the
		// final clause (C\{lit}) matters, and it is non-tautological.
		if s.bigAdjOff != nil && !s.bigDisabled &&
			!(s.structureScore < 0.7 && s.binaryRatio > 0.4) { // GATE: disable BIG for mixed-binary sub-0.7
			s.bigMinimizeCalls++
			if s.bigReachableInClause(lit) {
				s.bigMinimizeHits++
				s.recordBigOutcome(true)
				s.tmpLiteralInClause[v] = false
				// CRITICAL: Do NOT leave tmpSeenVar[v]=true for BIG-removed literals.
				// Recursive minimization treats tmpSeenVar as a "covered" set (literal
				// is removable, so can be skipped in reason clauses). But BIG removal
				// is only valid while the BFS target remains in the clause. If recursive
				// later removes the target, the BIG removal becomes invalid. Leaving
				// tmpSeenVar[v]=true creates a circular dependency: lit1 is "covered"
				// (BIG, depends on lit2), and lit2 is removable because lit1 is "covered".
				// This circular reasoning produces unsound minimization → false UNSAT.
				// Clearing tmpSeenVar[v] forces recursive to re-examine v independently.
				s.tmpSeenVar[v] = false
				reductionAchieved++
				continue
			}
			// Sliding-window hit-rate gate: disable BIG once the recent hit rate
			// (measured over the last bigHitWindow attempts) falls below
			// bigMinHitRate. The old cumulative gate only fired at total-hits==0,
			// so a low-but-nonzero hit rate (e.g. 0.5% on large timetabling
			// encodings) kept the ~99.5%-overhead BFS alive for the whole solve.
			s.recordBigOutcome(false)
		}
		if s.recursiveTryRemove(v) {
			s.tmpLiteralInClause[v] = false
			reductionAchieved++
			// tmpSeenVar[v] stays true — v is now "covered" for downstream
			// checks in the same pass (it's removable given prior removals).
		}
	}

	// Phase C: compact in-place, keeping literals still marked.
	writeIdx := 0
	for _, lit := range learnedLits {
		if s.tmpLiteralInClause[lit.Var()] {
			learnedLits[writeIdx] = lit
			writeIdx++
		}
	}

	// Phase D: clear tmpSeenVar marks recorded in tmpMinSeenVars. Also clear
	// tmpLiteralInClause (defensive — the next-conflict cleanup at learnClause
	// would do it, but keeping the buffer clean here is safer and matches the
	// pre-minimization invariant).
	for _, v := range s.tmpMinSeenVars {
		s.tmpSeenVar[v] = false
	}
	for _, lit := range learnedLits {
		s.tmpLiteralInClause[lit.Var()] = false
	}
	s.tmpMinSeenVars = s.tmpMinSeenVars[:0]

	if s.verbose && reductionAchieved > 0 {
		s.Log("c [minimize] Reduced: %d→%d literals (removed %d)\n",
			len(learnedLits), writeIdx, reductionAchieved)
	}

	s.minimizeLiteralsOut += uint64(writeIdx)
	return learnedLits[:writeIdx]
}

// storeLearnedClause stores the learned clause in the database, sets up watches,
// and bumps VSIDS. Returns false if a duplicate-literal soundness bug was
// detected (caller returns backjump level 0). Returns true on success or
// when there is nothing to store (empty tmpLearnedLits).
func (s *CDCLSolver) storeLearnedClause(lbd int) bool {
	// Store learned clause in database.
	// MiniSat stores ALL learned clauses and uses LBD for deletion priority,
	// not for initial storage. Filtering by LBD at learning time creates a
	// vicious cycle: high-LBD clauses are discarded → no learning → same
	// conflicts repeat → LBD stays high → no learning. On small structured
	// instances (e.g. 44092fcc, 90v), this caused 20K+ conflicts with 0
	// stored clauses.
	if len(s.tmpLearnedLits) > 0 {
		// Check for duplicate literals (SOUNDNESS CHECK)
		hasDup := false
		for _, lit := range s.tmpLearnedLits {
			if s.tmpSeenVar[lit.Var()] {
				hasDup = true
				break
			}
			s.tmpSeenVar[lit.Var()] = true
		}
		// Reset
		for _, lit := range s.tmpLearnedLits {
			s.tmpSeenVar[lit.Var()] = false
		}
		if hasDup {
			if s.verbose {
				s.Log("c [SOUNDNESS BUG] Learned clause has duplicate literals: conflict=%d, clause: ", s.conflicts)
				for _, lit := range s.tmpLearnedLits {
					s.Log("%d%c ", lit.Var()+1, map[bool]byte{true: '-', false: '+'}[lit.IsNegated()])
					s.Log("\n")
				}
			}
			// Skip storing this buggy clause
			return false
		}

		// Store literals in contiguous pool
		// NOTE: Slot reuse disabled - swap-remove moves clauses but literals stay in place,
		// causing corruption when freed slots are reused
		offset := len(s.learnedLiterals)
		s.learnedLiterals = append(s.learnedLiterals, s.tmpLearnedLits...)

		// Append metadata (packed struct for cache efficiency)
		s.learnedLoc = append(s.learnedLoc, LearnedClauseLoc{
			Offset: int32(offset),
			Size:   int32(len(s.tmpLearnedLits)),
		})
		s.recordLearnedClauseSize(len(s.tmpLearnedLits))
		// Fresh clauses start at Activity=0 (MiniSat convention). They only
		// gain activity when used as reasons during 1-UIP resolution. With
		// Activity=0, the sort's index tiebreak handles fresh-clause
		// protection (they have high indices, deleted last within Activity=0
		// tier = FIFO protection). When claActivityEnabled=false, Activity
		// stays 0 (pure FIFO fallback).
		s.learnedMetadata = append(s.learnedMetadata, cnf.ClauseMetadata{
			LBD: int32(lbd),
		})
		s.learnedSearchHint = append(s.learnedSearchHint, 0) // Fresh clause: no hint yet
		s.learnedActiveCount++
		s.learnedCapacity++

		// Add to watches
		// CRITICAL: Use learnedCapacity - 1 (the index of the just-appended data),
		// NOT learnedActiveCount - 1. After deleteLearnedClauses decrements
		// learnedActiveCount, using learnedActiveCount - 1 would point to a
		// tombstoned slot instead of the newly appended clause data, causing
		// duplicate watches and stale watch corruption.
		learnedIdx := s.learnedCapacity - 1
		literals := s.getLearnedClauseLiterals(learnedIdx)
		s.lastLearnedClauseIdx = learnedIdx
		// Register learned binary clauses into the dynamic BIG for stronger
		// transitive minimization (sound even after deletion — see
		// addLearnedBinaryToBIG).
		if len(literals) == 2 && !s.bigDisabled {
			s.addLearnedBinaryToBIG(literals[0], literals[1])
		}

		// Store watch indices for all clauses to maintain array consistency
		if len(literals) >= 2 {
			var idx0, idx1 int
			if s.watchInitialized {
				// Watch positions 0 and 1 directly (UIP at 0, backjump-level at 1).
				// The asserting-clause invariant guarantees position 0 (UIP) is
				// unassigned and position 1 is false after backjump, satisfying the
				// watched-literal invariant (at most one watched literal is false).
				lit0 := literals[0]
				lit1 := literals[1]
				idx0 = cnf.LitToIndex(lit0)
				idx1 = cnf.LitToIndex(lit1)
				clauseIdx0 := int32(watchLearnedBit | uint32(learnedIdx))
				clauseIdx1 := clauseIdx0 | int32(watchMyPosBit)
				if len(literals) == 2 {
					s.appendWatch(idx0, cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit1)}, true)
					s.appendWatch(idx1, cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit0)}, true)
				} else {
					s.appendWatch(idx0, cnf.Watch{ClauseIdx: clauseIdx0, Blit: litToBlit(lit1)}, false)
					s.appendWatch(idx1, cnf.Watch{ClauseIdx: clauseIdx1, Blit: litToBlit(lit0)}, false)
				}
			} else {
				// Watches not yet initialized (preprocessing); store positions 0,1
				// as placeholder — initWatches will choose correct positions later
				idx0 = cnf.LitToIndex(literals[0])
				idx1 = cnf.LitToIndex(literals[1])
			}
			s.learnedWatchIdx0 = append(s.learnedWatchIdx0, idx0)
			s.learnedWatchIdx1 = append(s.learnedWatchIdx1, idx1)
		} else {
			// Unit clause: use sentinel values
			s.learnedWatchIdx0 = append(s.learnedWatchIdx0, -1)
			s.learnedWatchIdx1 = append(s.learnedWatchIdx1, -1)
		}

		// Track unit clauses for O(1) propagation (OPTIMIZATION #1)
		if len(literals) == 1 {
			s.unitsDirty = true
			s.unitLearnedList = append(s.unitLearnedList, learnedIdx)
			s.rootUnitsLearned++ // trigger A: newly-discovered root fact
		}

		// VSIDS bump
		s.vsids.bumpLBD(s.tmpLearnedLits, lbd)
	}
	return true
}

func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) int {
	s.lastLearnedClauseIdx = -1
	if s.verbose && s.conflicts <= DebugConflictLimit {
		s.Log("c [conflict] Conflict %d, iter %d, level %d, learned %d, trail %d\n",
			s.conflicts, s.iterations, s.level, s.learnedActiveCount, len(s.trail))
	}

	// Fast cleanup from previous conflict
	for _, varIdx := range s.tmpTouchedVars {
		s.tmpLiteralInClause[varIdx] = false
		s.tmpLiteralIsNegated[varIdx] = false
	}
	for _, varIdx := range s.tmpResolvedVars {
		s.tmpResolved[varIdx] = false
	}
	s.tmpResolvedVars = s.tmpResolvedVars[:0]
	for _, lvl := range s.tmpLevelSet {
		s.tmpLevelCount[lvl] = 0
		s.tmpLevelSetUsed[lvl] = false
		s.tmpLevelCountUsed[lvl] = false
	}
	s.tmpTouchedVars = s.tmpTouchedVars[:0]
	s.tmpCandidates = s.tmpCandidates[:0]
	s.tmpLevelSet = s.tmpLevelSet[:0]

	currentCount := s.runOneUIPResolution(conflictLits)

	// Bump variables touched during 1-UIP analysis. bumpAnalyze (minisat
	// analyze_toclear) bumps ALL touched variables including intermediate
	// resolved vars; bumpClause bumps only the conflict clause vars. bumpAnalyze
	// is gated to default-decay instances — under aggressive decay (0.30→0.60)
	// the fast varInc growth flattens the VSIDS signal when distributed across
	// Must run before the currentCount != 1 early return so degenerate
	// conflicts still bump involved variables.
	if s.useBumpAnalyze {
		s.vsids.bumpAnalyze(s.tmpTouchedVars)
	} else {
		s.vsids.bumpClause(conflictLits)
	}

	// NOTE: currentCount != 1 means we do NOT have a genuine 1-UIP asserting
	// clause, so we skip learning and backjump conservatively:
	//   - currentCount == 0: resolution canceled all literal at the current
	//     decision level -> non-asserting, no literal to propagate. Skip.
	//   - currentCount > 1: resolution did not converge, i.e. reason clauses
	//     were inconsistent. Learning the partially-derived clause would risk
	//     an un-entailed clause (false UNSAT), so skip it (see runOneUIPResolution).
	// The produced clause (sound or not) would be non-asserting anyway, so
	// skipping it loses nothing real; backjump is always sound.
	if currentCount != 1 {
		// Compute maxLevel from remaining literals for a better backjump target
		bjLevel := 0
		for _, varIdx := range s.tmpTouchedVars {
			if s.tmpLiteralInClause[varIdx] {
				lvl := int(s.assignments[varIdx].Level)
				if lvl > 0 && lvl < s.level && lvl > bjLevel {
					bjLevel = lvl
				}
			}
		}
		if bjLevel == 0 {
			bjLevel = s.level - 1
			if bjLevel < 0 {
				bjLevel = 0
			}
		}
		return bjLevel
	}

	// Calculate LBD and backjump level
	// CRITICAL FIX: Level 0 is preprocessing - not a decision level for LBD
	lbd := 0
	maxLevel := 0
	// Build learned clause — exclude level-0 literals (always-true root facts).
	// Level-0 literals are preprocessing assignments and root-level learned units.
	// They are always true during search, so including them in learned clauses
	// wastes storage and watch slots without adding constraint value.
	//
	// LBD/maxLevel accumulation and learned-literal construction iterate the same
	// tmpLiteralInClause-filtered touched set and read the same assignments[].Level,
	// so fold them into a single pass (compute lvl once) instead of two O(touched)
	// scans per conflict.
	s.tmpLearnedLits = s.tmpLearnedLits[:0]
	for _, varIdx := range s.tmpTouchedVars {
		if s.tmpLiteralInClause[varIdx] {
			lvl := int(s.assignments[varIdx].Level)
			s.tmpLiteralInClause[varIdx] = false
			if lvl > 0 {
				if !s.tmpLevelSetUsed[lvl] {
					s.tmpLevelSetUsed[lvl] = true
					s.tmpLevelSet = append(s.tmpLevelSet, lvl)
					lbd++
				}
				s.tmpLearnedLits = append(s.tmpLearnedLits, cnf.NewLiteral(varIdx, s.tmpLiteralIsNegated[varIdx]))
			}
			if lvl > maxLevel && lvl < s.level {
				maxLevel = lvl
			}
		}
	}

	// NOTE: resolvedCount == 0 is normal when all conflict literals are decisions.
	// The learned clause IS useful - it prevents this exact combination of decisions.
	// Do NOT skip learning in this case - that would cripple the solver.

	// CRITICAL: Verify 1-UIP property to catch soundness bugs
	// If verification fails, the learned clause is invalid - skip learning (safer than wrong clause)
	// This point is reached only for a genuine 1-UIP (currentCount == 1).
	if !s.verifyLearnedClause(s.tmpLearnedLits, false) {
		if s.verbose {
			s.Log("c [learnClause] Skipping buggy learned clause due to 1-UIP violation\n")
		}
		return maxLevel
	}

	// Empty clause = UNSAT
	if len(s.tmpLearnedLits) == 0 {
		if s.verbose {
			s.Log("c [learnClause] Empty clause learned - UNSAT\n")
		}
		s.emptyClauseFound = true
		return 0
	}

	// CRITICAL FIX: Apply clause minimization via self-subsumption
	// This reduces learned clause size and LBD, enabling propagation
	// Disabled for clauses ≤2 literals (already minimal). originalSize and
	// originalLBD feed ONLY the verbose minimize log inside this branch, so
	// capture them here rather than per-learn unconditionally.
	//
	// A/B LBD gate (SetMinimizeLBDGate): skip the expensive BIG BFS + recursive
	// minimization for high-LBD clauses that reduceDB will evict anyway. Falling
	// through leaves the clause unminimized (higher LBD, deleted sooner) — always
	// sound, only trajectory/quality change. Low-LBD clauses are unaffected.
	// The gate must cover the whole block (including the post-minimize LBD
	// recompute) so lbd stays the unminimized (pre-minimize) value when skipped.
	if len(s.tmpLearnedLits) > 2 &&
		(s.minimizeLBDGate <= 0 || lbd <= s.minimizeLBDGate) {
		originalSize := len(s.tmpLearnedLits)
		originalLBD := lbd
		s.tmpLearnedLits = s.minimizeLearnedClause(s.tmpLearnedLits)

		// Recalculate LBD after minimization (CRITICAL - LBD may have decreased)
		// Must reset tmpLevelSetUsed since it was used for original LBD calculation.
		// Reset only the levels that were marked (tracked in tmpLevelSet), avoiding
		// an O(NumVars) sweep per conflict on the hot learnClause path.
		lbd = 0
		maxLevel = 0
		for _, lvl := range s.tmpLevelSet {
			s.tmpLevelSetUsed[lvl] = false
		}
		for _, lit := range s.tmpLearnedLits {
			varIdx := lit.Var()
			lvl := int(s.assignments[varIdx].Level)
			if lvl >= 0 {
				if !s.tmpLevelSetUsed[lvl] {
					s.tmpLevelSetUsed[lvl] = true
					lbd++
				}
				if lvl > maxLevel && lvl < s.level {
					maxLevel = lvl
				}
			}
		}

		if s.verbose && originalSize > len(s.tmpLearnedLits) {
			s.Log("c [minimize] Reduced: %d→%d literals, LBD %d→%d\n",
				originalSize, len(s.tmpLearnedLits), originalLBD, lbd)
		}
	}

	s.lbdSum += lbd
	s.lbdCount++
	s.emaLBD = 0.9*s.emaLBD + 0.1*float64(lbd)
	s.totalLbdSum += uint64(lbd)
	s.totalLbdCount++
	// Glue clause (LBD ≤ 2) counter, consumed by the behavioral governor.
	if lbd <= 2 {
		s.glueLearned++
	}

	// Backjump level = second-highest in learned clause (= maxLevel)
	// SPECIAL CASE: If learned clause is unit (1 literal) at current level, backjump to level 0
	// to flip the decision. The unit literal represents a constraint that must be satisfied.
	backjumpLevel := maxLevel

	if s.verbose && s.conflicts <= 10 {
		s.Log("c [LEARNED CLAUSE] ")
		for _, lit := range s.tmpLearnedLits {
			s.Log("%d%c ", lit.Var()+1, map[bool]byte{true: '-', false: '+'}[lit.IsNegated()])
		}
		s.Log("0 (LBD=%d, backjump=%d)\n", lbd, backjumpLevel)
	}
	if len(s.tmpLearnedLits) == 1 {
		// Unit clause: check if the literal's variable is at current level
		lit := s.tmpLearnedLits[0]
		if s.assignments[lit.Var()].Level == int32(s.level) {
			// Unit literal at current level: backjump to 0 to flip the decision
			backjumpLevel = 0
		} else if backjumpLevel == 0 {
			backjumpLevel = 1
		}
	} else if backjumpLevel == 0 {
		backjumpLevel = 1
	}

	if s.verbose {
		s.Log("c   FINAL: %d literals, LBD=%d, backjump=%d\n", len(s.tmpLearnedLits), lbd, backjumpLevel)
	}

	// Check for tautologies (both polarities of same variable)
	// This can happen due to bugs in conflict analysis
	for _, lit := range s.tmpLearnedLits {
		varIdx := lit.Var()
		if s.tmpSeenVar[varIdx] {
			// Tautology detected - skip learning this clause
			if s.verbose {
				s.Log("c [learnClause] TAUTOLOGY detected in learned clause - skipping\n")
			}
			// Reset before returning
			for _, l := range s.tmpLearnedLits {
				s.tmpSeenVar[l.Var()] = false
			}
			return backjumpLevel
		}
		s.tmpSeenVar[varIdx] = true
	}
	// Reset
	for _, lit := range s.tmpLearnedLits {
		s.tmpSeenVar[lit.Var()] = false
	}

	// Check for conflicting unit clauses
	if len(s.tmpLearnedLits) == 1 {
		lit := s.tmpLearnedLits[0]
		varIdx := lit.Var()
		litValue := !lit.IsNegated()
		if s.verbose {
			s.Log("c [learnClause] Learning unit: var=%d, value=%v\n", varIdx+1, litValue)
		}
		// Check if an opposite unit already exists among learned units.
		// unitLearnedList is maintained at store/delete/compact/vivify/restart,
		// so it's current here. This replaces the prior O(learnedCapacity)
		// scan over all learned clauses.
		for _, existingIdx := range s.unitLearnedList {
			existingLits := s.getLearnedClauseLiterals(existingIdx)
			if len(existingLits) != 1 {
				continue
			}
			existingLit := existingLits[0]
			if existingLit.Var() == varIdx {
				existingValue := !existingLit.IsNegated()
				if existingValue != litValue {
					// Conflicting unit found - UNSAT
					if s.verbose {
						s.Log("c [learnClause] Conflicting unit on var %d: existing=%v, new=%v - UNSAT\n", varIdx+1, existingValue, litValue)
					}
					s.emptyClauseFound = true
					return 0
				}
			}
		}
	}

	// Reorder so the UIP (current-level literal) is at position 0 and a
	// backjump-level literal is at position 1 (MiniSat asserting-clause
	// invariant). This lets us watch positions 0/1 directly and propagate
	// the asserting literal explicitly after backjump (qhead=decisionPoint),
	// avoiding an O(trail) re-scan of the earlier trail after each conflict.
	if len(s.tmpLearnedLits) >= 2 {
		uipPos := 0
		for i, lit := range s.tmpLearnedLits {
			if s.assignments[lit.Var()].Level == int32(s.level) {
				uipPos = i
				break
			}
		}
		if uipPos != 0 {
			s.tmpLearnedLits[0], s.tmpLearnedLits[uipPos] = s.tmpLearnedLits[uipPos], s.tmpLearnedLits[0]
		}
		for i := 1; i < len(s.tmpLearnedLits); i++ {
			if s.assignments[s.tmpLearnedLits[i].Var()].Level == int32(maxLevel) {
				if i != 1 {
					s.tmpLearnedLits[1], s.tmpLearnedLits[i] = s.tmpLearnedLits[i], s.tmpLearnedLits[1]
				}
				break
			}
		}
	}

	if !s.storeLearnedClause(lbd) {
		return 0
	}

	return backjumpLevel
}

// learnClause performs 1-UIP conflict analysis to learn a new clause

// 1-UIP (First Unique Implication Point) Algorithm:
// The goal is to find the earliest point in the implication graph where the
// conflict can be explained with exactly one literal at the current decision level.

// Algorithm:
// 1. Start with the conflict clause (all literals are false)
// 2. While there is more than one literal at current level:
//    - Pick the most recently decided literal at current level
//    - Resolve with its reason clause (the clause that forced it)
//    - This eliminates the literal and adds the reason's literals
// 3. The result is the 1-UIP learned clause with exactly one literal at current level

// Why 1-UIP?
// - Produces shorter, more general learned clauses than other schemes
// - The UIP literal is the "bottleneck" through which all paths to conflict pass
// - Backjumping to the second-highest level in the learned clause is sound

// Example:
// Decision: x=1, y=1, z=1 (level 3)
// Propagate: ¬x∨¬y∨a, a=0 (level 3)
// Propagate: ¬a∨¬z∨b, b=0 (level 3)
// Conflict: ¬b∨¬z (both false at level 3)

// Resolution:
// Start: {b, z} (conflict clause)
// Resolve on b with reason (¬a∨¬z∨b): {z, ¬a, ¬z} = {¬a} (z cancels)
// Now only ¬a at level 3 - this is the 1-UIP!
// Learned clause: (a ∨ ¬z) - backjump to level of ¬z

// Backjump Level Calculation:
// The backjump level is the second-highest decision level in the learned clause.
// This is the highest level we can backjump to while still preventing the conflict.
// We backjump to this level and flip the decision there.
//
// Asserting Clause Invariant:
// After analysis, the learned clause is reordered so the UIP (single current-level
// literal) is at position 0 and a backjump-level literal is at position 1. Positions
// 0/1 are watched directly. After backjump, qhead=decisionPoint skips re-processing
// the earlier trail; the asserting literal is propagated explicitly by
// propagateAssertingLiteral() since the watched literals sit at trail positions
// < decisionPoint.

// LBD (Literal Block Distance):
// LBD = number of distinct decision levels in the learned clause.
// Lower LBD = better clause (involves fewer decision levels).
// Clauses with LBD=2 are "glue clauses" - most valuable, never delete.
// runOneUIPResolution performs 1-UIP conflict analysis: it marks the conflict
// clause literals, then resolves on candidates from the current decision level
// until exactly one literal (the UIP) remains. If resolution does not converge
// (inconsistent reason clauses), a fallback forces the most recent literal as
// the UIP. Returns currentCount (number of literals at the current level after
// resolution; 0 = non-asserting, 1 = asserting).
func (s *CDCLSolver) runOneUIPResolution(conflictLits []cnf.Literal) (currentCount int) {
	// Add conflict clause literals
	if s.verbose {
		s.Log("c [1-UIP] ===== Conflict %d: %d literals at level %d =====\n", s.conflicts, len(conflictLits), s.level)
		for i, lit := range conflictLits {
			s.Log("c   INIT[%d]: var=%d, neg=%v, level=%d\n", i, lit.Var()+1, lit.IsNegated(), s.assignments[lit.Var()].Level)
		}
	}
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !s.tmpLiteralInClause[varIdx] {
			s.tmpLiteralInClause[varIdx] = true
			s.tmpLiteralIsNegated[varIdx] = lit.IsNegated()
			s.tmpTouchedVars = append(s.tmpTouchedVars, varIdx)
			lvl := int(s.assignments[varIdx].Level)
			if lvl >= 0 && lvl <= s.level {
				if !s.tmpLevelCountUsed[lvl] {
					s.tmpLevelCountUsed[lvl] = true
					s.tmpLevelSet = append(s.tmpLevelSet, lvl)
				}
				s.tmpLevelCount[lvl]++
			}
		}
	}

	// 1-UIP: Resolve until exactly 1 literal at current level
	currentCount = s.tmpLevelCount[s.level]

	// Resolve on candidates until 1 UIP remains. The O(current-level trail)
	// reverse-scan below feeds ONLY the resolution loop (which runs only when
	// currentCount > 1). When currentCount <= 1 the conflict is already
	// asserting/non-asserting and the candidate list is never read, so gate the
	// scan inside the branch to avoid the per-conflict waste on the dominant path.
	if currentCount > 1 {
		// Build candidate list from trail (most recent first). Only the current
		// decision level's trail slice can contain level==s.level literals (the
		// level starts at trailHead[s.level]), so scan that slice only — O(current-
		// level trail) instead of O(total trail) per conflict.
		s.tmpCandidates = s.tmpCandidates[:0]
		startIdx := s.trailHead[s.level]
		for i := len(s.trail) - 1; i >= startIdx; i-- {
			varIdx := s.trail[i]
			if s.assignments[varIdx].Level == int32(s.level) && s.tmpLiteralInClause[varIdx] {
				s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx: varIdx})
			}
		}
	}
	candidateIdx := 0
	resolveStep := 0
	resolvedCount := 0
	for currentCount > 1 && candidateIdx < len(s.tmpCandidates) {
		candidate := s.tmpCandidates[candidateIdx]
		candidateIdx++
		varIdx := candidate.varIdx

		// Skip resolved variables (prevents re-resolution cycles when the
		// trail-order invariant is violated by inconsistent reason clauses).
		if s.tmpResolved[varIdx] {
			continue
		}
		// Only resolve variables currently in the clause (a var may have been
		// cancelled by a prior resolution and is no longer present — resolving it
		// would produce an unsound super-clause and corrupt currentCount).
		if !s.tmpLiteralInClause[varIdx] {
			continue
		}

		reasonClauseIdx := s.assignments[varIdx].Reason
		// Clause-activity bump: each learned reason clause traversed during
		// 1-UIP resolution gets claInc added to its Activity (MiniSat
		// claBumpEvent equivalent). Only learned clauses (impIdx <= -5) are
		// bumped; original clauses and preprocessing sentinels (-2..-4) are
		// skipped. Reason clauses are protected from deletion while in use,
		// so the bump only matters once the clause becomes a candidate. When
		// claActivityEnabled=false this is a no-op (Activity stays 0).
		if s.claActivityEnabled && reasonClauseIdx <= -5 {
			s.learnedMetadata[-reasonClauseIdx-5].Activity += s.claInc
		}
		if reasonClauseIdx == -1 {
			continue
		}
		if s.assignments[varIdx].Level == 0 {
			continue
		}

		// Get reason clause literals via shared helper
		reasonLits := s.getReasonLitsForVar(varIdx)
		if reasonLits == nil {
			continue
		}

		// DEBUG: Trace resolution step
		if s.verbose && s.conflicts >= 4990 {
			s.Log("c [1-UIP TRACE] Resolving on var %d (level %d)\n", varIdx+1, s.assignments[varIdx].Level)
			s.Log("c   Current clause: ")
			for v := range s.tmpLiteralInClause {
				if s.tmpLiteralInClause[v] {
					s.Log("%d%c ", v+1, map[bool]byte{true: '-', false: '+'}[s.tmpLiteralIsNegated[v]])
				}
			}
			s.Log("0\n")
			s.Log("c   Reason clause (idx=%d): ", reasonClauseIdx)
			for _, rl := range reasonLits {
				s.Log("%d%c ", rl.Var()+1, map[bool]byte{true: '-', false: '+'}[rl.IsNegated()])
			}
			s.Log("0\n")
		}

		// Resolve: remove varIdx, add reason literals
		s.tmpLiteralInClause[varIdx] = false
		s.tmpResolved[varIdx] = true
		s.tmpResolvedVars = append(s.tmpResolvedVars, varIdx)
		s.tmpLevelCount[s.level]--
		currentCount--
		resolvedCount++

		for _, lit := range reasonLits {
			v := lit.Var()
			litNegated := lit.IsNegated()

			// Skip the resolved variable - its negation in the reason cancels with the original
			if v == varIdx {
				if s.verbose {
					s.Log("c [1-UIP]   Skip resolved var %d\n", v+1)
				}
				continue
			}
			if s.tmpLiteralInClause[v] {
				if s.tmpLiteralIsNegated[v] != litNegated {
					// Cancel: remove from clause
					if s.verbose {
						s.Log("c [1-UIP]   CANCEL var %d (neg=%v vs %v)\n", v+1, s.tmpLiteralIsNegated[v], litNegated)
					}
					s.tmpLiteralInClause[v] = false
					s.tmpLevelCount[int(s.assignments[v].Level)]--
					if s.assignments[v].Level == int32(s.level) {
						currentCount--
					}
				} else {
					if s.verbose {
						s.Log("c [1-UIP]   Keep var %d (same polarity)\n", v+1)
					}
				}
			} else {
				// Only add assigned literals (level > 0)
				// Level-0 literals are always true (root-level units) — including them
				// in learned clauses makes them longer with no benefit.
				assignLevel := int(s.assignments[v].Level)
				if assignLevel <= 0 {
					continue
				}
				// Skip already-resolved variables: their reason was already processed,
				// so re-adding them would inflate currentCount without the ability to
				// resolve them again (tmpResolved blocks re-resolution). This is the
				// root cause of 1-UIP non-convergence when the trail-order invariant
				// is violated by inconsistent reason clauses.
				if s.tmpResolved[v] {
					continue
				}

				// Add to clause
				if s.verbose {
					s.Log("c [1-UIP]   ADD var %d, neg=%v, level=%d\n", v+1, litNegated, assignLevel)
				}
				s.tmpLiteralInClause[v] = true
				s.tmpLiteralIsNegated[v] = litNegated
				s.tmpTouchedVars = append(s.tmpTouchedVars, v)
				lvl := assignLevel
				if lvl <= s.level {
					if !s.tmpLevelCountUsed[lvl] {
						s.tmpLevelCountUsed[lvl] = true
						s.tmpLevelSet = append(s.tmpLevelSet, lvl)
					}
					s.tmpLevelCount[lvl]++
					if lvl == s.level {
						currentCount++
						s.tmpCandidates = append(s.tmpCandidates, resolveCandidate{varIdx: v})
					}
				}
			}
		}
		resolveStep++
	}

	// FIX: If 1-UIP didn't converge (currentCount > 1) after exhausting all
	// candidates, check if remaining literals are all decisions or if some are
	// propagations with buggy reasons. The decisions/propagations counts are
	// consumed ONLY by the fallback diagnostic (logUIPFallback), and the fallback
	// is empirically never entered (uipFallback==0 across the 72-suite and all
	// held-out cnfgen families; the held-out gate FAILs any family that fires it).
	// So the O(touched) counting scan is behavior-neutral waste on the dominant
	// path: gate it inside the fallback branch to eliminate the per-conflict cost.
	if currentCount > 1 {
		decisionsAtCurrentLevel := 0
		propagationsAtCurrentLevel := 0
		for _, varIdx := range s.tmpTouchedVars {
			if s.tmpLiteralInClause[varIdx] && s.assignments[varIdx].Level == int32(s.level) {
				if s.assignments[varIdx].Reason == -1 {
					decisionsAtCurrentLevel++
				} else {
					propagationsAtCurrentLevel++
				}
			}
		}
		s.uipFallbackCount++
		// 1-UIP did not converge: there is more than one literal at the current
		// decision level still in the clause after resolution. In a correct CDCL
		// this never happens (each level has exactly one decision, so resolving
		// non-decision literals always reduces to that single decision = the UIP).
		// Non-convergence therefore indicates inconsistent reason clauses (e.g. a
		// propagated literal whose reason contains an unassigned literal, or more
		// than one decision at one level), which were skipped above.
		//
		// FAIL-SAFE: We do NOT try to force a 1-UIP by dropping the remaining
		// current-level literals. The dropped literals would not be logically
		// implied unless every reason clause is consistent — but non-convergence
		// is precisely the signature of *inconsistent* reason clauses, so pruning
		// them can produce an un-entailed learned clause that later rules out a
		// satisfying assignment and turns a satisfiable formula into a false
		// UNSAT. The safe action is to leave currentCount > 1 and let the caller
		// (learnClause) skip learning entirely and backjump conservatively.
		if s.verbose {
			s.Log("c [1-UIP] FALLBACK: %d decisions + %d propagations at level %d -> NO learn\n",
				decisionsAtCurrentLevel, propagationsAtCurrentLevel, s.level)
		}

		// Diagnostic: dump residual-literal state. Build trailPos for the
		// current-level trail slice (O(current-level trail)) for O(1) position
		// lookup, then clear it after the dump.
		startIdx := s.trailHead[s.level]
		for ti := startIdx; ti < len(s.trail); ti++ {
			s.trailPos[s.trail[ti]] = ti
		}
		s.logUIPFallback(decisionsAtCurrentLevel, propagationsAtCurrentLevel, startIdx)
		for ti := startIdx; ti < len(s.trail); ti++ {
			s.trailPos[s.trail[ti]] = 0
		}
	}
	return currentCount
}
