package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"satience/internal/parser"
	"satience/internal/solver"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: profile_solver <instance.cnf>")
		os.Exit(1)
	}
	
	filename := os.Args[1]
	
	// Create CPU profile
	f, err := os.Create("/tmp/cpu.prof")
	if err != nil {
		fmt.Printf("Error creating profile: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	
	if err := pprof.StartCPUProfile(f); err != nil {
		fmt.Printf("Error starting profile: %v\n", err)
		os.Exit(1)
	}
	defer pprof.StopCPUProfile()
	
	// Parse and solve
	file, err := os.Open(filename)
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()
	
	formula, err := parser.Parse(file)
	if err != nil {
		fmt.Printf("Error parsing: %v\n", err)
		os.Exit(1)
	}
	
	fmt.Printf("Solving: %d vars, %d clauses\n", formula.NumVars, formula.NumClauses)
	
	s := solver.NewCDCLSolver(formula)
	s.SetVerbose(true)
	
	start := time.Now()
	result := s.SolveWithResult()
	elapsed := time.Since(start)
	
	fmt.Printf("\nResult: %v (took %v)\n", result, elapsed)
	
	// Write memory profile
	memF, err := os.Create("/tmp/mem.prof")
	if err != nil {
		fmt.Printf("Error creating mem profile: %v\n", err)
		return
	}
	defer memF.Close()
	pprof.WriteHeapProfile(memF)
}
