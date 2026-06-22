package solver

import (
	"runtime"
	"satience/internal/cnf"
	"testing"
	"time"
)

func TestGCImpact(t *testing.T) {
	numVars := uint32(5000)
	numClauses := 10000

	formula := &cnf.CNF{
		NumVars:    numVars,
		Clauses:    make([]cnf.Clause, 0, numClauses),
		NumClauses: 0,
	}

	for i := 0; i < numClauses; i++ {
		lits := make([]cnf.Literal, 4)
		for j := 0; j < 4; j++ {
			lits[j] = cnf.NewLiteral(uint32(i+j)%numVars, (i+j)%2 == 0)
		}
		formula.Clauses = append(formula.Clauses, cnf.Clause{Literals: lits})
		formula.NumClauses++
	}

	t.Log("=== GC Impact Analysis ===")

	// Test with GC
	s := NewCDCLSolver(formula)

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	initialAlloc := memStats.TotalAlloc
	initialGC := memStats.NumGC

	start := time.Now()
	iterations := 0
	for time.Since(start) < 5*time.Second {
		s.Solve()
		iterations++
	}
	elapsed := time.Since(start)

	runtime.ReadMemStats(&memStats)

	totalAlloc := memStats.TotalAlloc - initialAlloc
	gcCount := memStats.NumGC - initialGC
	gcTime := float64(memStats.PauseTotalNs) / 1e9

	t.Logf("Iterations: %d in %.1fs", iterations, elapsed.Seconds())
	t.Logf("Rate: %.1f solves/sec", float64(iterations)/elapsed.Seconds())
	t.Logf("Total allocations: %d MB", totalAlloc/1024/1024)
	t.Logf("GC count: %d (%.1f GCs/sec)", gcCount, float64(gcCount)/elapsed.Seconds())
	t.Logf("Total GC pause time: %.3fs (%.1f%% of runtime)",
		gcTime, gcTime/elapsed.Seconds()*100)
	if gcCount > 0 {
		t.Logf("Avg GC pause: %.1f ms", gcTime/float64(gcCount)*1000)
	}

	// Memory pool stats
	active, literals, memKB := s.GetMemoryPoolStats()
	t.Logf("\nMemory Pool Stats:")
	t.Logf("  Active learned clauses: %d", active)
	t.Logf("  Pool literals: %d", literals)
	t.Logf("  Pool memory: %d KB", memKB)
}
