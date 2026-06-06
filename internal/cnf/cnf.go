package cnf

// Literal represents a SAT literal (variable with optional negation)
// Bit 31 is the sign (1 = negated), bits 0-30 are the variable index
type Literal uint32

const (
	litVarMask     uint32 = 0x7FFFFFFF
	litNegatedMask uint32 = 0x80000000
)

// Var returns the variable index (0-based)
func (l Literal) Var() uint32 {
	return uint32(l) & litVarMask
}

// IsNegated returns true if the literal is negated
func (l Literal) IsNegated() bool {
	return (uint32(l) & litNegatedMask) != 0
}

// Negate returns the negation of the literal
func (l Literal) Negate() Literal {
	return Literal(uint32(l) ^ litNegatedMask)
}

// ToDimacs converts to DIMACS format (1-based, signed integer)
func (l Literal) ToDimacs() int32 {
	v := int32(l.Var()) + 1 // Convert to 1-based
	if l.IsNegated() {
		return -v
	}
	return v
}

// NewLiteral creates a literal from a variable index and sign
func NewLiteral(varIdx uint32, negated bool) Literal {
	if negated {
		return Literal(varIdx | 0x80000000)
	}
	return Literal(varIdx)
}

// Clause represents a disjunction of literals
type Clause struct {
	Literals []Literal
	Learned  bool
}

// ClauseArena manages contiguous memory for clause storage
// All literals are stored in a single buffer for cache efficiency
// Clauses are referenced by (offset, length) pairs
type ClauseArena struct {
	buffer    []uint32 // Contiguous storage for all literals
	offsets   []int    // Start offset of each clause in buffer
	sizes     []int    // Number of literals in each clause
	learned   []bool   // Whether each clause is learned
	freeList  []int    // Indices of freed slots for reuse
}

// CNF represents a CNF formula
type CNF struct {
	NumVars    uint32
	Clauses    []Clause
	NumClauses int
	
	// Arena-based clause storage (alternative to Clauses slice)
	Arena *ClauseArena
}



// NewCNF creates a new CNF formula
func NewCNF(numVars uint32, numClauses int) *CNF {
	// Pre-allocate arena with estimated capacity
	arenaCapacity := numClauses * 4 // Average 4 literals per clause
	return &CNF{
		NumVars:    numVars,
		Clauses:    make([]Clause, 0, numClauses),
		NumClauses: 0,
		Arena:      NewClauseArena(arenaCapacity),
	}
}

// NewClauseArena creates a new clause arena with pre-allocated buffer
func NewClauseArena(capacity int) *ClauseArena {
	return &ClauseArena{
		buffer:   make([]uint32, 0, capacity),
		offsets:  make([]int, 0, capacity/4),
		sizes:    make([]int, 0, capacity/4),
		learned:  make([]bool, 0, capacity/4),
		freeList: make([]int, 0),
	}
}

// AddClause adds a clause to the CNF formula
func (c *CNF) AddClause(literals []Literal, learned bool) {
	c.Clauses = append(c.Clauses, Clause{
		Literals: literals,
		Learned:  learned,
	})
	c.NumClauses++
}

// LitToIndex converts a literal to an index
// varIdx * 2 + (0 for positive, 1 for negated)
func LitToIndex(lit Literal) int {
	varIdx := lit.Var()
	if lit.IsNegated() {
		return int(varIdx)*2 + 1
	}
	return int(varIdx) * 2
}

// IndexToLit converts an index back to a literal
func IndexToLit(idx int) Literal {
	varIdx := uint32(idx / 2)
	isNegated := (idx % 2) == 1
	return NewLiteral(varIdx, isNegated)
}

// Arena methods for clause allocation and access

// AllocateClause allocates a clause in the arena and returns its index
func (ca *ClauseArena) AllocateClause(literals []Literal, learned bool) int {
	// Reuse freed slot if available
	var idx int
	if len(ca.freeList) > 0 {
		idx = ca.freeList[len(ca.freeList)-1]
		ca.freeList = ca.freeList[:len(ca.freeList)-1]
		// Update existing entry
		ca.offsets[idx] = len(ca.buffer)
		ca.sizes[idx] = len(literals)
		ca.learned[idx] = learned
	} else {
		// Allocate new slot
		idx = len(ca.offsets)
		ca.offsets = append(ca.offsets, len(ca.buffer))
		ca.sizes = append(ca.sizes, len(literals))
		ca.learned = append(ca.learned, learned)
	}
	
	// Append literals to buffer
	for _, lit := range literals {
		ca.buffer = append(ca.buffer, uint32(lit))
	}
	
	return idx
}

// GetClauseLiterals returns the literals for a clause at given index
func (ca *ClauseArena) GetClauseLiterals(idx int) []Literal {
	offset := ca.offsets[idx]
	size := ca.sizes[idx]
	literals := make([]Literal, size)
	for i := 0; i < size; i++ {
		literals[i] = Literal(ca.buffer[offset+i])
	}
	return literals
}

// IsLearned returns whether a clause is learned
func (ca *ClauseArena) IsLearned(idx int) bool {
	return ca.learned[idx]
}

// FreeClause marks a clause slot as free for reuse
func (ca *ClauseArena) FreeClause(idx int) {
	ca.freeList = append(ca.freeList, idx)
}

// NumClauses returns the number of allocated clauses
func (ca *ClauseArena) NumClauses() int {
	return len(ca.offsets) - len(ca.freeList)
}

// Compact removes gaps from freed clauses and rebuilds indices
// Returns a mapping from old indices to new indices
func (ca *ClauseArena) Compact() []int {
	if len(ca.freeList) == 0 {
		return nil // No compaction needed
	}
	
	// Build mapping from old to new indices
	oldToNew := make([]int, len(ca.offsets))
	for i := range oldToNew {
		oldToNew[i] = i
	}
	
	// Create new arrays
	newBuffer := make([]uint32, 0, len(ca.buffer))
	newOffsets := make([]int, 0, len(ca.offsets))
	newSizes := make([]int, 0, len(ca.sizes))
	newLearned := make([]bool, 0, len(ca.learned))
	
	newIdx := 0
	for oldIdx := range ca.offsets {
		// Check if this index is in freeList
		isFree := false
		for _, freeIdx := range ca.freeList {
			if freeIdx == oldIdx {
				isFree = true
				break
			}
		}
		
		if !isFree {
			oldToNew[oldIdx] = newIdx
			newOffsets = append(newOffsets, len(newBuffer))
			newSizes = append(newSizes, ca.sizes[oldIdx])
			newLearned = append(newLearned, ca.learned[oldIdx])
			
			// Copy literals
			offset := ca.offsets[oldIdx]
			size := ca.sizes[oldIdx]
			for i := 0; i < size; i++ {
				newBuffer = append(newBuffer, ca.buffer[offset+i])
			}
			newIdx++
		}
	}
	
	ca.buffer = newBuffer
	ca.offsets = newOffsets
	ca.sizes = newSizes
	ca.learned = newLearned
	ca.freeList = ca.freeList[:0]
	
	return oldToNew
}

// Reset clears all clauses and resets the arena
func (ca *ClauseArena) Reset() {
	ca.buffer = ca.buffer[:0]
	ca.offsets = ca.offsets[:0]
	ca.sizes = ca.sizes[:0]
	ca.learned = ca.learned[:0]
	ca.freeList = ca.freeList[:0]
}

// CapacityBytes returns the current buffer capacity in bytes (for debugging)
func (ca *ClauseArena) CapacityBytes() int {
	return cap(ca.buffer) * 4  // 4 bytes per uint32
}


