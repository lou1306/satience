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
// Three detectors:
//   - Detector 1 (grind / timeout-cliff): deep binary cascade with extreme
//     sustained props/dec -> reduce restartBase to ~5 so the cascade is cut
//     short and diversified frequently. Targets bb34f22f (props/dec ~160) at
//     the 30s timeout cliff; empirically raising the base made it worse.
//   - Detector 3 (wander): high decisions/conflict ratio + low glue + flat LBD
//     (SAT model-search wandering) -> enable Glucose restarts. Generalizes the
//     prior static density/ternary "wanderer" gate into a runtime detector that
//     self-heals.
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
	winDec := s.decisions - int(s.govStartDec)
	winProps := s.propagations - int(s.govStartProps)
	winMoves := int(s.numWatchMoves - s.govStartMoves)
	lbdDelta := s.totalLbdSum - s.govStartLbd
	winLbdC := s.totalLbdCount - s.govStartLbdC
	s.govStartConf = uint64(s.conflicts)
	s.govStartDec = uint64(s.decisions)
	s.govStartProps = uint64(s.propagations)
	s.govStartMoves = s.numWatchMoves
	s.govStartLbd = s.totalLbdSum
	s.govStartLbdC = s.totalLbdCount
	// Skip very short windows (search may be about to conclude anyway).
	if winConf < 2000 {
		return
	}

	propsPerDec := 0.0
	if winDec > 0 {
		propsPerDec = float64(winProps) / float64(winDec)
	}
	decPerConf := 0.0
	if winConf > 0 {
		decPerConf = float64(winDec) / float64(winConf)
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

	if s.govUnified {
		s.unifiedGovernor(winProps, winDec, winConf, winMoves, lbdDelta, winLbdC)
		return
	}
	s.detector4LbdStagnation(winLBD, glueRatio)
	s.detector1Grind(propsPerDec)
	s.detector3Wander(decPerConf, glueRatio)
	s.detector5DBCost(winMoves, winDec, propsPerDec)
}
// unifiedGovernor is the consolidated runtime controller (A/B, -gov-unified).
// Unlike the separate one-way Det1/Det3/Det4 ratchets it:
//   - runs ONE windowed loop,
//   - tracks EMA trends (clause-quality and propagation-cost) so thresholds are
//     relative to the instance's own trajectory rather than absolute constants,
//   - makes restart-base corrections bounded and REVERSIBLE (heals toward the
//     default when the pathology clears), and
//   - adds adaptive vivify cadence keyed on the measured DB-bloat signal
//     (rising propagation cost with weak glue guidance => vivify less).
//
// Deliberately conservative: each actuator moves in bounded steps with
// hysteresis and self-corrects, so a wrong call is undone rather than locked.
func (s *CDCLSolver) unifiedGovernor(winProps, winDec, winConf, winMoves int, lbdDelta uint64, winLbdC uint64) {
	if s.govVivify == 0 {
		s.govVivify = s.vivifyPeriod
	}
	if winConf < 2000 {
		return
	}
	pd := float64(winProps) / float64(max(winDec, 1))
	dcd := float64(winDec) / float64(max(winConf, 1))
	winLBD := float64(lbdDelta) / float64(max(winLbdC, 1))
	glue := 0.0
	if s.totalLbdCount > 0 {
		glue = float64(s.glueLearned) / float64(s.totalLbdCount)
	}
	mpd := float64(winMoves) / float64(max(winDec, 1))

	// Trend EMAs.
	if s.govLbdEma == 0 {
		s.govLbdEma = winLBD
	}
	s.govLbdEma = 0.9*s.govLbdEma + 0.1*winLBD
	prevMoves := s.govMovesEma
	if s.govMovesEma == 0 {
		s.govMovesEma = mpd
	}
	s.govMovesEma = 0.8*s.govMovesEma + 0.2*mpd
	movesRising := s.govMovesEma > prevMoves*1.05

	// ----- 1) Restart-depth actuator (consolidates Det1 cascade + Det4 stagnation) -----
	// Deep pathology: enormous per-decision propagation (binary cascade) OR
	// high-flat LBD stagnation with weak phase guidance and no deep-search need.
	deepPathology := pd > 120.0 ||
		(s.govLbdEma >= 12.0 && glue < 0.05 && s.polarityImbalance < 0.4 &&
			s.longClauseRatio <= 0.8 && s.structureScore >= 0.7)
	if s.conflicts >= 30000 {
		if deepPathology && s.restartBase > s.govGrindBase {
			s.restartBase = max(s.govGrindBase, s.restartBase-15)
			s.govDepthTrend = -1
			s.Log("c [gov-u] depth pathology (pd=%.0f, LBD=%.1f, glue=%.3f) -> restartBase=%d\n",
				pd, s.govLbdEma, glue, s.restartBase)
		} else if !deepPathology && s.govDepthTrend == -1 && s.restartBase < 200 {
			s.restartBase = min(200, s.restartBase+10)
			s.govDepthTrend = 0
			s.Log("c [gov-u] depth healthy -> heal restartBase=%d\n", s.restartBase)
		}
	}

	// ----- 2) Glucose wander actuator (consolidates Det3) -----
	if !s.govGlucoseOn && dcd > 40.0 && glue < 0.10 && s.emaLBD >= 20.0 {
		s.govGlucoseOn = true
		if !s.flagSet("restart-glucose-ratio") {
			s.restartGlucoseRatio = 1.5
		}
		if !s.flagSet("restart-glucose-min") {
			s.restartGlucoseMinConflicts = 100
		}
		s.Log("c [gov-u] wander (%.0f dec/conf, glue=%.3f) -> Glucose on\n", dcd, glue)
	}

	// ----- 3) DB-cost actuator (reuse the self-correcting moves/dec logic) -----
	s.detector5DBCost(winMoves, winDec, pd)

	// ----- 4) Adaptive vivify cadence -----
	// Rising propagation cost with weak glue guidance => the learned DB output
	// (incl. vivify-inflated short clauses) is the bottleneck => vivify less.
	// Healthy trend => vivify more eagerly. Bounded [50,400].
	if movesRising && glue < 0.10 && s.govVivify < 400 {
		s.govVivify += 25
		s.Log("c [gov-u] vivify: moves rising, glue=%.3f -> period %d\n", glue, s.govVivify)
	} else if !movesRising && glue >= 0.10 && s.govVivify > 50 {
		s.govVivify -= 25
		s.Log("c [gov-u] vivify: healthy -> period %d\n", s.govVivify)
	}
	if s.govVivify != s.vivifyPeriod {
		s.vivifyPeriod = s.govVivify
		s.conflictsAtLastVivify = s.conflicts
	}
}

// detector1Grind targets the deep-binary-cascade / timeout-cliff signature
// (e.g. bb34f22f: props/dec ~160+, no Glucose-guided convergence, sits at the
// 30s timeout cliff). For these, the deep cascade re-enters itself after every
// (relatively rare) restart; RAISING the Luby base makes it worse (never
// restarts), so the correction is the opposite: reduce restartBase to ~5 so the
// cascade is cut short and diversified frequently. props/dec is a live signal
// (not a static classifier), and its extreme value (>120) cleanly separates the
// cascade grinders (only bb34f22f-type instances) from normal structured/binary
// instances (de2b/8202af ~8, 30eb4ef ~46). One-way ratchet guarded by
// govGearRaised (which here means "restart base already lowered").
func (s *CDCLSolver) detector1Grind(propsPerDec float64) {
	if s.govGearRaised {
		return
	}
	// Only for stainlessly-cascade-bound instances: extremely high props/dec.
	if propsPerDec < s.govGrindPDec {
		return
	}
	// propDecGrindMinConf: the cascade must be sustained over a meaningful
	// window before we conclude the instance is cascade-bound. props/dec > 120
	// is a strong discriminator (only the bb34f22f-type grinders exceed it), so
	// we can act earlier than the full 120K patience horizon — acting late
	// would leave most of the solve grinding at the (slow) default base.
	if s.conflicts < s.govGrindConf {
		return
	}
	// Anti-thrashing: only fire once per solve (govGearRaised).
	if s.flagSet("restart-base") {
		return
	}
	s.govGearRaised = true
	// Reduce the Luby restart base so the deep cascade gets cut short
	// frequently, breaking the re-entry cycle.
	save := s.restartBase
	s.restartBase = s.govGrindBase
	s.Log("c [governor] Det1 grind: props/dec=%.0f sustained past %d conflicts -> drop restartBase %d->%d\n",
		propsPerDec, s.govGrindConf, save, s.govGrindBase)
}

// detector3Wander generalizes the prior static "wanderer" Glucose gate into a
// runtime detector. Signature: the search makes many decisions per conflict
// (decPerConf large) with low glue and flat/high LBD - i.e. it is wandering in
// decision space during SAT model search and getting no restart feedback.
// Correction: enable standard Glucose (1.5 / 100). One-way self-healing: once
// enabled it stays, and because it only ever ADDS diversification we never
// risk starving a working trajectory.
func (s *CDCLSolver) detector3Wander(decPerConf, glueRatio float64) {
	if s.govGlucoseOn {
		return
	}
	// Only fire when Glucose is currently disabled (ratio suppressed to ~100).
	if s.restartGlucoseRatio < 50.0 {
		return // already on
	}
	// Wander: many decisions per conflict and effectively no glue guidance.
	if !(decPerConf > s.govWanderDecC && glueRatio < s.govWanderGlue) {
		return
	}
	// Flat/high LBD guard: improving searches (falling LBD) shouldn't be touched.
	if emaLBD := s.emaLBD; emaLBD < 20.0 && glueRatio >= 0.02 {
		return // still guided (low LBD and some glue) - not wandering
	}
	s.govGlucoseOn = true
	if !s.flagSet("restart-glucose-ratio") {
		s.restartGlucoseRatio = 1.5
	}
	if !s.flagSet("restart-glucose-min") {
		s.restartGlucoseMinConflicts = 100
	}
	s.Log("c [governor] Det3 wander: %.0f decisions/conflict, glue=%.3f -> enable Glucose (1.5/100)\n",
		decPerConf, glueRatio)
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
//     existing adaptiveRestartGear governor already RAISES the gear for exactly
//     these. Guard: never fire when long clause dominated (longClauseRatio > 0.8).
//   - 69d72f81 (88% ternary, but PolImb 0.84) looks stagnant yet needs deep
//     search (base=200 beats base=20) because high polarity imbalance means
//     phase saving provides strong guidance. Guard: require weak phase guidance
//     (PolarityImbalance < 0.4).
//   - Already-frequent-restart instances (binary-heavy: Det1 already ran, or
//     govGearRaised) need no further lowering.
func (s *CDCLSolver) detector4LbdStagnation(winLBD, glueRatio float64) {
	if s.govStagFired {
		return
	}
	// Don't fight Det1 (grind) or the gear governor; both already lowered or
	// manage deep-search cases differently.
	if s.govGearRaised {
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

// detector5DBCost progressively reduces the learned-DB cap on binary-heavy
// formulas whose per-propagation cost (watch-moves/decision) is elevated,
// self-correcting per window: it steps dbCapFactor down while the next window's
// moves/decision keeps improving, and reverts the last step (then stops)
// whenever improvement stalls. The binaryRatio guard excludes deep-search
// non-binary instances (e.g. 30eb4ef brat=0.00) that rely on a large learned DB.
func (s *CDCLSolver) detector5DBCost(winMoves, winDec int, propsPerDec float64) {
	if !s.govDbEnabled {
		return
	}
	if s.binaryRatio <= s.govDbMinBinary {
		return
	}
	if s.conflicts < s.govDbStartConf {
		return
	}
	if winDec <= 0 {
		return
	}
	curMovesPerDec := float64(winMoves) / float64(winDec)

	if !s.govDbLaunched {
		// Precondition: per-prop cost must be elevated before we shrink anything.
		if propsPerDec < s.govDbStartPDec {
			return
		}
		s.govDbLaunched = true
		s.govDbPendingEval = true
		s.govDbFactorBase = s.dbCapFactor
		s.govDbRefMovesPerDec = curMovesPerDec
		next := s.dbCapFactor * s.govDbStepFactor
		if next < s.govDbFloor {
			next = s.govDbFloor
		}
		if next < s.dbCapFactor {
			s.dbCapFactor = next
			s.govDbSteps++
		}
		s.govDbActive = s.dbCapFactor < 1.0
		s.govDbFinalFactor = s.dbCapFactor
		s.Log("c [governor] Det5 DB-cost: launch (props/dec=%.0f, binary=%.2f) cap %.2f->%.2f\n",
			propsPerDec, s.binaryRatio, s.govDbFactorBase, s.dbCapFactor)
		return
	}

	if !s.govDbPendingEval {
		return
	}
	s.govDbPendingEval = false

	if curMovesPerDec < s.govDbRefMovesPerDec*(1.0-s.govDbMargin) {
		// Reduction helped (watch-moves per decision dropped): keep it, go deeper.
		s.govDbFactorBase = s.dbCapFactor
		s.govDbRefMovesPerDec = curMovesPerDec
		next := s.dbCapFactor * s.govDbStepFactor
		if next < s.govDbFloor {
			next = s.govDbFloor
		}
		if next < s.dbCapFactor {
			s.dbCapFactor = next
			s.govDbSteps++
			s.govDbPendingEval = true
		}
		s.Log("c [governor] Det5 DB-cost: improvement moves/dec %.0f->%.0f, cap ->%.2f\n",
			s.govDbRefMovesPerDec, curMovesPerDec, s.dbCapFactor)
	} else {
		// Stall/regress: undo the last step and stop (per-instance self-correct).
		s.dbCapFactor = s.govDbFactorBase
		s.govDbReverts++
		s.Log("c [governor] Det5 DB-cost: stall moves/dec %.0f->%.0f, revert cap ->%.2f (stop)\n",
			s.govDbRefMovesPerDec, curMovesPerDec, s.dbCapFactor)
	}
	s.govDbActive = s.dbCapFactor < 1.0
	s.govDbFinalFactor = s.dbCapFactor
}
