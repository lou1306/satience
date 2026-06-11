// Package solver implements memory pool for learned clauses.
package solver

import (
	"satience/internal/cnf"
)

// LearnedClausePool manages memory for learned clauses using a pool allocator.
//
// Instead of allocating each learned clause separately, we store all literals
// in a single large slice and track clause boundaries with offsets. This reduces:
//   - Allocation overhead (1 large alloc vs N small allocs)
//   - GC pressure (2 objects vs 2N objects per clause)
//   - Memory fragmentation (contiguous storage)
//
// The pool uses a simple bump allocator: new clauses are appended to the end.
// When clauses are deleted, we mark them as deleted but don't immediately reclaim.
// Periodic compaction rebuilds the pool when deletion ratio is high.
type LearnedClausePool struct {
	// literals stores all learned clause literals contiguously
	literals []cnf.Literal

	// clauseOffsets tracks the start offset of each clause in the literals slice
	// clauseOffsets[i] = start index in literals for clause i
	clauseOffsets []int

	// clauseSizes tracks the number of literals in each clause
	// clauseSizes[i] = number of literals for clause i
	clauseSizes []int

	// deleted tracks which clauses have been deleted (true = deleted)
	deleted []bool

	// numDeleted counts deleted clauses for compaction trigger
	numDeleted int

	// compactionThreshold triggers compaction when deleted/total > 1/threshold
	// e.g., threshold=4 means compact when >25% clauses are deleted
	compactionThreshold int
}

// NewLearnedClausePool creates a new learned clause pool with pre-allocated capacity.
func NewLearnedClausePool(expectedClauses int, expectedLiteralsPerClause int) *LearnedClausePool {
	return &LearnedClausePool{
		literals:            make([]cnf.Literal, 0, expectedClauses*expectedLiteralsPerClause),
		clauseOffsets:       make([]int, 0, expectedClauses),
		clauseSizes:         make([]int, 0, expectedClauses),
		deleted:             make([]bool, 0, expectedClauses),
		numDeleted:          0,
		compactionThreshold: 4, // Compact when >25% deleted
	}
}

// AddClause adds a new learned clause to the pool.
// Returns the clause index (0-based) and a slice view of the literals.
// IMPORTANT: The returned slice is only valid until the next AddClause or Compact call.
// Callers must copy the literals if they need them longer.
func (p *LearnedClausePool) AddClause(literals []cnf.Literal) (clauseIdx int, clauseLits []cnf.Literal) {
	clauseIdx = len(p.clauseOffsets)

	// Append literals to pool
	offset := len(p.literals)
	p.literals = append(p.literals, literals...)

	// Track clause boundaries
	p.clauseOffsets = append(p.clauseOffsets, offset)
	p.clauseSizes = append(p.clauseSizes, len(literals))
	p.deleted = append(p.deleted, false)

	// Return slice view of the literals
	clauseLits = p.literals[offset : offset+len(literals)]
	return clauseIdx, clauseLits
}

// GetClause returns a slice view of the literals for a clause.
// Returns nil if the clause index is invalid or the clause is deleted.
// IMPORTANT: The returned slice is only valid until the next AddClause or Compact call.
func (p *LearnedClausePool) GetClause(clauseIdx int) []cnf.Literal {
	if clauseIdx < 0 || clauseIdx >= len(p.clauseOffsets) {
		return nil
	}
	if p.deleted[clauseIdx] {
		return nil
	}

	offset := p.clauseOffsets[clauseIdx]
	size := p.clauseSizes[clauseIdx]
	return p.literals[offset : offset+size]
}

// DeleteClause marks a clause as deleted.
// The memory is not immediately reclaimed; use Compact() to reclaim.
func (p *LearnedClausePool) DeleteClause(clauseIdx int) {
	if clauseIdx < 0 || clauseIdx >= len(p.clauseOffsets) {
		return
	}
	if !p.deleted[clauseIdx] {
		p.deleted[clauseIdx] = true
		p.numDeleted++
	}
}

// IsDeleted returns true if a clause has been deleted.
func (p *LearnedClausePool) IsDeleted(clauseIdx int) bool {
	if clauseIdx < 0 || clauseIdx >= len(p.clauseOffsets) {
		return true
	}
	return p.deleted[clauseIdx]
}

// NumClauses returns the total number of clauses (including deleted).
func (p *LearnedClausePool) NumClauses() int {
	return len(p.clauseOffsets)
}

// NumActiveClauses returns the number of non-deleted clauses.
func (p *LearnedClausePool) NumActiveClauses() int {
	return len(p.clauseOffsets) - p.numDeleted
}

// ShouldCompact returns true if compaction would reclaim significant memory.
func (p *LearnedClausePool) ShouldCompact() bool {
	if len(p.clauseOffsets) == 0 {
		return false
	}
	// Compact when >25% clauses are deleted
	return p.numDeleted >= len(p.clauseOffsets)/p.compactionThreshold
}

// Compact reclaims memory from deleted clauses.
// This rebuilds the pool, keeping only active clauses.
// IMPORTANT: All clause indices remain valid, but the underlying literal storage changes.
// Callers with cached literal slices must refresh them after Compact().
func (p *LearnedClausePool) Compact() {
	if p.numDeleted == 0 {
		return
	}

	// Create new pool
	newCapacity := len(p.literals) - p.numDeleted*4 // Estimate
	if newCapacity < 0 {
		newCapacity = 0
	}
	newLiterals := make([]cnf.Literal, 0, newCapacity)
	newOffsets := make([]int, 0, len(p.clauseOffsets)-p.numDeleted)
	newSizes := make([]int, 0, len(p.clauseSizes)-p.numDeleted)
	newDeleted := make([]bool, 0, len(p.deleted)-p.numDeleted)

	for i := range p.clauseOffsets {
		if p.deleted[i] {
			continue // Skip deleted clauses
		}

		// Copy literals to new pool
		oldOffset := p.clauseOffsets[i]
		oldSize := p.clauseSizes[i]
		newOffset := len(newLiterals)

		newLiterals = append(newLiterals, p.literals[oldOffset:oldOffset+oldSize]...)

		// Track new boundaries
		newOffsets = append(newOffsets, newOffset)
		newSizes = append(newSizes, oldSize)
		newDeleted = append(newDeleted, false)
	}

	// Replace old pool
	p.literals = newLiterals
	p.clauseOffsets = newOffsets
	p.clauseSizes = newSizes
	p.deleted = newDeleted
	p.numDeleted = 0
}

// Clear removes all clauses and resets the pool.
func (p *LearnedClausePool) Clear() {
	p.literals = p.literals[:0]
	p.clauseOffsets = p.clauseOffsets[:0]
	p.clauseSizes = p.clauseSizes[:0]
	p.deleted = p.deleted[:0]
	p.numDeleted = 0
}

// MemoryUsage returns the approximate memory usage in bytes.
func (p *LearnedClausePool) MemoryUsage() int {
	// literals: 4 bytes per literal
	// offsets: 8 bytes per clause (int on 64-bit)
	// sizes: 8 bytes per clause
	// deleted: 1 byte per clause (bool)
	literalBytes := len(p.literals) * 4
	metadataBytes := len(p.clauseOffsets) * (8 + 8 + 1)
	return literalBytes + metadataBytes
}
