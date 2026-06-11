package solver

import (
	"fmt"
	"runtime"
	"testing"
	"satience/internal/cnf"
)

func TestMemoryPoolPerformance(t *testing.T) {
	// Create a mock CNF
	formula := &cnf.CNF{
		NumVars:    100,
		Clauses:    make([]cnf.Clause, 0),
		NumClauses: 10,
	}
	
	// Add some clauses
	for i := 0; i < 10; i++ {
		formula.Clauses = append(formula.Clauses, cnf.Clause{
			Literals: []cnf.Literal{cnf.NewLiteral(uint32(i), false)},
		})
		formula.NumClauses++
	}
	
	s := NewCDCLSolver(formula)
	
	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	
	// Simulate learning 1000 clauses
	for i := 0; i < 1000; i++ {
		lits := make([]cnf.Literal, 6)
		for j := 0; j < 6; j++ {
			lits[j] = cnf.NewLiteral(uint32(i*6+j), false)
		}
		s.learnedClausePool.AddClause(lits)
		s.learnedClauses = append(s.learnedClauses, cnf.Clause{Literals: lits, Learned: true})
	}
	
	var memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memAfter)
	
	active, literals, memKB := s.GetMemoryPoolStats()
	
	fmt.Printf("\n=== Memory Pool Performance Test ===\n")
	fmt.Printf("Active clauses: %d\n", active)
	fmt.Printf("Pool literals: %d\n", literals)
	fmt.Printf("Pool memory: %d KB\n", memKB)
	fmt.Printf("Total alloc: %d KB\n", (memAfter.TotalAlloc-memBefore.TotalAlloc)/1024)
	fmt.Printf("Objects allocated: %d\n", memAfter.Mallocs-memBefore.Mallocs)
	
	// Verify pool is being used
	if literals != active*6 {
		t.Errorf("Expected %d literals, got %d", active*6, literals)
	}
	
	// Verify memory efficiency
	expectedMemKB := (literals * 4) / 1024 // 4 bytes per literal
	if memKB < expectedMemKB {
		t.Errorf("Pool memory %d KB seems too low (expected ~%d KB)", memKB, expectedMemKB)
	}
}
