//go:build ignore
// +build ignore

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// MiniSat output parser
type MiniSatStats struct {
	Conflicts    int
	Decisions    int
	Propagations int
	Variables    int
	Clauses      int
	CPUTime      float64
	Result       string
}

// Parse MiniSat verbose output
func parseMiniSatOutput(output string) (*MiniSatStats, error) {
	stats := &MiniSatStats{}
	scanner := bufio.NewScanner(strings.NewReader(output))

	for scanner.Scan() {
		line := scanner.Text()

		// Parse conflicts
		if strings.Contains(line, "Conflicts") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "Conflicts" && i+1 < len(parts) {
					stats.Conflicts, _ = strconv.Atoi(parts[i+1])
				}
			}
		}

		// Parse decisions
		if strings.Contains(line, "Decisions") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "Decisions" && i+1 < len(parts) {
					stats.Decisions, _ = strconv.Atoi(parts[i+1])
				}
			}
		}

		// Parse propagations
		if strings.Contains(line, "Propagations") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "Propagations" && i+1 < len(parts) {
					stats.Propagations, _ = strconv.Atoi(parts[i+1])
				}
			}
		}

		// Parse variables
		if strings.Contains(line, "Variables") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "Variables" && i+1 < len(parts) {
					stats.Variables, _ = strconv.Atoi(parts[i+1])
				}
			}
		}

		// Parse clauses
		if strings.Contains(line, "Clauses") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "Clauses" && i+1 < len(parts) {
					stats.Clauses, _ = strconv.Atoi(parts[i+1])
				}
			}
		}

		// Parse CPU time
		if strings.Contains(line, "CPU time") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "time" && i+1 < len(parts) {
					stats.CPUTime, _ = strconv.ParseFloat(strings.Trim(parts[i+1], "s"), 64)
				}
			}
		}

		// Parse result
		if strings.Contains(line, "SATISFIABLE") {
			stats.Result = "SAT"
		}
		if strings.Contains(line, "UNSATISFIABLE") {
			stats.Result = "UNSAT"
		}
	}

	return stats, nil
}

func runMiniSat(instancePath string, verbose bool) (*MiniSatStats, error) {
	args := []string{}
	if verbose {
		args = append(args, "-verb=2")
	}
	args = append(args, instancePath, "/tmp/minisat_out.txt")

	cmd := exec.Command("/home/luca/bin/minisat", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("MiniSat failed: %v\n%s", err, string(output))
	}

	// Read output file for result
	resultFile, err := os.ReadFile("/tmp/minisat_out.txt")
	if err != nil {
		return nil, err
	}

	outputStr := string(output) + "\n" + string(resultFile)
	return parseMiniSatOutput(outputStr)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run compare_with_minisat.go <instance.cnf> [verbose]")
		os.Exit(1)
	}

	instancePath := os.Args[1]
	verbose := len(os.Args) > 2 && os.Args[2] == "verbose"

	fmt.Printf("Comparing MiniSat vs Satience on: %s\n\n", instancePath)

	// Run MiniSat
	fmt.Println("Running MiniSat...")
	miniSat, err := runMiniSat(instancePath, verbose)
	if err != nil {
		fmt.Printf("MiniSat error: %v\n", err)
		return
	}

	fmt.Printf("MiniSat Result: %s\n", miniSat.Result)
	fmt.Printf("MiniSat Conflicts: %d\n", miniSat.Conflicts)
	fmt.Printf("MiniSat Decisions: %d\n", miniSat.Decisions)
	fmt.Printf("MiniSat Propagations: %d\n", miniSat.Propagations)
	fmt.Printf("MiniSat CPU Time: %.4fs\n", miniSat.CPUTime)
	if miniSat.Propagations > 0 && miniSat.CPUTime > 0 {
		fmt.Printf("MiniSat Propagations/sec: %.0f\n", float64(miniSat.Propagations)/miniSat.CPUTime)
	}
	fmt.Println()

	// Run Satience with verbose
	fmt.Println("Running Satience...")
	satienceCmd := exec.Command("./satience", "-verbose", instancePath)
	satienceOutput, err := satienceCmd.CombinedOutput()
	if err != nil && !strings.Contains(string(satienceOutput), "Exit: ") {
		fmt.Printf("Satience error: %v\n", err)
	}

	fmt.Println(string(satienceOutput))

	// Parse Satience output (simple parsing)
	outputStr := string(satienceOutput)
	if strings.Contains(outputStr, "Conflicts:") {
		lines := strings.Split(outputStr, "\n")
		for _, line := range lines {
			if strings.Contains(line, "Conflicts:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					conflicts, _ := strconv.Atoi(parts[1])
					fmt.Printf("\n=== Comparison ===\n")
					fmt.Printf("Conflicts: MiniSat=%d, Satience=%d (ratio: %.2fx)\n",
						miniSat.Conflicts, conflicts, float64(conflicts)/float64(miniSat.Conflicts))
				}
			}
			if strings.Contains(line, "Decisions:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					decisions, _ := strconv.Atoi(parts[1])
					fmt.Printf("Decisions: MiniSat=%d, Satience=%d (ratio: %.2fx)\n",
						miniSat.Decisions, decisions, float64(decisions)/float64(miniSat.Decisions))
				}
			}
		}
	}
}
