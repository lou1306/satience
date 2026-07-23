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
	f, _ := os.Open(os.Args[1])
	defer f.Close()
	cnf, _ := parser.Parse(f)
	s := solver.NewCDCLSolver(cnf)
	s.SetRestartParameters(100, 1.5, 10)

	cpuFile, _ := os.Create("/tmp/cpu.prof")
	pprof.StartCPUProfile(cpuFile)
	defer pprof.StopCPUProfile()

	start := time.Now()
	result := s.SolveWithResult()
	elapsed := time.Since(start)
	fmt.Printf("Result: %v, Time: %.3fs\n", result, elapsed.Seconds())
}
