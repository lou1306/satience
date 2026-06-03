package main

import (
	"flag"
	"fmt"
	"os"

	"satience/internal/cnf"
	"satience/internal/parser"
	"satience/internal/solver"
)

func main() {
	showModel := flag.Bool("model", false, "Show satisfying assignment if SAT")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0 = unlimited)")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [-model] [-max-iter N] <input.cnf>\n", os.Args[0])
		os.Exit(2)
	}

	filename := args[0]
	
	file, err := os.Open(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(2)
	}
	defer file.Close()

	cnf, err := parser.Parse(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing CNF: %v\n", err)
		os.Exit(2)
	}

	s := solver.NewCDCLSolver(cnf)
	if *maxIter > 0 {
		s.SetMaxIter(*maxIter)
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

func printModel(s *solver.CDCLSolver, cnf *cnf.CNF) {
	assignments := s.GetAssignments()
	fmt.Println("Model:")
	for i := uint32(0); i < cnf.NumVars; i++ {
		varStr := fmt.Sprintf("x%d", i+1)
		fmt.Printf("  %s = %v\n", varStr, assignments[i].Value)
	}
}
