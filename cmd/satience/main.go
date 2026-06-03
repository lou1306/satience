package main

import (
	"flag"
	"fmt"
	"os"

	"satience/internal/parser"
	"satience/internal/solver"
)

func main() {
	showModel := flag.Bool("model", false, "Show satisfying assignment if SAT")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [-model] <input.cnf>\n", os.Args[0])
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
	
	if s.Solve() {
		fmt.Println("SAT")
		if *showModel {
			printModel(s)
		}
		os.Exit(0)
	} else {
		fmt.Println("UNSAT")
		os.Exit(1)
	}
}

func printModel(s *solver.CDCLSolver) {
	model := s.GetModel()
	if model == nil {
		return
	}
	
	fmt.Println("Model:")
	for i, val := range model {
		varStr := fmt.Sprintf("x%d", i+1)
		if val {
			fmt.Printf("  %s = true\n", varStr)
		} else {
			fmt.Printf("  %s = false\n", varStr)
		}
	}
}
