package solver

import (
	"satience/internal/cnf"
)

// Restart policy, level-0 reset, backtracking, and the in-processing/vivify/
// subsumption co-scheduling at restart boundaries. Split out of solver_cdcl.go
// for structure; same CDCLSolver state, no behavior change.

// backtrack backtracks (or backjumps) to a lower decision level
// Returns false if backtracking to level 0 (UNSAT)

// Backjumping vs Chronological Backtracking:
// Traditional DPLL backtracks one level at a time (chronological).
// CDCL solvers use backjumping (non-chronological backtracking) to skip
// irrelevant decision levels.

// How Backjumping Works (standard CDCL):
// 1. After 1-UIP conflict analysis, the learned clause has exactly one literal
//    at the current decision level (the UIP - Unique Implication Point)
// 2. The backjump level is the second-highest level in the learned clause
// 3. Instead of backtracking to level-1, we jump directly to backjumpLevel
// 4. At backjumpLevel, the learned clause is unit: all non-UIP literals are
//    false at levels <= backjumpLevel. propagateAssertingLiteral() assigns the
//    UIP at backjumpLevel. The decision at backjumpLevel is NOT flipped.

// Why Backjumping is Sound:
// The learned clause explains why the conflict occurred. All literals in the
// learned clause except the UIP are already false at levels < current.
// By backjumping to the second-highest level, the learned clause becomes unit
// and propagates the UIP literal, preventing the same conflict.

// Example:
// Decisions: x=1 (level 1), y=1 (level 2), z=1 (level 3)
// Conflict at level 3
// Learned clause: (¬x ∨ ¬y ∨ ¬z) with LBD=3 (levels 1,2,3)
// Backjump level = 2 (second-highest in learned clause)
// After backjump: trail = [x=1, y=1], z is unassigned
// The learned clause is now unit: ¬z is forced at level 2

// This skips exploring the entire subtree under (x=1, y=1, z=1) at level 3,
// which would all lead to the same conflict.
func (s *CDCLSolver) backtrack() bool {
	// Check for empty learned clause (UNSAT detected during 1-UIP analysis)
	if s.emptyClauseFound {
		s.Log("c [BACKTRACK] Empty clause found - returning UNSAT\n")
		return false
	}

	if len(s.trailHead) <= 1 {
		s.Log("c [BACKTRACK] Returning false: trailHead len=%d\n", len(s.trailHead))
		return false
	}

	// Use backjump level if available, otherwise backtrack one level
	bjLevel := s.backjumpLevel
	if bjLevel < 0 {
		bjLevel = s.level - 1
	}
	// Always backtrack at least one level below the conflict level.
	if bjLevel >= s.level {
		bjLevel = s.level - 1
	}
	if bjLevel < 0 {
		s.Log("c [BACKTRACK] bjLevel=%d invalid at level %d - returning UNSAT\n", bjLevel, s.level)
		return false
	}

	// Standard CDCL backjump: unassign everything at levels > bjLevel, KEEP the
	// decision and propagations at bjLevel. The asserting literal (UIP) is then
	// propagated at bjLevel by propagateAssertingLiteral().
	//
	// The previous implementation used trailHead[bjLevel] (start of level bjLevel)
	// as the decision point, which unassigned the decision at bjLevel and then
	// re-assigned it with the FLIPPED value. That is DPLL chronological backtracking:
	// it discards all propagations at bjLevel and explores the opposite branch,
	// defeating conflict-driven learning. Standard CDCL (MiniSat, Glucose) keeps
	// level bjLevel and lets the asserting literal propagate — that is what
	// cancelUntil() already does.
	var decisionPoint int
	if bjLevel+1 < len(s.trailHead) {
		decisionPoint = s.trailHead[bjLevel+1]
	} else {
		decisionPoint = len(s.trail)
	}

	// Clear all assignments above bjLevel.
	// No preprocessing check needed — preprocessing vars are on preprocessTrail (not s.trail).
	// Only mark unitsDirty if we actually unassign a variable that is the target
	// of a learned unit clause (Reason encodes -learnedIdx-5 for a size-1 clause).
	// Reading Reason BEFORE unassignVar (which resets it to -1) is required.
	// This avoids an O(unitLearnedList) re-scan on every conflict when no learned
	// unit became unassigned (typically all units sit at root/level 0 and survive
	// a backjump unchanged).
	var unitVarUnassigned bool
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := s.trail[i]
		reason := s.assignments[varIdx].Reason
		if reason <= -5 {
			ui := int(-reason - 5)
			if ui < len(s.learnedLoc) && s.learnedLoc[ui].Size == 1 {
				unitVarUnassigned = true
			}
		}
		s.unassignVar(varIdx)
		s.vsids.onUnassign(varIdx)
	}
	if unitVarUnassigned {
		s.unitsDirty = true
	}
	s.trail = s.trail[:decisionPoint]
	s.qhead = decisionPoint
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel

	return true
}

// propagateAssertingLiteral propagates the UIP (asserting literal) from the
// most recently learned clause after a backjump. With qhead=decisionPoint,
// the learned clause's watched literals sit at trail positions < decisionPoint
// and would otherwise never be re-checked by the propagation loop.
//
// The asserting-clause invariant (established by learnClause and trusted by
// the watch system) guarantees position 0 (UIP) is unassigned and all others
// are false after backjump: the UIP is the only literal at s.level (> bjLevel),
// all others are at levels <= bjLevel and retain their conflict-time (false)
// values. So we assign literals[0] directly — O(1) instead of O(clause-size).
//
// In debug builds, verifyAssertingInvariant runs the full O(clause-size) scan
// and panics if the invariant is violated, catching any bug in learnClause
// or backtrack that would corrupt the invariant.
func (s *CDCLSolver) propagateAssertingLiteral() {
	if s.lastLearnedClauseIdx < 0 || s.lastLearnedClauseIdx >= s.learnedCapacity {
		return
	}
	learnedIdx := s.lastLearnedClauseIdx
	if s.learnedLoc[learnedIdx].Size < 2 {
		return // unit clauses are handled via unitLearnedList
	}
	literals := s.getLearnedClauseLiterals(learnedIdx)

	verifyAssertingInvariant(s, learnedIdx, literals)

	propLevel := s.level
	s.assignLiteralByClause(literals[0], propLevel, -learnedIdx-5)
}

// maybeLazyInit detects when the search has locked onto a bad trajectory and
// injects an occurrence-based VSIDS bump to escape it. This is the reactive
// counterpart to initVSIDSOccurrenceBonus: instead of always injecting an init
// prior (which creates trajectory sensitivity), it injects only when the search
// is demonstrably stuck. One-shot (li.done).
//
// Only active when skipVSIDSInit is true — when static init is already applied,
// the occurrence signal is already in VSIDS activity, and injecting more just
// amplifies the prior harmfully. Lazy init is a REPLACEMENT for static init,
// not a supplement.
//
// Detection requires ALL of:
//   - Enough data: totalLbdCount > li.minConflicts
//   - High avg LBD: totalAvgLBD > li.avgLBDThreshold (consistently bad learned clauses)
//   - Low glue rate: glueLearned/totalLbdCount < li.glueRateLimit (no tight implication chains)
//   - Flat/increasing LBD trend: emaLBD hasn't improved across the last li.trendWindow restarts
//
// The trend check distinguishes "bad trajectory" (stuck, not improving) from
// "legitimately hard instance" (slow but improving — LBD trend is downward).
func (s *CDCLSolver) maybeLazyInit() {
	li := s.lazyInit

	// Snapshot emaLBD at each restart for trend analysis.
	li.emaLBDSnapshots = append(li.emaLBDSnapshots, s.emaLBD)

	// Size guard: occurrence-based priors are most informative for small
	// instances where the search space is compact. For large instances,
	// the occurrence distribution adds noise — skip injection.
	if s.cnf.NumVars > 2000 {
		li.done = true
		return
	}

	// Need enough data for stable metrics.
	if s.totalLbdCount < uint64(li.minConflicts) {
		return
	}

	// Condition 1: high average LBD (consistently bad learned clauses).
	totalAvgLBD := float64(s.totalLbdSum) / float64(s.totalLbdCount)
	if totalAvgLBD <= li.avgLBDThreshold {
		return
	}

	// Condition 2: low glue rate (no tight implication chains found).
	glueRate := float64(s.glueLearned) / float64(s.totalLbdCount)
	if glueRate >= li.glueRateLimit {
		return
	}

	// Condition 3: flat or increasing LBD trend across recent restarts.
	// The search isn't learning — emaLBD is not going down.
	n := len(li.emaLBDSnapshots)
	if n < li.trendWindow+1 {
		return // not enough snapshots yet
	}
	oldest := li.emaLBDSnapshots[n-li.trendWindow-1]
	newest := li.emaLBDSnapshots[n-1]
	// Trend must be flat or increasing (not improving by more than 5%).
	if newest < oldest*0.95 {
		return // improving — don't interfere
	}

	// Bad trajectory detected. Inject occurrence-based VSIDS bump.
	s.Log("c [lazy-init] Bad trajectory detected: avgLBD=%.1f, glueRate=%.3f, emaLBD %.1f→%.1f (flat/increasing)\n",
		totalAvgLBD, glueRate, oldest, newest)
	s.injectOccurrenceBonus()
	li.done = true
}

func (s *CDCLSolver) restart() bool {
	s.unitsDirty = true // Restart clears all assignments; units need re-propagation
	s.Log("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)

	// Record the just-ended segment's productivity (instrumentation): conflicts
	// per 1000 decisions. A low value means the search made little progress per
	// decision (unproductive deep/branching search); high means many conflicts
	// per decision (dense cascades). Both extremes are signals the governor can
	// use. Segments with no decisions (preprocessing) yield a neutral 0.
	segDec := s.decisions - s.restartSegStartDec
	s.restartSegGlueCount = int(s.glueLearned) - s.restartSegStartGlue
	if s.verbose {
		s.Log("c [verbose] segment: %d conflicts / %d decisions, %d glue learned\n",
			s.conflicts-s.restartSegStartConf, segDec, s.restartSegGlueCount)
	}
	s.restartSegStartConf = s.conflicts
	s.restartSegStartDec = s.decisions
	s.restartSegStartGlue = int(s.glueLearned)

	// VSIDS activity is NOT reset on restart. Standard CDCL solvers (MiniSat,
	// Glucose) preserve activity across restarts — it is the solver's memory of
	// which variables matter. A full reset (0.3×) was destroying search memory
	// and causing 100× regressions on most instances. A mild reset (0.8×)
	// helped some instances but hurt others. No reset gives the best net result.

	// Use stored LBD values (calculated at learning time) instead of recalculating
	// Recalculating during restart gives wrong values since assignments change
	// Gated on verbose — the count is only consumed by the s.Log call below.
	if s.verbose {
		glueCount := 0
		for i := 0; i < len(s.learnedLoc); i++ {
			if s.learnedMetadata[i].LBD <= 2 {
				glueCount++
			}
		}
		s.Log("c [verbose] Restart: %d glue clauses (LBD≤2), %d total active\n", glueCount, s.learnedActiveCount)
	}

	// NOTE: We don't delete clauses on restart - let deleteLearnedClauses handle memory management
	// Restart is for escaping local minima, not for clause deletion
	// Deleting clauses on restart throws away potentially useful learned information

	// Clear search trail and assignments.
	// Preprocessing vars live on preprocessTrail (separate, permanent) — no preservation check needed.
	// Iterate the trail (only assigned vars) instead of scanning the full assignment array.
	for i := 0; i < len(s.trail); i++ {
		s.unassignVar(s.trail[i])
	}
	s.trail = s.trail[:0]
	s.trailHead = append(s.trailHead[:0], 0)
	s.qhead = 0
	s.level = 0
	// Restart clears all search assignments. Sunk heap entries (at -Inf) are
	// NOT restored by onUnassign (restart doesn't call it). Force a rebuild
	// to fix all entries with current scores.
	s.vsids.heapValid = false

	// Reset restart counters
	s.lubyIndex++
	if s.geometricRestarts {
		if s.geometricRestartThreshold == 0 {
			s.geometricRestartThreshold = float64(s.restartBase)
		}
		s.geometricRestartThreshold *= 1.5
	}
	s.restartCount = s.conflicts
	s.lbdSum = 0
	s.lbdCount = 0

	// emaLBD is NOT reset here. The EMA tracks the LBD trend across restart
	// boundaries; resetting it to 0 would destroy the trend memory and make
	// the Glucose criterion (emaLBD > avgLBD × ratio) unable to detect
	// cross-restart search degradation. lbdSum/lbdCount (per-restart average)
	// ARE reset — that's the intended design (compare persistent EMA against
	// the current restart's average).

	// Adaptive phase flip: when the search is unproductive (high props/dec,
	// indicating deep binary cascades), enable phase flipping to break the
	// phase-saving + VSIDS-preservation fixed point where the solver re-enters
	// the same cascade after every restart. When the search is productive
	// (low props/dec), disable flipping to preserve good phase information.
	// Hysteresis: enable at >120% of limit, disable at <80% of limit. The 40%
	// dead zone prevents small trajectory shifts near the threshold from
	// toggling the phase flip on/off, which amplifies into completely different
	// search trajectories.
	if s.adaptivePhaseFlipRate > 0 && s.decisions > 10 {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		// Phase flip tracks the ABSOLUTE props/dec magnitude (deep binary
		// cascades), independent of the restart trigger's low limit and its
		// deep-search gate. Baseline thresholds (120% / 80% of 100) preserve
		// pre-existing behavior: only genuinely cascade-bound instances (e.g.
		// bb34f22f at props/dec ~175) enable flipping.
		enableThreshold := float64(propsDecPhaseFlipBase) * 12 / 10
		disableThreshold := float64(propsDecPhaseFlipBase) * 8 / 10
		if propsPerDec > enableThreshold {
			s.restartPhaseFlipRate = s.adaptivePhaseFlipRate
		} else if propsPerDec < disableThreshold {
			s.restartPhaseFlipRate = 0
		}
	}

	// Phase randomization: flip each saved phase with probability
	// restartPhaseFlipRate. This breaks fixed points where phase saving
	// + VSIDS preservation causes the solver to re-enter the same search
	// region after every restart (e.g., binary-heavy instances where the
	// same polarity cascade repeats). Default 0 = disabled (standard
	// phase saving).
	if s.restartPhaseFlipRate > 0 {
		for i := range s.assignments {
			s.randomSeed ^= s.randomSeed << 13
			s.randomSeed ^= s.randomSeed >> 7
			s.randomSeed ^= s.randomSeed << 17
			if float64(s.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF) < s.restartPhaseFlipRate {
				s.assignments[i].SavedPhase = !s.assignments[i].SavedPhase
			}
		}
	}
	s.backjumpLevel = 0

	// CRITICAL FIX: DO NOT reset VSIDS activity on restart
	// VSIDS activity MUST persist across restarts to remember important variables
	// Standard CDCL solvers (MiniSat, Glucose) never reset VSIDS activity
	// Resetting activity causes solver to repeat same mistakes after each restart
	// This was causing conflicts at level 50+ and poor quality learned clauses

	// Clear temporary buffers after restart (assignments are cleared, levels reset)
	for i := range s.tmpLiteralInClause {
		s.tmpLiteralInClause[i] = false
		s.tmpLiteralIsNegated[i] = false
	}
	for i := range s.tmpResolved {
		s.tmpResolved[i] = false
	}
	for i := range s.tmpLevelCount {
		s.tmpLevelCount[i] = 0
	}
	for i := range s.tmpLevelSetUsed {
		s.tmpLevelSetUsed[i] = false
		s.tmpLevelCountUsed[i] = false
	}
	s.tmpLevelSet = s.tmpLevelSet[:0]
	s.tmpCandidates = s.tmpCandidates[:0]
	s.tmpResolvedVars = s.tmpResolvedVars[:0]

	// Reclaim literal storage when too many tombstones have accumulated.
	// This is the safe moment: we are at level 0 and no learned clause is in
	// use as a reason (all search implications were cleared above), so the
	// implication remapping inside compactLearnedClauses is a no-op and the
	// watch rebuild uses the non-false literal selection. Reprocess the
	// trail against the freshly rebuilt watches for safety.
	if s.compactPending {
		s.compactLearnedClauses()
		s.compactPending = false
		s.qhead = 0
	}

	// In-processing co-schedule (D): we are at level 0 with no clause in use as
	// a reason, sharing this restart's already-paid reset/watch-rebuild. Firing
	// here (rather than only forcing a level-0 cancelUntil in the loop) amortizes
	// the materialize+simplify cost over the restart's existing level-0 work. The
	// loop fallback still handles long restart-free stretches.
	if s.inprocessDue() {
		if s.runInprocess() {
			return true // UNSAT detected
		}
	}

	// Vivification cadence, decoupled from the restart index. Under the old
	// schedule (lubyIndex % vivifyPeriod == 0 AND the conflict gap), vivify was
	// keyed to a flat restart counter that grows ~logarithmically with conflicts
	// under geometric restarts (threshold = restartBase * 1.5^k with base 200),
	// so it silently almost never fired on geometric-default instances and only
	// engaged after the Det6 geometric->Luby flip. Vivify can only run at the
	// level-0 restart boundary, so a true mode-independent cadence should be
	// conflict-based: fire when vivifyMinConflictGap conflicts have elapsed since
	// the last round. vivifyPeriod>0 is retained purely as the on/off switch.
	// The default gap is deliberately high (600000) so vivify fires rarely,
	// recovering the protective (near-dormant) firing rate of the old gate. A
	// low/moderate gap re-opens a regression on high-conflict vivify-hostile
	// instances (30eb, ~483k conflicts): every firing gap that helps the
	// 274099073/de2b class also fires on it, because it has the highest conflict
	// count, so no single conflict-cadence value keeps those wins without it.
	if s.vivifyEnabled && s.vivifyPeriod > 0 &&
		(s.vivifyMinConflictGap <= 0 || s.conflicts-s.conflictsAtLastVivify >= s.vivifyMinConflictGap) {
		if s.runVivification() {
			return true // UNSAT detected
		}
	}

	// Run learned-clause subsumption (forward subsumption + strengthening)
	// after vivification, at the same level-0 safety boundary. Self-gating:
	// early-returns when no binary learned clauses exist, so effectively free
	// on random instances.
	if s.subsumptionPeriod > 0 && s.lubyIndex > 0 && s.lubyIndex%s.subsumptionPeriod == 0 &&
		(s.subsumptionMinConflictGap <= 0 || s.conflicts-s.conflictsAtLastSubsumption >= s.subsumptionMinConflictGap) {
		if s.runLearnedSubsumption() {
			return true // UNSAT detected
		}
		s.conflictsAtLastSubsumption = s.conflicts
	}

	// Runtime decay adaptation: periodic re-check with rolling windows.
	// Called at restart boundaries (cold path) — calling from the hot CDCL loop
	// caused 69d72f81 to regress 9s→TMO due to compiler generating worse loop
	// code around the non-inlinable call target, even when gated to 1/500
	// conflicts. Restart boundaries are already cold (vivify/subsumption run
	// here), so the call is free.
	//
	// Gate on lubyIndex%adaptPeriod == 0 (like vivify/subsumption) to avoid
	// calling the function at every restart — even the call overhead at every
	// restart caused 1.7s regression on 69d72f81 (8.8s→10.5s). adaptPeriod=10
	// means the function is called every 10th restart; internally it early-
	// returns if s.conflicts < s.adaptNextConflict, so most calls are a single
	// comparison + return.

	// Unified search governor: runtime self-correction compensating for
	// classifier misclassification. Same cadence as decay adaptation (every
	// adaptRestartPeriod restarts, cold path); internally window-gated on
	// govNextConflict (govWindowConfScope conflicts), so it is effectively a
	// compare+return plus a cheap window eval every 20K conflicts.
	const adaptRestartPeriod = 10
	if s.lubyIndex > 0 && s.lubyIndex%adaptRestartPeriod == 0 {
		s.maybeAdaptSearch()
	}

	// Lazy init: detect bad trajectory and inject occurrence-based VSIDS bump.
	// Only fires when skipVSIDSInit is true — checked here to avoid the
	// function call overhead when lazy init is enabled but skipVSIDSInit is not.
	if s.lazyInit != nil && !s.lazyInit.done && s.skipVSIDSInit {
		s.maybeLazyInit()
	}

	return false // No UNSAT detected
}

// runInprocess executes one in-processing round. Caller must already be at
// level 0 (either a scheduled restart — co-schedule D — or an on-demand
// cancelUntil(0) from the loop fallback). Returns true if the formula became
// UNSAT. Updates the last-round snapshot and the governor's segment counters
// (this level-0 reset wasn't a scheduled restart).
func (s *CDCLSolver) runInprocess() bool {
	s.inprocessRoundsRun++
	s.Log("c [inprocess] round %d: conflicts=%d newUnits=%d gap=%d\n",
		s.inprocessRoundsRun, s.conflicts, s.rootUnitsLearned-s.unitsAtLastInprocess, s.inprocessGapCur)
	if s.simplifyOriginalDB() {
		return true
	}
	s.conflictsAtLastInprocess = s.conflicts
	s.unitsAtLastInprocess = s.rootUnitsLearned
	s.restartSegStartConf = s.conflicts
	s.restartSegStartDec = s.decisions
	s.restartSegStartGlue = int(s.glueLearned)
	return false
}

// inprocessDue reports whether an in-processing round is due at the current
// point. Combines trigger A (enough NEW root-level units discovered since the
// last round — the causal condition: formula changes unlock reductions) with
// the adaptive conflict gap C (inprocessGapCur, driven by yield), and the
// static dense-binary exclusion. It is deliberately not a bare conflict-period
// check: firing the expensive materialize+scan+rebuild only on real reductions
// amortizes the cost.
func (s *CDCLSolver) inprocessDue() bool {
	return s.inprocessPeriod > 0 && !s.inprocessExcluded && s.inprocessGapCur > 0 &&
		s.conflicts >= s.conflictsAtLastInprocess+s.inprocessGapCur &&
		s.rootUnitsLearned-s.unitsAtLastInprocess >= s.inprocessMinUnits
}

// simplifyOriginalDB re-runs original-clause simplification (subsumption +
// bounded VE) at a level-0 restart boundary, then rebuilds the original-clause
// SoA (literal pool + locs), rebuilds watches, and re-propagates unit clauses.
// In-processing makes the (otherwise preprocessing-only) reduction available to
// instances whose formula changes as search discovers unit clauses. Returns
// true if the formula became UNSAT.
func (s *CDCLSolver) simplifyOriginalDB() bool {
	if s.cnf.NumClauses == 0 {
		return false
	}
	// Materialize the original clause DB into slice form (it is nil during
	// search; parser + pre-processing consumed it into the SoA pool/locs).
	pool := s.cnf.GetLiteralPool()
	locs := s.cnf.GetOriginalClauseLocs()
	clauses := make([]cnf.Clause, 0, len(locs))
	for i := range locs {
		off := int(locs[i].Offset)
		sz := int(locs[i].Size)
		if sz == 0 {
			continue
		}
		lits := make([]cnf.Literal, sz)
		copy(lits, pool[off:off+sz])
		clauses = append(clauses, cnf.Clause{Literals: lits})
	}
	s.cnf.Clauses = clauses
	s.cnf.NumClauses = len(clauses)

	saveBudget := s.veBudget
	if s.inprocessBudget > 0 {
		s.veBudget = s.inprocessBudget
	}
	defer func() { s.veBudget = saveBudget; s.cnf.Clauses = nil }()

	subSubsumed, subStrengthened := s.subsumptionPass()
	if s.hasEmptyClause() {
		return true
	}
	elim := 0
	if vr := s.boundedVarElimination(true); vr < 0 {
		return true
	} else if vr > 0 {
		elim = vr
	}

	// Yield-based adaptive cadence (C): adjust the conflict gap for the NEXT
	// round from this round's yield (subsumed + strengthened + eliminated,
	// i.e. actual formula reduction). A productive round tightens the cadence
	// (fire again sooner), a low-yield round backs it off toward inprocessGapMax
	// (effectively stopping) without a hard latch, so a formula that becomes
	// unit-rich again can resume. Yield reuses roundYield (subsumed+
	// strengthened+eliminated).
	roundYield := subSubsumed + subStrengthened + elim
	if s.inprocessGapCur > 0 {
		if roundYield >= s.inprocessMinYield {
			if s.inprocessGapCur/2 < s.inprocessGapMin {
				s.inprocessGapCur = s.inprocessGapMin
			} else {
				s.inprocessGapCur /= 2
			}
		} else if s.inprocessGapMin > 0 && s.inprocessGapMin != s.inprocessGapMax {
			if s.inprocessGapCur*2 > s.inprocessGapMax {
				s.inprocessGapCur = s.inprocessGapMax
			} else {
				s.inprocessGapCur *= 2
			}
		}
	}

	s.cnf.RebuildLiteralPool()
	s.originalUnitClauses = precomputeOriginalUnitClauses(s.cnf)

	// Rebuild watches (original + learned) and re-propagate level-0 units so
	// the reduced formula is fully consistent before search resumes.
	s.watchInitialized = false
	s.initWatches()
	if s.propagateOriginalUnitsAndActivateWatches() {
		return true
	}
	return false
}

func (s *CDCLSolver) cancelUntil(level int) {
	if level >= s.level {
		return
	}
	var decisionPoint int
	if level+1 < len(s.trailHead) {
		decisionPoint = s.trailHead[level+1]
	} else {
		decisionPoint = len(s.trail)
	}
	for i := decisionPoint; i < len(s.trail); i++ {
		s.unassignVar(s.trail[i])
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:level+1]
	s.level = level
	s.qhead = len(s.trail)
}

// cancelUntil backtracks to the given decision level, unassigning all variables
// above it. Used by vivification to roll back trial assignments. Unlike
// backtrack(), this does not flip decisions or perform conflict analysis — it
// is a pure state rollback.
// unassignVar clears the assignment for a single variable and updates all
// derived state (litTrue, implication, numUnassigned). Used by cancelUntil,
// backtrack, and restart to unassign variables from the trail.
func (s *CDCLSolver) unassignVar(varIdx uint32) {
	s.assignments[varIdx].Level = -1
	s.assignments[varIdx].Value = false
	s.assignments[varIdx].Reason = -1
	s.litTrue[int(varIdx)*2] = false
	s.litTrue[int(varIdx)*2+1] = false
	s.numUnassigned++
}

// shouldRestart determines if the solver should restart search

// Restart Policies in CDCL Solvers:
// Restarts are essential for modern SAT solvers. They escape unproductive search
// regions where the solver is making poor decisions or exploring fruitless branches.

// Two Restart Policies Implemented:

// 1. Glucose-Style Adaptive Restarts (PRIMARY, more aggressive):
//    - Monitor the LBD of learned clauses during search
//    - When current LBD > 1.5× average LBD, the search is unproductive
//    - Restart immediately to try different decisions
//    - This is reactive: restarts based on actual search quality

//    Why it works:
//    - High LBD means the learned clause spans many decision levels
//    - This indicates the search is "lost" - decisions don't connect well
//    - Restarting allows the solver to make different decisions
//    - The 1.5× threshold is empirically optimal (Glucose solver)

// 2. Luby Sequence (FALLBACK, conservative):
//    - Geometric sequence: 1, 1, 2, 1, 1, 2, 4, 1, 1, 2, ...
//    - Multiply by restartBase (default 100) for conflict threshold
//    - Guaranteed to restart periodically even if LBD criterion not met
//    - This is proactive: restarts based on conflict count

// Hybrid Approach (NOW WITH CONFIGURABLE PARAMETERS):
// - First restartGlucoseMinConflicts conflicts: Use Luby only
// - After restartGlucoseMinConflicts: Use Glucose criterion (configurable ratio)
// - If Glucose criterion not met: Fall back to Luby (configurable base)

// What Happens on Restart:
// 1. Clear the trail (all search assignments; preprocessing vars preserved on preprocessTrail)
// 2. Keep ALL learned clauses (deletion is handled by deleteLearnedClauses based on
//    database size, NOT by restart)
// 3. Reset lbdSum/lbdCount for fresh per-restart average. emaLBD is NOT reset —
//    it tracks the LBD trend across restart boundaries (B5 fix). The glucoseGap
//    field (default 100 conflicts) prevents double-restarts from the persistent
//    EMA carrying high pre-restart values vs low post-restart avgLBD.
// 4. Continue search with same VSIDS scores (activity preserved across restarts)
// 5. Run compaction if tombstones accumulated, vivification every Nth restart

// Why Keep Glue Clauses?
// Glue clauses (LBD ≤ 2) are the "backbone" of the search:
// - They connect few decision levels (highly general)
// - They propagate often and prune large parts of search space
// - Deleting them would cause the solver to re-explore the same conflicts
func (s *CDCLSolver) shouldRestart() bool {
	// MiniSat-style mode: pure geometric restart, no Glucose LBD criterion.
	// threshold = restartBase * 1.5^lubyIndex. Cached incrementally in
	// geometricRestartThreshold (updated in restart() alongside lubyIndex) to
	// avoid math.Pow on every conflict. With base=100: 100, 150, 225, 337, ...
	if s.geometricRestarts {
		if s.geometricRestartThreshold == 0 {
			s.geometricRestartThreshold = float64(s.restartBase)
		}
		if float64(s.conflicts-s.restartCount) >= s.geometricRestartThreshold {
			s.restartReasons[4]++ // geometric
			return true
		}
		// Still allow props/dec and level-capped early escape (below).
	} else {
		// Check Glucose-style adaptive restart first (if past min conflicts).
		// The gap gate (conflicts - restartCount >= glucoseGap) prevents double-
		// restarts: with a persistent EMA (B5 fix), the EMA carries high pre-
		// restart LBD values while avgLBD is low right after a restart (fresh
		// good clauses), so the criterion could fire immediately. The gap lets
		// avgLBD stabilize first.
		if s.conflicts >= s.restartGlucoseMinConflicts && s.lbdCount > 0 &&
			s.conflicts-s.restartCount >= s.glucoseGap {
			avgLBD := float64(s.lbdSum) / float64(s.lbdCount)

			// Glucose criterion: restart when the EMA of recent LBDs exceeds
			// ratio × overall average. Using EMA (α=0.1, half-life ~7 conflicts)
			// instead of single lastConflictLBD avoids noise from individual
			// LBD spikes triggering spurious restarts.
			if s.emaLBD > avgLBD*s.restartGlucoseRatio {
				s.restartReasons[0]++ // glucose
				s.Log("c [restart] Glucose: EMA LBD %.1f > avg %.1f × %.2f\n",
					s.emaLBD, avgLBD, s.restartGlucoseRatio)
				return true
			}
		}

		// Fall back to Luby sequence (configurable base). The raw Luby sequence
		// oscillates back to short segments (1,1,2,1,1,2,4,...) forever.
		lubyValue := luby(s.lubyIndex + 1)
		threshold := float64(lubyValue * s.restartBase)

		if float64(s.conflicts-s.restartCount) >= threshold {
			s.restartReasons[1]++ // luby
			return true
		}

	}

	// Props/dec-bounded restart: if the solver is going too deep per decision
	// (unproductive binary cascade), restart to escape the trajectory. This
	// catches the "deep search" pathology where props/dec >> 100 (e.g.,
	// binary-heavy instances where each decision cascades through hundreds of
	// binary clauses for a single conflict). The Glucose EMA criterion doesn't
	// fire here because binary cascades produce low-LBD glue clauses, making
	// the search look productive by LBD metrics when it's actually going nowhere.
	// Requires a minimum conflict gap to prevent thrashing (the solver needs
	// time to explore between restarts, and the phase flip needs time to take
	// effect). In no-init mode, fires from conflict 0 (early escape needed to
	// compensate for missing VSIDS init). In default init-on mode, gated on
	// lubyIndex >= 3 (matches B3 baseline behavior).
	if s.restartPropsDecLimit > 0 && s.decisions > 10 &&
		(s.skipVSIDSInit || s.lubyIndex >= 3) &&
		s.conflicts-s.restartCount >= s.propsDecRestartGap {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		if propsPerDec > float64(s.restartPropsDecLimit) {
			s.restartReasons[2]++ // propsdec
			s.Log("c [restart] Props/dec %.1f > %d\n", propsPerDec, s.restartPropsDecLimit)
			return true
		}
	}

	// Tier 2: deep-search escape. A low props/dec threshold gated on unrewarded
	// deep search (conflict level exceeding adaptPropDecDeepGate). This restarts
	// instances stuck in a deep cascade (high conflict level) despite only modest
	// average props/dec (e.g. 8d58ca: level up to 4400), while leaving shallow
	// productive cascades (30eb4e2 level<=28) and high-props/dec cascade-bound
	// instances (caught by Tier 1) untouched.
	if s.adaptPropDecDeepGate > 0 &&
		s.decisions > 10 && (s.skipVSIDSInit || s.lubyIndex >= 3) &&
		s.conflicts-s.restartCount >= s.propsDecRestartGap {
		propsPerDec := float64(s.propagations) / float64(s.decisions)
		if propsPerDec > float64(s.adaptPropDecLimit) &&
			s.lastConflictLevel > s.adaptPropDecDeepGate {
			s.restartReasons[2]++ // propsdec
			s.Log("c [restart] Props/dec %.1f > %d (deep-search, level %d > gate %d)\n", propsPerDec, s.adaptPropDecLimit, s.lastConflictLevel, s.adaptPropDecDeepGate)
			return true
		}
	}

	// Level-capped restart: on long-clause instances, force a restart when the
	// conflict level is excessively deep. Long-clause instances produce high-LBD
	// clauses consistently, so the Glucose EMA criterion (emaLBD > avg*ratio)
	// never fires (EMA ≈ avg). Deep search produces high-LBD clauses with no
	// propagation guidance, creating a deep-search → high-LBD → no-guidance →
	// deep-search cycle. This breaks the cycle by restarting when level > cap.
	// Gated on LongClauseRatio > 0.8 (binary-heavy instances have the props/dec
	// restart instead). Anti-thrashing: decisions > 10, min-conflict gap.
	// In no-init mode, fires from conflict 0. In default init-on mode, gated
	// on lubyIndex >= 3 (matches B3 baseline behavior).
	if s.restartLevelCap > 0 && s.longClauseRatio > 0.8 &&
		(s.skipVSIDSInit || s.lubyIndex >= 3) &&
		s.decisions > 10 &&
		s.conflicts-s.restartCount >= s.levelRestartGap &&
		s.lastConflictLevel > s.restartLevelCap {
		s.restartReasons[3]++ // levelcap
		s.Log("c [restart] Level-capped: conflict level %d > %d (long-clause instance)\n",
			s.lastConflictLevel, s.restartLevelCap)
		return true
	}

	return false
}
