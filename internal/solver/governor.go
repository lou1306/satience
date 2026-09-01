// Unified search governor: runtime self-correction that compensates for
// classifier misclassification WITHOUT adding static per-instance gates.
//
// The static classifier assigns search parameters (decay, restartBase, Glucose
// on/off, preprocessing) from syntactic structure. When that prediction is
// wrong, the search exhibits a characteristic live behavioral signature. The
// governor observes those signals over rolling windows at restart boundaries
// (the cold path where vivify/subsumption already run, so the eval is free)
// and applies a corrective adjustment.
//
// Two detectors remain (Det4, Det6), both targeting the structured unguided
// deep-search spiral; Det1/Det3/Det5 were removed after a broad-rail (25
// cnfgen families, 30 draws) ablation showed they never fire / are neutral,
// while Det4 and Det6 are load-bearing (removing them turns the ordering-
// principle family from ~1s to TMO):
//   - Detector 4 (LBD-stagnation): sustained HIGH & FLAT window avgLBD with no
//     glue -> drop restartBase to cut the deep unguided spiral short.
//   - Detector 6 (geometric-spiral): under geometric restarts, flips the
//     restart MECHANISM geometric->Luby when a structured, weak-phase, no-glue,
//     high-flat-LBD, deep-cascade signature matches.
//
// All evals are gated on conflict windows (govNextConflict) so the function is a
// cheap compare+return on the hot path (the call site already sits at restart
// boundaries only).
package solver

// maybeAdaptSearch is the unified runtime self-correction entry point. Called at
// restart boundaries (cold path). Early-returns unless a full window has elap-
// sed, so the hot loop never sees per-restart overhead.
func (s *CDCLSolver) maybeAdaptSearch() {
	if s.conflicts < s.govNextConflict {
		return
	}
	s.govNextConflict = s.conflicts + s.govWindow

	// Build this window's delta of cumulative counters.
	winConf := s.conflicts - int(s.govStartConf)
	lbdDelta := s.totalLbdSum - s.govStartLbd
	winLbdC := s.totalLbdCount - s.govStartLbdC
	s.govStartConf = uint64(s.conflicts)
	s.govStartLbd = s.totalLbdSum
	s.govStartLbdC = s.totalLbdCount
	// Skip very short windows (search may be about to conclude anyway).
	if winConf < 2000 {
		return
	}

	// Live LBD quality. High avgLBD + low glue ratio = unguided search.
	glueRatio := 0.0
	if s.totalLbdCount > 0 {
		glueRatio = float64(s.glueLearned) / float64(s.totalLbdCount)
	}

	// Window-average LBD (delta over this window only) for stagnation tracking.
	winLBD := 0.0
	if winLbdC > 0 {
		winLBD = float64(lbdDelta) / float64(winLbdC)
	}

	s.detector4LbdStagnation(winLBD, glueRatio)
}

// maybeGeoSpiral is the Det6 evaluation path driven by a CONFLICT-WINDOW cadence
// rather than the restart-boundary cadence used by maybeAdaptSearch. Under
// geometric (geometricRestarts) restarts the Luby/Glucose-oriented governor is
// starved: geometric deepens by design and restarts are rare, so the
// maybeAdaptSearch gate (lubyIndex%10==0) fires too late (op_20 TMOs before
// restart #10). Here we evaluate the deep-unguided-spiral signature every
// govWindow conflicts and flip geometric->Luby when it matches. One compare
// per loop iteration (returns early until the window elapses).
func (s *CDCLSolver) maybeGeoSpiral() {
	if !s.geometricRestarts || s.geoFlipFired {
		return
	}
	if s.conflicts < s.govSpiralNext {
		return
	}
	// Evaluate on a sub-window cadence (governor window / 4) so the flip fires
	// early in the spiral, before geometric pollution accumulates. Firing at
	// ~60K conflicts (three full govWindow steps) was too late: Luby then had
	// to recover from a badly-polluted learned DB (~28s on op_20).
	s.govSpiralNext = s.conflicts + s.govWindow/4

	winConf := s.conflicts - int(s.govSpiralStartConf)
	winDec := s.decisions - int(s.govSpiralStartDec)
	winProps := s.propagations - int(s.govSpiralStartProps)
	lbdDelta := s.totalLbdSum - s.govSpiralStartLbd
	winLbdC := s.totalLbdCount - s.govSpiralStartLbdC
	s.govSpiralStartConf = uint64(s.conflicts)
	s.govSpiralStartDec = uint64(s.decisions)
	s.govSpiralStartProps = uint64(s.propagations)
	s.govSpiralStartLbd = s.totalLbdSum
	s.govSpiralStartLbdC = s.totalLbdCount
	if winConf < 2000 {
		return
	}
	propsPerDec := 0.0
	if winDec > 0 {
		propsPerDec = float64(winProps) / float64(winDec)
	}
	glueRatio := 0.0
	if s.totalLbdCount > 0 {
		glueRatio = float64(s.glueLearned) / float64(s.totalLbdCount)
	}
	winLBD := 0.0
	if winLbdC > 0 {
		winLBD = float64(lbdDelta) / float64(winLbdC)
	}
	s.detector6GeoSpiral(winLBD, glueRatio, propsPerDec)
}

// detector4LbdStagnation targets the unguided deep-search spiral: sustained
// HIGH and FLAT window-average LBD with essentially no glue. When learned
// clauses keep coming out at high LBD (spanning many levels) the LBD/Glucose
// restart is dead (EMA never exceeds avg), and the search grinds deeper and
// deeper producing ever-more high-LBD clauses with no propagation guidance —
// a deep-search → high-LBD → no-guidance cycle. Classic case: the ordering-
// principle family (op_20 is TMO at restartBase=200, 0.4s at 20).
//
// Correction: lower restartBase so restarts come much more frequently, cutting
// the deep spiral short and keeping learned clauses tight. One-way ratchet via
// govStagFired.
//
// False-positive protection is critical (behavior alone can't separate these):
//   - 274099073 (99.4% long clause, avgLBD ~16 flat, 0 glue) also looks stagnant
//     but needs DEEP search with DEFAULT decay — its long-clause structure means
//     the flat-LBD signature is genuine structure, not unguided wandering. The
//     existing restart-stack guards the deep-search cases. Guard: never fire when long clause dominated (longClauseRatio > 0.8).
//   - 69d72f81 (88% ternary, but PolImb 0.84) looks stagnant yet needs deep
//     search (base=200 beats base=20) because high polarity imbalance means
//     phase saving provides strong guidance. Guard: require weak phase guidance
//     (PolarityImbalance < 0.4).
//   - Already-frequent-restart instances (binary-heavy) need no further lowering.
func (s *CDCLSolver) detector4LbdStagnation(winLBD, glueRatio float64) {
	if s.govStagFired {
		return
	}
	// Long-clause + high-PolImb structural guards (see comment above).
	if s.longClauseRatio > 0.8 {
		return
	}
	if s.polarityImbalance >= 0.4 {
		return
	}
	// Structured-only guard. Random k-SAT / phase-transition instances
	// (structureScore < 0.7) also show sustained high-LBD + no glue, but they are
	// deliberately tuned for DEEP search (restartBase=100, Glucose) and lowering
	// the base to 20 breaks them (30eb4ef4: TMO with Det4, instant without).
	// Det4 targets the STRUCTURED unguided-spiral signature only.
	if s.structureScore < 0.7 {
		return
	}
	// Respect explicit CLI restart-base.
	if s.flagSet("restart-base") {
		return
	}

	// Record this window's avgLBD (ring buffer); compute stagnation over history.
	s.govLbdHist[s.govLbdHistIdx] = winLBD
	s.govLbdHistIdx = (s.govLbdHistIdx + 1) % len(s.govLbdHist)
	if s.govLbdHistN < len(s.govLbdHist) {
		s.govLbdHistN++
	}
	if s.govLbdHistN < s.govStagWin {
		return // not enough windows yet
	}

	// Stagnation = every recent window avgLBD is HIGH and NONE shows meaningful
	// improvement. Compute the min over the last govStagWin windows.
	need := s.govStagWin
	minLBD := 1e18
	ll := s.govLbdHistN
	if ll > len(s.govLbdHist) {
		ll = len(s.govLbdHist)
	}
	highCount := 0
	for i := 0; i < ll; i++ {
		if s.govLbdHist[i] >= s.govStagLBD {
			highCount++
		}
		if s.govLbdHist[i] < minLBD {
			minLBD = s.govLbdHist[i]
		}
	}
	if highCount < need {
		return // not consistently high LBD
	}
	if glueRatio >= s.govStagGlue {
		return // there IS glue guidance — not unguided
	}

	s.govStagFired = true
	save := s.restartBase
	s.restartBase = s.govStagBase
	s.Log("c [governor] Det4 LBD-stagnation: win LBD=%.1f (min %.1f), glue=%.3f -> drop restartBase %d->%d\n",
		winLBD, minLBD, glueRatio, save, s.govStagBase)
}

// detector6GeoSpiral flips the restart MECHANISM from geometric to Luby/Glucose
// when geometric restarts are pathological. Geometric thresholds grow as
// base·1.5^i with no way to recur to short segments, so it can never rescue a
// deep unguided spiral (the Ordering-Principle / chain family: op_20 TMO under
// geometric, 96k conflicts under Luby). Only a mechanism flip — not a base
// tweak — helps, because restart() caches geometricRestartThreshold and ignores
// subsequent restartBase changes.
//
// Signature (all required) — deliberately conservative to avoid flipping
// instances that legitimately want deep geometric search:
//   - geometric restarts active (s.geometricRestarts), not yet flipped;
//   - structured instance (structureScore >= 0.7): excludes hard random k-SAT
//     / phase-transition rails that are tuned for deep search;
//   - weak phase guidance (polarityImbalance < 0.4): strong phase saving can
//     guide deep search (69d72f81 PolImb 0.84) so we must not flip those;
//   - no glue (glueRatio < govStagGlue): there is no LBD/Glu cose guidance;
//   - sustained HIGH and FLAT window avgLBD (like Det4's stagnation): rules out
//     healthy structured instances whose LBD improves;
//   - deep cascade (props/dec >= govSpiralPDec): the spiral grinds deep.
func (s *CDCLSolver) detector6GeoSpiral(winLBD, glueRatio, propsPerDec float64) {
	if !s.geometricRestarts || s.geoFlipFired {
		return
	}
	// Structured-only gate (mirrors Det4's guard: excludes random k-SAT rails).
	if s.structureScore < 0.7 {
		return
	}
	// Weak phase guidance required — strong polarity saving guides deep search.
	if s.polarityImbalance >= 0.4 {
		return
	}
	if glueRatio >= s.govStagGlue {
		return
	}
	// Ring-buffer this window's avgLBD; require govStagWin consecutive windows.
	s.govSpiralHist[s.govSpiralIdx] = winLBD
	s.govSpiralIdx = (s.govSpiralIdx + 1) % len(s.govSpiralHist)
	if s.govSpiralN < len(s.govSpiralHist) {
		s.govSpiralN++
	}
	if s.govSpiralN < s.govStagWin {
		return
	}
	minLBD, maxLBD := 1e18, 0.0
	for i := 0; i < int(s.govSpiralN); i++ {
		if s.govSpiralHist[i] < minLBD {
			minLBD = s.govSpiralHist[i]
		}
		if s.govSpiralHist[i] > maxLBD {
			maxLBD = s.govSpiralHist[i]
		}
	}
	// Stagnation: consistently high and flat (no meaningful LBD improvement).
	if maxLBD < s.govStagLBD || maxLBD-minLBD > 6.0 {
		return
	}
	// Deep unguided cascade — distinguishes op-like spirals from shallow solves.
	if propsPerDec < s.govSpiralPDec {
		return
	}

	s.geoFlipFired = true
	s.geometricRestarts = false
	s.geometricRestartThreshold = 0
	s.Log("c [governor] Det6 geometric-spiral: structure=%.2f winLBD=%.1f (min %.1f) glue=%.3f props/dec=%.0f -> flip geo->Luby\n",
		s.structureScore, winLBD, minLBD, glueRatio, propsPerDec)
}
