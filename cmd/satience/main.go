package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"satience/internal/parser"
	"satience/internal/solver"
)

func main() {
	showModel := flag.Bool("model", false, "Show satisfying assignment if SAT")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0 = unlimited)")
	verbose := flag.Bool("verbose", false, "Show solving statistics")
	cpuprofile := flag.String("cpuprofile", "", "Write CPU profile to file")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [-model] [-max-iter N] [-verbose] <input.cnf>\n", os.Args[0])
		os.Exit(2)
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating profile file: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	f, err := os.Open(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(2)
	}
	defer f.Close()

	cnf, err := parser.Parse(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing CNF: %v\n", err)
		os.Exit(2)
	}

	s := solver.NewCDCLSolver(cnf)
	if *maxIter > 0 {
		s.SetMaxIter(*maxIter)
	}
	if *verbose {
		s.SetVerbose(true)
	}
	
	result := s.SolveWithResult()
	
	switch result {
	case solver.SAT:
		fmt.Println("SAT")
		if *showModel {
			printModel(s, cnf)
		}
		os.Exit(0)
	case solver.UNSAT:
		fmt.Println("UNSAT")
		os.Exit(1)
	case solver.UNKNOWN:
		fmt.Println("UNKNOWN")
		os.Exit(2)
	}
}

func printModel(s *solver.CDCLSolver, cnf interface{}) {
	assignments := s.GetAssignments()
	fmt.Println("Model:")
	for i, a := range assignments {
		if a.Level > 0 {
			fmt.Printf("  x%d = %v\n", i+1, a.Value)
		}
	}
}
