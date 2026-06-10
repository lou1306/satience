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

// Watch represents a watched literal reference for a clause
// Used in the watched literals scheme for efficient propagation
type Watch struct {
	Clause   *Clause // Direct pointer to clause (nil if deleted)
	Blit     uint32  // Blocking literal index (the other watched literal)
	SymPos   int32   // Position of symmetric watch in the other watch list
}

// Clause represents a disjunction of literals
type Clause struct {
	Literals []Literal
	Learned  bool
}

// CNF represents a CNF formula
type CNF struct {
	NumVars    uint32
	Clauses    []Clause
	NumClauses int
	
	// Contiguous literal storage for original clauses (optimization)
	// All original clause literals stored in one array for better cache locality
	originalClauseOffsets []int // Start offset of each clause
	originalClauseSizes   []int // Number of literals in each clause
	literalPool           []uint32 // Contiguous storage for all original clause literals
}



// NewCNF creates a new CNF formula
func NewCNF(numVars uint32, numClauses int) *CNF {
	return &CNF{
		NumVars:    numVars,
		Clauses:    make([]Clause, 0, numClauses),
		NumClauses: 0,
	}
}

// AddClause adds a clause to the CNF formula (stores in contiguous pool)
func (c *CNF) AddClause(literals []Literal, learned bool) {
	// Store in Clauses slice for backwards compatibility
	c.Clauses = append(c.Clauses, Clause{
		Literals: literals,
		Learned:  learned,
	})
	
	// Also store in contiguous literal pool for cache efficiency
	offset := len(c.literalPool)
	c.originalClauseOffsets = append(c.originalClauseOffsets, offset)
	c.originalClauseSizes = append(c.originalClauseSizes, len(literals))
	
	for _, lit := range literals {
		c.literalPool = append(c.literalPool, uint32(lit))
	}
	
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


// GetOriginalClauseLiterals returns literals for an original clause (zero-allocation view)
// Returns offset and size into the literal pool
func (c *CNF) GetOriginalClauseLiterals(clauseIdx int) (offset int, size int, pool []uint32) {
	if clauseIdx < 0 || clauseIdx >= len(c.originalClauseOffsets) {
		return 0, 0, nil
	}
	return c.originalClauseOffsets[clauseIdx], c.originalClauseSizes[clauseIdx], c.literalPool
}

// NumOriginalClauses returns the number of original clauses
func (c *CNF) NumOriginalClauses() int {
	return len(c.originalClauseOffsets)
}

// GetOriginalClauseInfo returns the offset and size for an original clause
func (c *CNF) GetOriginalClauseInfo(clauseIdx int) (offset int, size int) {
	if clauseIdx < 0 || clauseIdx >= len(c.originalClauseOffsets) {
		return 0, 0
	}
	return c.originalClauseOffsets[clauseIdx], c.originalClauseSizes[clauseIdx]
}

// GetLiteralPool returns the contiguous literal storage
func (c *CNF) GetLiteralPool() []uint32 {
	return c.literalPool
}

// RebuildLiteralPool rebuilds the literal pool from Clauses slice
// Call this after preprocessing modifies Clauses directly
func (c *CNF) RebuildLiteralPool() {
	c.literalPool = make([]uint32, 0, c.NumClauses*4)
	c.originalClauseOffsets = make([]int, 0, c.NumClauses)
	c.originalClauseSizes = make([]int, 0, c.NumClauses)
	
	for _, clause := range c.Clauses {
		offset := len(c.literalPool)
		c.originalClauseOffsets = append(c.originalClauseOffsets, offset)
		c.originalClauseSizes = append(c.originalClauseSizes, len(clause.Literals))
		
		for _, lit := range clause.Literals {
			c.literalPool = append(c.literalPool, uint32(lit))
		}
	}
}


