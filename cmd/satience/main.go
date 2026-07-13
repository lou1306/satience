package main

import (
	"flag"
	"fmt"
	"os"
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
	dpll := flag.Bool("dpll", false, "Use plain DPLL algorithm (no clause learning)")
	noPreprocess := flag.Bool("no-preprocess", false, "Disable adaptive preprocessing (use lightweight preprocessing only)")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0=unlimited)")
	verbose := flag.Bool("verbose", false, "Show solving statistics")
	cpuprofile := flag.String("cpuprofile", "", "Write CPU profile to file")
	useCHB := flag.Bool("chb", false, "Use CHB (Conflict History Based) heuristic instead of VSIDS")
	randomSeed := flag.Uint64("seed", 0, "Random seed for deterministic solving (default=0)")
	minimizeDepth := flag.Int("minimize-depth", 0, "Max recursion depth for recursive clause minimization (default=0=unlimited, relies on DAG property for termination)")
	vivifyPeriod := flag.Int("vivify-period", 50, "Run clause vivification every Nth restart (default=50, 0=disabled)")
	vivifyMinConflictGap := flag.Int("vivify-min-gap", 20000, "Min conflicts between vivification rounds (default=20000, 0=restart-based only)")
	randomPhaseRate := flag.Float64("random-phase-rate", 0.0, "Probability of flipping saved phase per decision (default=0.0, 0=disabled)")
	restartPhaseFlip := flag.Float64("restart-phase-flip", 0.0, "Probability of flipping each saved phase on restart (default=0.0, 0=disabled)")
	// Restart policy parameters
	restartBase := flag.Int("restart-base", 100, "Luby restart sequence base multiplier (default=100)")
	restartGlucoseRatio := flag.Float64("restart-glucose-ratio", 1.5, "Glucose restart when LBD > ratio × avg (default=1.5 for PHP)")
	restartGlucoseMin := flag.Int("restart-glucose-min", 10, "Min conflicts before Glucose restarts (default=10 for PHP)")
	restartPropsDecLimit := flag.Int("restart-props-dec", 100, "Restart when props/dec exceeds this (deep search escape, 0=disabled)")
	adaptivePhaseFlip := flag.Float64("adaptive-phase-flip", 0.1, "Phase flip rate when props/dec is high (0=disabled)")
	// VSIDS parameters
	decayInterval := flag.Int("decay-interval", 1, "VSIDS decay interval - conflicts between activity decays (default=1, O(1) decay)")
	initialDecay := flag.Float64("initial-decay", 0.9792, "VSIDS initial decay factor (default=0.9792)")
	maxDecay := flag.Float64("max-decay", 0.9998, "VSIDS maximum decay factor (default=0.9998)")
	decayRampup := flag.Int("decay-rampup", 25000, "Conflicts to reach max decay (default=25000)")
	lbdScale := flag.Float64("lbd-scale", 0.0, "LBD bonus scale for VSIDS (default=0=adaptive: max(10, 200000/numVars))")
	bumpAmount := flag.Float64("bump-amount", 25.0, "Base bump amount for conflicts (default=25.0)")
	clauseInitBase := flag.Float64("clause-init-base", 10.0, "Base clause initialization weight (default=10.0)")
	clauseInitBinary := flag.Float64("clause-init-binary", 100.0, "Binary clause initialization weight (default=100.0)")
	flag.Parse()

	// -verify implies -model
	if *verify {
		*model = true
	}

	var profileFile *os.File
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
	solver.InitLogger(*verbose)
	s.SetVerbose(*verbose)
	if *maxIter > 0 {
		s.SetMaxIter(*maxIter)
	}
	if *useCHB {
		s.EnableCHB()
	}
	s.SetRandomSeed(*randomSeed)

	// Configure restart policy
	s.SetRestartParameters(*restartBase, *restartGlucoseRatio, *restartGlucoseMin)
	s.SetRestartPropsDecLimit(*restartPropsDecLimit)
	s.SetAdaptivePhaseFlipRate(*adaptivePhaseFlip)

	// Configure VSIDS parameters
	s.SetDecayInterval(*decayInterval)
	s.SetInitialDecay(*initialDecay)
	s.SetMaxDecay(*maxDecay)
	s.SetDecayRampup(*decayRampup)
	if *lbdScale > 0 {
		s.SetLBDBonusScale(*lbdScale)
	}
	s.SetBumpAmount(*bumpAmount)
	s.SetClauseInitWeights(*clauseInitBase, *clauseInitBinary)

	// Configure recursive clause minimization depth
	s.SetMinimizeMaxDepth(*minimizeDepth)

	// Configure clause vivification
	s.SetVivifyPeriod(*vivifyPeriod)
	s.SetVivifyMinConflictGap(*vivifyMinConflictGap)
	s.SetRandomPhaseRate(*randomPhaseRate)
	s.SetRestartPhaseFlipRate(*restartPhaseFlip)

	start := time.Now()
	var result solver.SolveResult
	if *dpll {
		result = s.SolveDPLL()
	} else if *noPreprocess {
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
