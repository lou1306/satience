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
	showModel := flag.Bool("model", false, "Show satisfying assignment if SAT (SAT Competition format)")
	maxIter := flag.Int("max-iter", 0, "Maximum iterations (0 = unlimited)")
	verbose := flag.Bool("verbose", false, "Show solving statistics (to stderr)")
	cpuprofile := flag.String("cpuprofile", "", "Write CPU profile to file")
	lrb := flag.Bool("lrb", false, "Use LRB (Learning Rate Based) heuristic instead of VSIDS")
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
	if *lrb {
		s.EnableLRB()
	}
	
	result := s.SolveWithResult()
	
	switch result {
	case solver.SAT:
		// SAT Competition 2026 format: "s SATISFIABLE"
		fmt.Println("s SATISFIABLE")
		if *showModel {
			printModelSATCompetition(s, cnf)
		}
		os.Exit(10) // SAT Competition exit code for SAT
	case solver.UNSAT:
		// SAT Competition 2026 format: "s UNSATISFIABLE"
		fmt.Println("s UNSATISFIABLE")
		os.Exit(20) // SAT Competition exit code for UNSAT
	case solver.UNKNOWN:
		// SAT Competition 2026 format: "s UNKNOWN"
		fmt.Println("s UNKNOWN")
		os.Exit(0) // SAT Competition exit code for UNKNOWN
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

// printModelSATCompetition prints satisfying assignment in SAT Competition 2026 format
// Value lines: "v <lit1> <lit2> ... 0" (max 4096 chars per line)
func printModelSATCompetition(s *solver.CDCLSolver, cnf interface{}) {
	assignments := s.GetAssignments()
	
	// Collect all assigned variables (include level 0 for preprocessing assignments)
	var literals []int
	for i, a := range assignments {
		// Include all assigned variables (Level >= 0 means assigned)
		if a.Level >= 0 {
			varId := i + 1 // Variables are 1-based in DIMACS
			if a.Value {
				literals = append(literals, varId)
			} else {
				literals = append(literals, -varId)
			}
		}
	}
	
	// Print in chunks of max 4096 characters
	const maxLineLength = 4096
	currentLine := "v"
	currentLen := 1 // Start with "v"
	
	for _, lit := range literals {
		litStr := fmt.Sprintf(" %d", lit)
		if currentLen+len(litStr) > maxLineLength {
			// Line would be too long, print it and start new one
			fmt.Println(currentLine + " 0")
			currentLine = "v" + litStr
			currentLen = 1 + len(litStr)
		} else {
			currentLine += litStr
			currentLen += len(litStr)
		}
	}
	
	// Print final line if not empty
	if currentLen > 1 {
		fmt.Println(currentLine + " 0")
	}
}
