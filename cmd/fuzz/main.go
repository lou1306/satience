package main

import (
	"flag"
	"fmt"
	"os"

	"satience/internal/fuzzer"
)

func main() {
	seed := flag.Int64("seed", 0, "Random seed (0 = use current time)")
	verbose := flag.Bool("verbose", false, "Show detailed progress")
	numTests := flag.Int("n", 20, "Number of fuzz tests to run")
	maxVars := flag.Int("max-vars", 100, "Maximum variables for random instances")
	maxClauses := flag.Int("max-clauses", 500, "Maximum clauses for random instances")
	timeout := flag.Int("timeout", 10, "Timeout per test in seconds")
	mode := flag.String("mode", "random", "Fuzzing mode: random, structured, or pigeonhole")

	flag.Parse()

	fuzz := fuzzer.New(*seed, *verbose)

	var results []fuzzer.FuzzResult

	switch *mode {
	case "random":
		fmt.Printf("Running random fuzzing: %d tests, %d-%d vars, %d-%d clauses\n",
			*numTests, 10, *maxVars, *maxVars, *maxClauses)
		results = fuzz.RunRandomFuzz(*numTests, *maxVars, *maxClauses, *timeout)

	case "structured":
		fmt.Printf("Running structured fuzzing: %d tests\n", *numTests)
		results = fuzz.RunStructuredFuzz(*numTests, *timeout)

	case "pigeonhole":
		fmt.Printf("Running pigeonhole fuzzing: up to %d pigeons\n", *numTests)
		results = fuzz.RunPigeonholeFuzz(*numTests, *timeout)

	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s\n", *mode)
		os.Exit(1)
	}

	fuzzer.PrintSummary(results)

	if fuzzer.HasCriticalErrors(results) {
		os.Exit(1)
	}
}
