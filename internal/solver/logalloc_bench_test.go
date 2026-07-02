package solver

import (
	"os"
	"testing"

	"satience/internal/parser"
)

// BenchmarkRealInstanceLogAlloc measures allocations solving a real GBD instance
// with verbose logging OFF (the default/production path). Used to quantify the
// allocation cost of unguarded s.Log calls in the per-conflict hot path of
// propagateWatched.
func BenchmarkRealInstanceLogAlloc(b *testing.B) {
	f, err := os.Open("../../benchmark/gbd_instances/0f4576a6e7399336e11f0828d32263dd.cnf")
	if err != nil {
		b.Skipf("instance not available: %v", err)
	}
	defer f.Close()

	formula, err := parser.Parse(f)
	if err != nil {
		b.Fatalf("parse: %v", err)
	}

	// Ensure logging is OFF (production default)
	InitLogger(false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := NewCDCLSolver(formula)
		s.SetVerbose(false)
		s.SetMaxIter(3000) // bound to ~300 conflicts/24ms per solve
		s.SolveWithResult()
	}
}
