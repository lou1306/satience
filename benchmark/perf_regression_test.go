// Performance regression tests for satience SAT solver
// 
// These tests track solver performance on standard benchmark instances
// to detect performance regressions during development.
//
// Usage: go test -run=Perf ./benchmark
//
// Test instances are selected from different families to cover diverse problem types:
// - Tseitin grid: Structured, propagation-heavy
// - XOR/Equality: Algebraic structures
// - Arg chain: Chain-like dependencies
// - Random k-3: Random structured instances
// - PHP: Pigeonhole principle (theoretically hard for CDCL)
//
// Performance metrics tracked:
// - Conflicts: Number of conflicts encountered
// - Decisions: Number of decision steps
// - Time: Wall-clock solving time (when available)
//
// Regression detection:
// Tests fail if performance degrades by more than 20% compared to baseline.
// Baseline values should be updated when known improvements are made.

package benchmark

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// perfTestInstance defines a benchmark instance with expected performance bounds
type perfTestInstance struct {
	name           string  // Instance filename
	expectedSAT    bool    // Expected result (true=SAT, false=UNSAT)
	maxConflicts   int     // Maximum acceptable conflicts (regression threshold)
	maxDecisions   int     // Maximum acceptable decisions
	maxTimeSeconds float64 // Maximum acceptable solving time
}

// Baseline performance values (to be tuned with actual measurements)
// These represent the current best-known performance
var baselineInstances = []perfTestInstance{
	// Tseitin grid instances (structured, propagation-heavy)
	{
		name:           "tseitin_grid_3x3_sat.cnf",
		expectedSAT:    true,
		maxConflicts:   50,
		maxDecisions:   100,
		maxTimeSeconds: 1.0,
	},
	{
		name:           "tseitin_grid_4x4_unsat.cnf",
		expectedSAT:    false,
		maxConflicts:   100,
		maxDecisions:   200,
		maxTimeSeconds: 2.0,
	},
	
	// XOR/Equality instances (algebraic structures)
	{
		name:           "algebra_xor_20_sat.cnf",
		expectedSAT:    true,
		maxConflicts:   500,
		maxDecisions:   1000,
		maxTimeSeconds: 5.0,
	},
	
	// Chain instances (dependency chains)
	{
		name:           "arg_chain_50_sat.cnf",
		expectedSAT:    true,
		maxConflicts:   200,
		maxDecisions:   300,
		maxTimeSeconds: 2.0,
	},
	
	// Random structured instances
	{
		name:           "random_k3_50_sat.cnf",
		expectedSAT:    true,
		maxConflicts:   1000,
		maxDecisions:   2000,
		maxTimeSeconds: 10.0,
	},
}

// TestPerformanceRegression runs performance regression tests on standard instances
func TestPerformanceRegression(t *testing.T) {
	benchmarkDir := "./gbd_instances"
	
	// Check if benchmark directory exists
	if _, err := os.Stat(benchmarkDir); os.IsNotExist(err) {
		t.Skipf("Benchmark directory %s not found - skipping performance regression tests", benchmarkDir)
	}
	
	for _, instance := range baselineInstances {
		t.Run(instance.name, func(t *testing.T) {
			instancePath := filepath.Join(benchmarkDir, instance.name)
			
			// Check if instance file exists
			if _, err := os.Stat(instancePath); os.IsNotExist(err) {
				t.Skipf("Instance %s not found - skipping", instance.name)
			}
			
			// Run solver and measure performance
			startTime := time.Now()
			
			result, stats, err := runSolver(instancePath)
			if err != nil {
				t.Fatalf("Solver failed on %s: %v", instance.name, err)
			}
			
			elapsed := time.Since(startTime).Seconds()
			
			// Verify correctness
			if result != instance.expectedSAT {
				t.Errorf("Wrong result for %s: expected %v, got %v", 
					instance.name, instance.expectedSAT, result)
			}
			
			// Check performance bounds
			if stats.Conflicts > instance.maxConflicts {
				t.Errorf("Performance regression in %s: conflicts=%d (max=%d, +%.1f%%)",
					instance.name, stats.Conflicts, instance.maxConflicts,
					100.0*float64(stats.Conflicts-instance.maxConflicts)/float64(instance.maxConflicts))
			}
			
			if stats.Decisions > instance.maxDecisions {
				t.Errorf("Performance regression in %s: decisions=%d (max=%d, +%.1f%%)",
					instance.name, stats.Decisions, instance.maxDecisions,
					100.0*float64(stats.Decisions-instance.maxDecisions)/float64(instance.maxDecisions))
			}
			
			if elapsed > instance.maxTimeSeconds {
				t.Errorf("Performance regression in %s: time=%.2fs (max=%.2fs, +%.1f%%)",
					instance.name, elapsed, instance.maxTimeSeconds,
					100.0*(elapsed-instance.maxTimeSeconds)/instance.maxTimeSeconds)
			}
			
			// Print performance metrics
			t.Logf("✓ %s: conflicts=%d, decisions=%d, time=%.3fs",
				instance.name, stats.Conflicts, stats.Decisions, elapsed)
		})
	}
}

// runSolver runs the solver on an instance and returns result and statistics
// This is a placeholder - should be replaced with actual solver invocation
func runSolver(instancePath string) (bool, SolverStats, error) {
	// TODO: Implement actual solver invocation
	// For now, return placeholder values
	return true, SolverStats{}, nil
}

// SolverStats holds performance statistics from solver run
type SolverStats struct {
	Conflicts  int
	Decisions  int
	Iterations int
	Learned    int
	TimeSec    float64
}
