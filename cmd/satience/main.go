package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"satience/internal/cnf"
	"satience/internal/parser"
	"satience/internal/solver"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	// P4: Reduce GC pressure by increasing GC target percentage
	debug.SetGCPercent(150)

	model := flag.Bool("model", false, "Print satisfying assignment")
	verify := flag.Bool("verify", false, "Verify model is correct (implies -model)")
	noPreprocess := flag.Bool("no-preprocess", false, "Disable adaptive preprocessing (use lightweight preprocessing only)")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0=unlimited)")
	verbose := flag.Bool("verbose", false, "Show solving statistics")
	cpuprofile := flag.String("cpuprofile", "", "Write CPU profile to file")
	memprofile := flag.String("memprofile", "", "Write heap (allocation) profile to file")
	randomSeed := flag.Uint64("seed", 0, "Random seed for deterministic solving (default=0)")
	rndInit := flag.Float64("rnd-init", 0.0, "Magnitude of random noise added to initial VSIDS activity (MiniSat-style, default=0=disabled)")
	minimizeDepth := flag.Int("minimize-depth", 0, "Max recursion depth for recursive clause minimization (default=0=unlimited, relies on DAG property for termination)")
	dbCapMult := flag.Float64("db-cap-mult", 1.0, "Scale the learned-DB deletion target (experimental: DB-size vs per-propagation cost)")
	govDB := flag.Bool("gov-db", true, "Enable Detector 5: per-instance self-correcting learned-DB reduction on binary-heavy cost-bound instances")
	parity := flag.Bool("parity", true, "XOR/parity preconditioning: detect parity families + GF(2)-derive units/binaries (add-only; default ON after distributional held-out gate; use -parity=false to disable)")
	parityMaxLen := flag.Int("parity-max-len", 6, "Parity: max support size (clause length) for a detected parity family")
	parityBudget := flag.Int("parity-budget", 2000, "Parity: hard cap on derived binary clauses appended (<=0 disables)")
	vivifyPeriod := flag.Int("vivify-period", 200, "Run clause vivification every Nth restart (default=200, 0=disabled)")
	vivifyMinConflictGap := flag.Int("vivify-min-gap", 20000, "Min conflicts between vivification rounds (default=20000, 0=restart-based only)")
	subsumptionPeriod := flag.Int("subsumption-period", 100, "Run learned-clause subsumption every Nth restart (default=100, 0=disabled)")
	randomPhaseRate := flag.Float64("random-phase-rate", 0.0, "Probability of flipping saved phase per decision (default=0.0, 0=disabled)")
	restartPhaseFlip := flag.Float64("restart-phase-flip", 0.0, "Probability of flipping each saved phase on restart (default=0.0, 0=disabled)")
	preferTrueCap := flag.Int("prefer-true-cap", 0, "Learned-clause replacement-scan prefer-true hunt budget (positions to search for a true literal before falling back to the first unassigned; 0=disabled [default: hard DEV-rail sweep showed no net win])")
	// Restart policy parameters
	restartBase := flag.Int("restart-base", 200, "Luby restart sequence base multiplier (default=200)")
	restartGlucoseRatio := flag.Float64("restart-glucose-ratio", 10.0, "Glucose restart when LBD > ratio × avg (default=10.0; classifier overrides per-instance)")
	restartGlucoseMin := flag.Int("restart-glucose-min", 10, "Min conflicts before Glucose restarts (default=10.0; classifier overrides per-instance)")
	restartPropsDecLimit := flag.Int("restart-props-dec", 100, "Tier-1 restart when props/dec exceeds this (cascade-bound, 0=disabled)")
	adaptPropDecLimit := flag.Int("restart-props-deep-limit", 20, "Tier-2 low props/dec threshold for deep-search escape")
	adaptPropDecDeepGate := flag.Int("restart-props-deep", 85, "Tier-2 deep-search escape fires when conflict level exceeds this (0=disabled)")
	adaptivePhaseFlip := flag.Float64("adaptive-phase-flip", 0.5, "Phase flip rate when props/dec is high (0=disabled)")
	// Search-governor tuning knobs (runtime self-correction parameter sweeps).
	govGrindBase := flag.Int("gov-grind-base", 4, "Governor Det1: target restartBase when cascade grind fires")
	govGrindPDec := flag.Float64("gov-grind-pdec", 120.0, "Governor Det1: props/dec threshold to qualify as cascade grind")
	govGrindConf := flag.Int("gov-grind-conf", 30000, "Governor Det1: min conflicts before Det1 may fire")
	govWanderDecC := flag.Float64("gov-wander-decconf", 40.0, "Governor Det3: min decisions/conflict to qualify as wander")
	govWanderGlue := flag.Float64("gov-wander-glue", 0.10, "Governor Det3: max glue ratio to qualify as wander")
	govWindow := flag.Int("gov-window", 20000, "Governor: window scope in conflicts per evaluation")
	govStagLBD := flag.Float64("gov-stag-lbd", 12.0, "Governor Det4: window avgLBD threshold for LBD-stagnation")
	govStagGlue := flag.Float64("gov-stag-glue", 0.05, "Governor Det4: max window glue ratio to qualify as unguided")
	govStagWin := flag.Int("gov-stag-win", 3, "Governor Det4: consecutive windows with no LBD improvement to confirm")
	govStagBase := flag.Int("gov-stag-base", 20, "Governor Det4: target restartBase when LBD-stagnation fires")
	govSpiralPDec := flag.Float64("gov-spiral-pdec", 8.0, "Governor Det6: min props/dec to flip geometric->Luby on a structured unguided deep spiral")
	// VSIDS parameters
	initialDecay := flag.Float64("initial-decay", 0.95, "VSIDS initial decay factor (default=0.95, MiniSat-equivalent)")
	maxDecay := flag.Float64("max-decay", 0.95, "VSIDS maximum decay factor (default=0.95, fixed)")
	decayRampup := flag.Int("decay-rampup", 100, "Conflicts to reach max decay (default=100, minimal since initial==max)")
	lbdScale := flag.Float64("lbd-scale", 0.0, "LBD bonus scale for VSIDS (>0=explicit, 0=adaptive: max(10, 200000/numVars), <0=disable)")
	bumpAmount := flag.Float64("bump-amount", 35.0, "Base bump amount for conflicts (default=35.0; MiniSat-aligned VSIDS: equal-bump scheme with stronger bump, DEV-rail validated with -minisat-bumps)")
	clauseInitBase := flag.Float64("clause-init-base", 10.0, "Base clause initialization weight (default=10.0)")
	clauseInitBinary := flag.Float64("clause-init-binary", 100.0, "Binary clause initialization weight (default=100.0)")
	statsInterval := flag.Int("stats", 0, "Print stats every N conflicts (0=disabled, bypasses verbose gate)")
	restartLevelCap := flag.Int("restart-level-cap", 40, "Force restart when conflict level exceeds this on long-clause instances (0=disabled)")
	levelRestartGap := flag.Int("restart-level-gap", 100, "Min conflicts between level-capped restarts")
	claDecay := flag.Float64("cla-decay", 0.99, "Clause activity decay factor for deletion ordering (default=0.99, slower than MiniSat 0.95)")
	noClaActivity := flag.Bool("no-cla-activity", false, "Disable activity-based clause deletion (use FIFO within LBD tiers)")
	lbdTier1 := flag.Int("lbd-tier1", 5, "Pass 1 deletion: delete LBD > threshold (default 5)")
	lbdTier2 := flag.Int("lbd-tier2", 2, "Pass 2 deletion: delete LBD > threshold (default 2; glue ≤ threshold never deleted)")
	dbGrowthDiv := flag.Int("db-growth-div", 50, "dynamicLimit = maxLearned + conflicts/div (default 50)")
	delTriggerRatio := flag.Float64("del-trigger-ratio", 1.5, "Trigger deletion when active > ratio × dynamicLimit (default 1.5)")
	dbShrinkThresh := flag.Int("db-shrink-thresh", 10, "Shrink maxLearned when avgLBD > threshold (default 10)")
	dbShrinkFloorMult := flag.Int("db-shrink-mult", 3, "Shrink floor = numVars × multiplier (default 3)")
	dbMaxLen := flag.Int("db-max-len", 25, "reduceDB length gate: evict clauses strictly longer than this first on binary-heavy formulas (0 = disabled)")
	occurrenceWeight := flag.Float64("occurrence-weight", 0.5, "Occurrence bonus weight for VSIDS init (default 0.5)")
	bigBfs := flag.Int("big-bfs", 16, "Per-call BIG transitive-minimization BFS node budget (default 16)")
	bigHitWindow := flag.Int64("big-hit-window", 50000, "Adaptive BIG gate: disable BIG after this many 0-hit per-conflict attempts (0=never; default 50000)")
	bigLearn := flag.Bool("big-learn", false, "Add learned binary clauses to the BIG for stronger transitive minimization (A/B; net wall-time regression, default off)")
	skipVSIDSInit := flag.Bool("skip-vsids-init", false, "Skip clause-length and occurrence VSIDS initialization (zero init, like MiniSat)")
	geometricRestarts := flag.Bool("geometric", true, "Use geometric restarts (restartBase * 1.5^idx, like MiniSat's -no-luby). Default on: distributional held-out gate PASS (all cnfgen families, no new TMO) and broad hard-cnfgen PAR2 win. Set -geometric=false to use the Glucose-adaptive + Luby fallback instead.")
	minisatBumps := flag.Bool("minisat-bumps", true, "Use MiniSat-style equal VSIDS bumps (no clause-length weighting, no minBump floor). Default on: full-48-rail DEV win with -bump-amount=35 (-6.6% PAR2, net -1 TMO). Disable with -minisat-bumps=false.")
	msAnalyze := flag.Bool("ms-analyze", false, "Force analyze_toclear bumping (bump all touched vars, like MiniSat) for A/B testing")
	lazyInit := flag.Bool("lazy-init", false, "Detect bad trajectory and inject occurrence-based VSIDS bump (reactive init for zero-init mode)")
	flag.Parse()

	// Collect explicitly-set flags so the behavioral governor knows which CLI
	// restart/Glucose values to respect instead of overriding. flag.Visit
	// only yields flags that were actually passed on the command line, not
	// flags at their default values.
	explicitFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	// -verify implies -model
	if *verify {
		*model = true
	}

	var profileFile *os.File
	if *memprofile != "" {
		runtime.MemProfileRate = 1
		defer func() {
			f, err := os.Create(*memprofile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error creating memprofile: %v\n", err)
				return
			}
			pprof.Lookup("heap").WriteTo(f, 0)
			f.Close()
		}()
	}
	if *cpuprofile != "" {
		var err error
		profileFile, err = os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating profile: %v\n", err)
			return 1
		}
		if err := pprof.StartCPUProfile(profileFile); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting CPU profile: %v\n", err)
			profileFile.Close()
			return 1
		}
		defer func() {
			pprof.StopCPUProfile()
			profileFile.Close()
		}()
	}

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <file.cnf>\n", os.Args[0])
		return 1
	}

	filename := flag.Arg(0)
	f, err := os.Open(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		return 1
	}
	defer f.Close()

	cnfFormula, err := parser.Parse(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing CNF: %v\n", err)
		return 1
	}

	s := solver.NewCDCLSolver(cnfFormula)
	s.SetVerbose(*verbose)
	if *maxIter > 0 {
		s.SetMaxIter(*maxIter)
	}
	s.SetRandomSeed(*randomSeed)
	s.SetRndInitNoise(*rndInit)

	// Configure restart policy
	s.SetRestartParameters(*restartBase, *restartGlucoseRatio, *restartGlucoseMin)
	s.SetRestartPropsDecLimit(*restartPropsDecLimit)
	s.SetAdaptPropDecDeepGate(*adaptPropDecLimit, *adaptPropDecDeepGate)
	s.SetGovernorParams(*govGrindBase, *govGrindPDec, *govGrindConf, *govWanderDecC, *govWanderGlue, *govWindow)
	s.SetGovernorDet4Params(*govStagLBD, *govStagGlue, *govStagWin, *govStagBase)
	s.SetGovernorSpiralPDec(*govSpiralPDec)
	s.SetAdaptivePhaseFlipRate(*adaptivePhaseFlip)

	// Configure VSIDS parameters
	s.SetInitialDecay(*initialDecay)
	s.SetMaxDecay(*maxDecay)
	s.SetDecayRampup(*decayRampup)
	if *lbdScale > 0 {
		s.SetLBDBonusScale(*lbdScale)
	} else if *lbdScale < 0 {
		s.SetNoLBDBonus(true)
	}
	s.SetBumpAmount(*bumpAmount)
	s.SetClauseInitWeights(*clauseInitBase, *clauseInitBinary)

	// Configure recursive clause minimization depth
	s.SetMinimizeMaxDepth(*minimizeDepth)
	s.SetDBCapFactor(*dbCapMult)
	s.SetGovernorDB(*govDB)
	s.SetParityParams(*parity, *parityMaxLen, *parityBudget)

	// Configure clause vivification
	s.SetVivifyPeriod(*vivifyPeriod)
	s.SetVivifyMinConflictGap(*vivifyMinConflictGap)
	s.SetSubsumptionPeriod(*subsumptionPeriod)
	s.SetRandomPhaseRate(*randomPhaseRate)
	s.SetRestartPhaseFlipRate(*restartPhaseFlip)
	s.SetPreferTrueCap(*preferTrueCap)
	s.SetStatsInterval(*statsInterval)
	s.SetRestartLevelCap(*restartLevelCap)
	s.SetLevelRestartGap(*levelRestartGap)
	s.SetClaDecay(*claDecay)
	if *noClaActivity {
		s.SetClaActivityEnabled(false)
	}
	s.SetExplicitFlags(explicitFlags)
	s.SetClauseDBParams(*lbdTier1, *lbdTier2, *dbGrowthDiv, *delTriggerRatio, *dbShrinkThresh, *dbShrinkFloorMult)
	s.SetDBMaxLen(*dbMaxLen)
	s.SetOccurrenceWeight(*occurrenceWeight)
	s.SetBigBfsBudget(*bigBfs)
	s.SetBigHitWindow(*bigHitWindow)
	if *bigLearn {
		s.SetBigLearn(true)
	}
	if *skipVSIDSInit {
		s.SetSkipVSIDSInit(true)
	}
	if *geometricRestarts {
		s.SetGeometricRestarts(true)
	}
	if *minisatBumps {
		s.SetMinisatBumps(true)
	}
	if *msAnalyze {
		s.SetUseBumpAnalyze(true)
	}
	if *lazyInit {
		s.SetLazyInit(true)
	}

	start := time.Now()
	var result solver.SolveResult
	if *noPreprocess {
		result = s.SolveWithoutPreprocessing()
	} else {
		result = s.SolveWithResult()
	}
	elapsed := time.Since(start)

	if *verbose {
		fmt.Printf("c Time: %.3fs\n", elapsed.Seconds())
	}

	switch result {
	case solver.SAT:
		fmt.Println("s SATISFIABLE")
		if *model {
			if err := printModel(s, cnfFormula, *verify); err != nil {
				fmt.Fprintf(os.Stderr, "c [ERROR] %v\n", err)
				return 1
			}
		}
		return 10
	case solver.UNSAT:
		fmt.Println("s UNSATISFIABLE")
		return 20
	default:
		fmt.Println("s UNKNOWN")
		return 0
	}
}

func printModel(s *solver.CDCLSolver, cnf *cnf.CNF, verify bool) error {
	assignments := s.GetAssignments()

	numVars := int(cnf.NumVars)
	if len(assignments) < numVars {
		numVars = len(assignments)
	}
	for i := 0; i < numVars; i++ {
		assign := assignments[i]
		if assign.Level >= 0 {
			val := int32(i + 1)
			if !assign.Value {
				val = -val
			}
			fmt.Printf("v %d\n", val)
		}
	}
	fmt.Println("v 0")

	if verify {
		if err := solver.VerifySolution(cnf, assignments, false); err != nil {
			return fmt.Errorf("model verification failed: %w", err)
		}
		fmt.Println("c Model verified: all clauses satisfied")
	}
	return nil
}
