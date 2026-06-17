package main

import (
	"flag"
	"fmt"
	"os"
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
	model := flag.Bool("model", false, "Print satisfying assignment")
	verify := flag.Bool("verify", false, "Verify model is correct (implies -model)")
	dpll := flag.Bool("dpll", false, "Use plain DPLL algorithm (no clause learning)")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0=unlimited)")
	verbose := flag.Bool("verbose", false, "Show solving statistics")
	cpuprofile := flag.String("cpuprofile", "", "Write CPU profile to file")
	useLRB := flag.Bool("lrb", false, "Use LRB (Learning Rate Based) heuristic instead of VSIDS")
	useCHB := flag.Bool("chb", false, "Use CHB (Conflict History Based) heuristic instead of VSIDS")
	preprocess := flag.Bool("preprocess", false, "Enable aggressive preprocessing (unit prop, pure lit, subsumption, equivalence)")
	randomRate := flag.Float64("random-rate", 0.0, "Probability of random decision (0.0-1.0, default=0.0)")
	randomSeed := flag.Uint64("seed", 0, "Random seed for deterministic solving (default=0)")
	minimize := flag.String("minimize", "selective", "Clause minimization: aggressive (all), selective (size≤15,LBD≤5, default), none")
	// Restart policy parameters
	restartBase := flag.Int("restart-base", 10, "Luby restart sequence base multiplier (default=10)")
	restartGlucoseRatio := flag.Float64("restart-glucose-ratio", 1.2, "Glucose restart when LBD > ratio × avg (default=1.2)")
	restartGlucoseMin := flag.Int("restart-glucose-min", 25, "Min conflicts before Glucose restarts (default=25)")
	restartKeepGlue := flag.Int("restart-keep-glue", 3, "Keep clauses with LBD ≤ this during restart (default=3)")
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
		pprof.StartCPUProfile(profileFile)
	}

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <file.cnf>\n", os.Args[0])
		os.Exit(1)
	}

	filename := flag.Arg(0)
	f, err := os.Open(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	cnfFormula, err := parser.Parse(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing CNF: %v\n", err)
		os.Exit(1)
	}

	s := solver.NewCDCLSolver(cnfFormula)
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

	// Configure clause minimization
	switch *minimize {
	case "aggressive":
		// Default: minimize all clauses (thresholds set high in solver_cdcl.go)
	case "selective":
		// Old behavior: minimize only small/low-LBD clauses
		s.SetMinimizationThresholds(15, 5, 10)
	case "none":
		// Disable minimization
		s.SetMinimizationThresholds(0, 0, 0)
	default:
		fmt.Fprintf(os.Stderr, "Invalid minimize option: %s (use aggressive, selective, or none)\n", *minimize)
		os.Exit(1)
	}

	start := time.Now()
	var result solver.SolveResult
	if *preprocess {
		result = s.SolveWithPreprocessing()
	} else if *dpll {
		result = s.SolveDPLL()
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
			printModel(s, cnfFormula, *verify)
		}
		if *cpuprofile != "" {
			pprof.StopCPUProfile()
			profileFile.Close()
		}
		return 10
	case solver.UNSAT:
		fmt.Println("s UNSATISFIABLE")
		if *cpuprofile != "" {
			pprof.StopCPUProfile()
			profileFile.Close()
		}
		return 20
	default:
		fmt.Println("s UNKNOWN")
		if *cpuprofile != "" {
			pprof.StopCPUProfile()
			profileFile.Close()
		}
		return 0
	}
}

func printModel(s *solver.CDCLSolver, cnf *cnf.CNF, verify bool) {
	assignments := s.GetAssignments()

	// Print model - only output variables up to cnf.NumVars
	numVars := int(cnf.NumVars)
	if len(assignments) < numVars {
		numVars = len(assignments)
	}
	for i := 0; i < numVars; i++ {
		assign := assignments[i]
		if assign.Level > 0 {
			val := int32(i + 1)
			if !assign.Value {
				val = -val
			}
			fmt.Printf("v %d\n", val)
		}
	}
	fmt.Println("v 0")

	// Verify model if requested
	if verify {
		if err := solver.VerifySolution(cnf, assignments, false); err != nil {
			fmt.Fprintf(os.Stderr, "c [ERROR] Model verification failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("c Model verified: all clauses satisfied")
	}
}
