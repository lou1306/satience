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
	useLRB := flag.Bool("lrb", false, "Use LRB (Learning Rate Based) heuristic instead of VSIDS")
	useCHB := flag.Bool("chb", false, "Use CHB (Conflict History Based) heuristic instead of VSIDS")
	randomRate := flag.Float64("random-rate", 0.0, "Probability of random decision (0.0-1.0, default=0.0)")
	randomSeed := flag.Uint64("seed", 0, "Random seed for deterministic solving (default=0)")
	minimizeDepth := flag.Int("minimize-depth", 100, "Max recursion depth for recursive clause minimization (default=100, 0=disabled)")
	vivifyPeriod := flag.Int("vivify-period", 50, "Run clause vivification every Nth restart (default=50, 0=disabled)")
	// Restart policy parameters
	restartBase := flag.Int("restart-base", 100, "Luby restart sequence base multiplier (default=100)")
	restartGlucoseRatio := flag.Float64("restart-glucose-ratio", 1.5, "Glucose restart when LBD > ratio × avg (default=1.5 for PHP)")
	restartGlucoseMin := flag.Int("restart-glucose-min", 10, "Min conflicts before Glucose restarts (default=10 for PHP)")
	restartKeepGlue := flag.Int("restart-keep-glue", 3, "Keep clauses with LBD ≤ this during restart (default=3)")
	// Clause deletion parameters
	clauseDelLBD := flag.Float64("clause-del-lbd", 200.0, "LBD score weight for clause deletion (default=200.0)")
	clauseDelAge := flag.Float64("clause-del-age", 5.0, "Age score weight for clause deletion (default=5.0)")
	clauseDelSize := flag.Float64("clause-del-size", 10.0, "Size score weight for clause deletion (default=10.0)")
	clauseDelActivity := flag.Float64("clause-del-activity", 100.0, "Activity protection weight (default=100.0)")
	clauseDelKeepRatio := flag.Float64("clause-del-keep-ratio", 0.5, "Ratio of clauses to keep during deletion (default=0.5)")
	// VSIDS parameters
	decayInterval := flag.Int("decay-interval", 10, "VSIDS decay interval - conflicts between activity decays (default=10)")
	initialDecay := flag.Float64("initial-decay", 0.90, "VSIDS initial decay factor (default=0.90)")
	maxDecay := flag.Float64("max-decay", 0.999, "VSIDS maximum decay factor (default=0.999)")
	decayRampup := flag.Int("decay-rampup", 5000, "Conflicts to reach max decay (default=5000)")
	lbdScale := flag.Float64("lbd-scale", 2000.0, "LBD bonus scale for VSIDS (default=2000.0)")
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
	if *useLRB {
		s.EnableLRB()
	}
	if *useCHB {
		s.EnableCHB()
	}
	if *randomRate > 0.0 {
		s.SetRandomDecisionRate(*randomRate)
	}
	s.SetRandomSeed(*randomSeed)

	// Configure restart policy
	s.SetRestartParameters(*restartBase, *restartGlucoseRatio, *restartGlucoseMin, *restartKeepGlue)

	// Configure clause deletion policy
	s.SetClauseDeletionParameters(*clauseDelLBD, *clauseDelAge, *clauseDelSize, *clauseDelActivity, *clauseDelKeepRatio)

	// Configure VSIDS parameters
	s.SetDecayInterval(*decayInterval)
	s.SetInitialDecay(*initialDecay)
	s.SetMaxDecay(*maxDecay)
	s.SetDecayRampup(*decayRampup)
	s.SetLBDBonusScale(*lbdScale)
	s.SetBumpAmount(*bumpAmount)
	s.SetClauseInitWeights(*clauseInitBase, *clauseInitBinary)

	// Configure recursive clause minimization depth
	s.SetMinimizeMaxDepth(*minimizeDepth)

	// Configure clause vivification
	s.SetVivifyPeriod(*vivifyPeriod)

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
