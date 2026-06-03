package main

import (
	"fmt"
	"os"

	"satience/internal/parser"
	"satience/internal/solver"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <input.cnf>\n", os.Args[0])
		os.Exit(2)
	}

	filename := os.Args[1]
	
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

	s := solver.NewSolver(cnf)
	
	if s.Solve() {
		fmt.Println("SAT")
		os.Exit(0)
	} else {
		fmt.Println("UNSAT")
		os.Exit(1)
	}
}
