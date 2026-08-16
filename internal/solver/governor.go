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
	s.govStartConf = uint64(s.conflicts)
	s.govStartDec = uint64(s.decisions)
	s.govStartProps = uint64(s.propagations)
	s.govStartGlue = s.glueLearned
	s.govStartLbd = s.totalLbdSum
	s.govWindows++
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

	s.detector1Grind(propsPerDec)
	s.detector3Wander(decPerConf, glueRatio)
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
