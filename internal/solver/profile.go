package solver

// Profile bundles the search parameters that couple to the branching heuristic.
// Different branching heuristics (VSIDS vs LRB/CHB) interact differently with
// restart cadence, glucose gating, inprocessing cadence, and the branch
// heuristic's own scalars, so each heuristic gets its own defaults while the
// many structure-only parameters stay shared.
//
// Resolution model (see cmd/satience/main.go): the CLI flag for each coupled
// field is DEFINED with this profile's value as its default. Go flag semantics
// then give us "implicit profile + explicit override" for free: `-branch=lrb`
// alone loads the LRB defaults, while an explicitly-passed flag overrides just
// that one field regardless of profile. The vsids profile is the frozen,
// landable baseline and is never re-tuned.
type Profile struct {
	RestartBase         int
	RestartGlucoseRatio float64
	RestartGlucoseMin   int
	Geometric           bool
	InprocessPeriod     int
	InprocessGapMin     int
	InprocessGapMax     int
	LrbEwmaAlpha        float64
	LrbMergePeriod      uint64
	ChbDecayBase        float64
	ChbSweepPeriod      uint64
}

// ProfileFor returns the frozen parameter profile for the given branch mode.
// Currently the LRB/CHB profiles mirror VSIDS (a controlled starting point);
// after DEV-rail tuning, the winning scalar values are baked in here so that
// `-branch=lrb` alone reproduces them without needing CLI flags.
func ProfileFor(mode int) Profile {
	vsids := Profile{
		RestartBase:         200,
		RestartGlucoseRatio: 10.0,
		RestartGlucoseMin:   10,
		Geometric:           true,
		InprocessPeriod:     0,
		InprocessGapMin:     5000,
		InprocessGapMax:     200000,
		LrbEwmaAlpha:        0.9,
		LrbMergePeriod:      8192,
		ChbDecayBase:        0.9,
		ChbSweepPeriod:      1024,
	}
	switch mode {
	case branchCHB, branchLRB:
		// Mirrors vsids for now; tuned after DEV-rail measurement.
		return vsids
	default:
		return vsids
	}
}
