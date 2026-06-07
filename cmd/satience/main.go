package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"satience/internal/parser"
	"satience/internal/solver"
	"time"
)

func main() {
	model := flag.Bool("model", false, "Print satisfying assignment")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0=unlimited)")
	verbose := flag.Bool("verbose", false, "Show solving statistics")
	cpuprofile := flag.String("cpuprofile", "", "Write CPU profile to file")
	nopreprocess := flag.Bool("nopreprocess", false, "Disable preprocessing (for debugging)")
	flag.Parse()
	
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating profile: %v\n", err)
			os.Exit(1)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
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
	
	cnf, err := parser.Parse(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing CNF: %v\n", err)
		os.Exit(1)
	}
	
	s := solver.NewCDCLSolver(cnf)
	s.SetVerbose(*verbose)
	if *maxIter > 0 {
		s.SetMaxIter(*maxIter)
	}
	
	start := time.Now()
	result := s.SolveWithResultNoPreprocess(*nopreprocess)
	elapsed := time.Since(start)
	
	if *verbose {
		fmt.Printf("c Time: %.3fs\n", elapsed.Seconds())
	}
	
	switch result {
	case solver.SAT:
		fmt.Println("s SATISFIABLE")
		if *model {
			printModel(s)
		}
		os.Exit(10)
	case solver.UNSAT:
		fmt.Println("s UNSATISFIABLE")
		os.Exit(20)
	default:
		fmt.Println("s UNKNOWN")
		os.Exit(0)
	}
}

func printModel(s *solver.CDCLSolver) {
	assignments := s.GetAssignments()
	for i, assign := range assignments {
		if assign.Level > 0 {
			val := int32(i + 1)
			if !assign.Value {
				val = -val
			}
			fmt.Printf("v %d\n", val)
		}
	}
	fmt.Println("v 0")
}
