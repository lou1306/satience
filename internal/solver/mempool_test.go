package solver

import (
	"testing"
	"satience/internal/cnf"
)

func TestLearnedClausePool(t *testing.T) {
	pool := NewLearnedClausePool(100, 6)
	
	// Test adding clauses
	lits1 := []cnf.Literal{cnf.NewLiteral(0, false), cnf.NewLiteral(1, true)}
	idx1, clause1 := pool.AddClause(lits1)
	
	if idx1 != 0 {
		t.Errorf("Expected clause index 0, got %d", idx1)
	}
	
	if len(clause1) != 2 {
		t.Errorf("Expected 2 literals, got %d", len(clause1))
	}
	
	// Test getting clause
	retrieved := pool.GetClause(0)
	if len(retrieved) != 2 {
		t.Errorf("Expected 2 literals, got %d", len(retrieved))
	}
	
	// Test deletion
	pool.DeleteClause(0)
	if !pool.IsDeleted(0) {
		t.Error("Expected clause 0 to be deleted")
	}
	
	if pool.NumActiveClauses() != 0 {
		t.Errorf("Expected 0 active clauses, got %d", pool.NumActiveClauses())
	}
}

func TestLearnedClausePoolCompaction(t *testing.T) {
	pool := NewLearnedClausePool(10, 4)
	
	// Add 5 clauses
	for i := 0; i < 5; i++ {
		lits := []cnf.Literal{cnf.NewLiteral(uint32(i), false)}
		pool.AddClause(lits)
	}
	
	// Delete 2 clauses (40% - should trigger compaction threshold)
	pool.DeleteClause(0)
	pool.DeleteClause(2)
	
	if pool.NumActiveClauses() != 3 {
		t.Errorf("Expected 3 active clauses, got %d", pool.NumActiveClauses())
	}
	
	// Compact
	pool.Compact()
	
	// Verify active clauses are still accessible
	for i := 0; i < 3; i++ {
		clause := pool.GetClause(i)
		if clause == nil {
			t.Errorf("Clause %d should exist after compaction", i)
		}
	}
	
	if pool.NumActiveClauses() != 3 {
		t.Errorf("Expected 3 active clauses after compaction, got %d", pool.NumActiveClauses())
	}
}

func TestLearnedClausePoolMemoryUsage(t *testing.T) {
	pool := NewLearnedClausePool(1000, 6)
	
	// Add 100 clauses with 6 literals each
	for i := 0; i < 100; i++ {
		lits := make([]cnf.Literal, 6)
		for j := 0; j < 6; j++ {
			lits[j] = cnf.NewLiteral(uint32(i*6+j), false)
		}
		pool.AddClause(lits)
	}
	
	memUsage := pool.MemoryUsage()
	expectedLiteralBytes := 100 * 6 * 4 // 4 bytes per literal
	expectedMetadataBytes := 100 * (8 + 8 + 1) // offsets + sizes + deleted
	
	if memUsage < expectedLiteralBytes+expectedMetadataBytes {
		t.Errorf("Memory usage %d seems too low (expected ~%d)", memUsage, expectedLiteralBytes+expectedMetadataBytes)
	}
}
