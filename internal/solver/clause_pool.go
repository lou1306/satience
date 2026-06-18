package solver

import (
	"satience/internal/cnf"
)

// LearnedClausePool manages learned clause storage with contiguous literal memory pool
// 
// Memory Layout:
// - All learned clause literals stored in contiguous literals[] slice
// - Each clause has offset/size into literals[]
// - Parallel metadata arrays (LBD, activity, age, etc.) indexed by clause index
// - Tombstone deletion: inactive clauses marked, slots reused via free list
// - Periodic compaction when fragmentation exceeds threshold
//
// Benefits:
// - Eliminates per-clause []Literal allocations (was 1 alloc per clause)
// - Better cache locality: literals contiguous, metadata parallel arrays
// - Stable clause indices: watches don't need updates on deletion
// - O(1) clause addition: reuse free slot or append
// - O(1) clause deletion: mark tombstone, add to free list
type LearnedClausePool struct {
	// Contiguous literal storage for ALL learned clauses
	literals []cnf.Literal

	// Clause metadata (parallel arrays indexed by clause index)
	offsets   []int // Start offset in literals[]
	sizes     []int // Number of literals
	lbd       []int
	activity  []float64
	age       []int
	useCount  []int
	propCount []int
	active    []bool // false = deleted (tombstone)

	// Free list for reusing deleted slots
	freeList []int

	// Count of active clauses (excludes tombstones)
	count int

	// Total capacity (includes tombstones)
	capacity int

	// Configuration
	compactionThreshold float64 // Fragmentation ratio to trigger compaction (default 0.5)
}

// NewLearnedClausePool creates a new learned clause memory pool
func NewLearnedClausePool(initialCapacity int) *LearnedClausePool {
	return &LearnedClausePool{
		literals:            make([]cnf.Literal, 0, initialCapacity*4), // Assume avg 4 literals/clause
		offsets:             make([]int, 0, initialCapacity),
		sizes:               make([]int, 0, initialCapacity),
		lbd:                 make([]int, 0, initialCapacity),
		activity:            make([]float64, 0, initialCapacity),
		age:                 make([]int, 0, initialCapacity),
		useCount:            make([]int, 0, initialCapacity),
		propCount:           make([]int, 0, initialCapacity),
		active:              make([]bool, 0, initialCapacity),
		freeList:            make([]int, 0, 8),
		count:               0,
		capacity:            0,
		compactionThreshold: 0.5, // Compact when 50% tombstones
	}
}

// AddClause adds a new learned clause to the pool
// Returns the clause index (stable, only reused after deletion)
func (p *LearnedClausePool) AddClause(literals []cnf.Literal, lbd int) int {
	var clauseIdx int

	// Reuse free slot if available
	if len(p.freeList) > 0 {
		lastIdx := len(p.freeList) - 1
		clauseIdx = p.freeList[lastIdx]
		p.freeList = p.freeList[:lastIdx]

		// Calculate offset for new literals
		offset := len(p.literals)

		// Store literals contiguously
		p.literals = append(p.literals, literals...)

		// Update metadata at reused slot
		p.offsets[clauseIdx] = offset
		p.sizes[clauseIdx] = len(literals)
		p.lbd[clauseIdx] = lbd
		p.activity[clauseIdx] = 0.0
		p.age[clauseIdx] = 0
		p.useCount[clauseIdx] = 0
		p.propCount[clauseIdx] = 0
		p.active[clauseIdx] = true
	} else {
		// Allocate new slot
		clauseIdx = p.capacity
		p.capacity++

		// Calculate offset for new literals
		offset := len(p.literals)

		// Store literals contiguously
		p.literals = append(p.literals, literals...)

		// Append metadata
		p.offsets = append(p.offsets, offset)
		p.sizes = append(p.sizes, len(literals))
		p.lbd = append(p.lbd, lbd)
		p.activity = append(p.activity, 0.0)
		p.age = append(p.age, 0)
		p.useCount = append(p.useCount, 0)
		p.propCount = append(p.propCount, 0)
		p.active = append(p.active, true)
	}

	p.count++
	return clauseIdx
}

// DeleteClause marks a clause as deleted (tombstone)
// The slot will be reused by future AddClause calls
func (p *LearnedClausePool) DeleteClause(clauseIdx int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity || !p.active[clauseIdx] {
		return // Invalid or already deleted
	}

	// Mark as tombstone
	p.active[clauseIdx] = false
	p.freeList = append(p.freeList, clauseIdx)
	p.count--
}

// GetLiterals returns the literals for a clause (view into contiguous pool)
func (p *LearnedClausePool) GetLiterals(clauseIdx int) []cnf.Literal {
	if clauseIdx < 0 || clauseIdx >= p.capacity || !p.active[clauseIdx] {
		return nil
	}
	offset := p.offsets[clauseIdx]
	size := p.sizes[clauseIdx]
	return p.literals[offset : offset+size]
}

// GetLBD returns the LBD for a clause
func (p *LearnedClausePool) GetLBD(clauseIdx int) int {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return 999999
	}
	return p.lbd[clauseIdx]
}

// SetLBD sets the LBD for a clause
func (p *LearnedClausePool) SetLBD(clauseIdx int, lbd int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.lbd[clauseIdx] = lbd
}

// GetActivity returns the activity for a clause
func (p *LearnedClausePool) GetActivity(clauseIdx int) float64 {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return 0.0
	}
	return p.activity[clauseIdx]
}

// SetActivity sets the activity for a clause
func (p *LearnedClausePool) SetActivity(clauseIdx int, activity float64) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.activity[clauseIdx] = activity
}

// GetAge returns the age for a clause
func (p *LearnedClausePool) GetAge(clauseIdx int) int {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return 0
	}
	return p.age[clauseIdx]
}

// SetAge sets the age for a clause
func (p *LearnedClausePool) SetAge(clauseIdx int, age int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.age[clauseIdx] = age
}

// GetUseCount returns the use count for a clause
func (p *LearnedClausePool) GetUseCount(clauseIdx int) int {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return 0
	}
	return p.useCount[clauseIdx]
}

// SetUseCount sets the use count for a clause
func (p *LearnedClausePool) SetUseCount(clauseIdx int, count int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.useCount[clauseIdx] = count
}

// GetPropCount returns the propagation count for a clause
func (p *LearnedClausePool) GetPropCount(clauseIdx int) int {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return 0
	}
	return p.propCount[clauseIdx]
}

// SetPropCount sets the propagation count for a clause
func (p *LearnedClausePool) SetPropCount(clauseIdx int, count int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.propCount[clauseIdx] = count
}

// IncrementPropCount increments the propagation count
func (p *LearnedClausePool) IncrementPropCount(clauseIdx int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.propCount[clauseIdx]++
}

// IncrementUseCount increments the use count
func (p *LearnedClausePool) IncrementUseCount(clauseIdx int) {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return
	}
	p.useCount[clauseIdx]++
}

// IsActive returns true if clause is active (not deleted)
func (p *LearnedClausePool) IsActive(clauseIdx int) bool {
	if clauseIdx < 0 || clauseIdx >= p.capacity {
		return false
	}
	return p.active[clauseIdx]
}

// Count returns the number of active clauses
func (p *LearnedClausePool) Count() int {
	return p.count
}

// Capacity returns the total capacity (including tombstones)
func (p *LearnedClausePool) Capacity() int {
	return p.capacity
}

// FragmentationRatio returns the ratio of tombstones to total capacity
// Returns 0.0 if capacity is 0
func (p *LearnedClausePool) FragmentationRatio() float64 {
	if p.capacity == 0 {
		return 0.0
	}
	tombstones := p.capacity - p.count
	return float64(tombstones) / float64(p.capacity)
}

// ShouldCompact returns true if fragmentation exceeds threshold
func (p *LearnedClausePool) ShouldCompact() bool {
	return p.FragmentationRatio() > p.compactionThreshold
}

// Compact rebuilds the pool to remove tombstones and defragment literals
// This is expensive but reclaims memory and improves locality
// Returns mapping from old indices to new indices (for updating external references)
func (p *LearnedClausePool) Compact() []int {
	if p.count == 0 {
		// No active clauses, clear everything
		p.literals = p.literals[:0]
		p.offsets = p.offsets[:0]
		p.sizes = p.sizes[:0]
		p.lbd = p.lbd[:0]
		p.activity = p.activity[:0]
		p.age = p.age[:0]
		p.useCount = p.useCount[:0]
		p.propCount = p.propCount[:0]
		p.active = p.active[:0]
		p.freeList = p.freeList[:0]
		p.capacity = 0
		return []int{}
	}

	// Build mapping from old to new indices
	oldToNew := make([]int, p.capacity)
	for i := range oldToNew {
		oldToNew[i] = -1 // -1 means deleted
	}

	// Build new arrays
	newLiterals := make([]cnf.Literal, 0, len(p.literals))
	newOffsets := make([]int, 0, p.count)
	newSizes := make([]int, 0, p.count)
	newLBD := make([]int, 0, p.count)
	newActivity := make([]float64, 0, p.count)
	newAge := make([]int, 0, p.count)
	newUseCount := make([]int, 0, p.count)
	newPropCount := make([]int, 0, p.count)
	newActive := make([]bool, 0, p.count)

	newIdx := 0
	for oldIdx := 0; oldIdx < p.capacity; oldIdx++ {
		if !p.active[oldIdx] {
			continue // Skip tombstones
		}

		// Map old index to new index
		oldToNew[oldIdx] = newIdx

		// Copy literals
		oldOffset := p.offsets[oldIdx]
		oldSize := p.sizes[oldIdx]
		newOffset := len(newLiterals)
		newLiterals = append(newLiterals, p.literals[oldOffset:oldOffset+oldSize]...)

		// Append metadata
		newOffsets = append(newOffsets, newOffset)
		newSizes = append(newSizes, oldSize)
		newLBD = append(newLBD, p.lbd[oldIdx])
		newActivity = append(newActivity, p.activity[oldIdx])
		newAge = append(newAge, p.age[oldIdx])
		newUseCount = append(newUseCount, p.useCount[oldIdx])
		newPropCount = append(newPropCount, p.propCount[oldIdx])
		newActive = append(newActive, true)

		newIdx++
	}

	// Replace old arrays
	p.literals = newLiterals
	p.offsets = newOffsets
	p.sizes = newSizes
	p.lbd = newLBD
	p.activity = newActivity
	p.age = newAge
	p.useCount = newUseCount
	p.propCount = newPropCount
	p.active = newActive
	p.freeList = p.freeList[:0] // Clear free list
	p.capacity = p.count

	return oldToNew
}

// SetCompactionThreshold sets the fragmentation ratio that triggers compaction
func (p *LearnedClausePool) SetCompactionThreshold(threshold float64) {
	p.compactionThreshold = threshold
}

// GetLiteralsView returns a read-only view of the literal pool
// Useful for debugging/profiling
func (p *LearnedClausePool) GetLiteralsView() []cnf.Literal {
	return p.literals
}

// MemoryUsage returns approximate memory usage in bytes
func (p *LearnedClausePool) MemoryUsage() int {
	bytes := 0
	bytes += cap(p.literals) * 4                 // Literal is uint32
	bytes += cap(p.offsets) * 8                  // int is 8 bytes on 64-bit
	bytes += cap(p.sizes) * 8
	bytes += cap(p.lbd) * 8
	bytes += cap(p.activity) * 8                 // float64
	bytes += cap(p.age) * 8
	bytes += cap(p.useCount) * 8
	bytes += cap(p.propCount) * 8
	bytes += cap(p.active) * 1                   // bool is 1 byte
	bytes += cap(p.freeList) * 8
	return bytes
}
