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
// Used in the watched literals scheme for efficient propagation.
// Watched literals are always kept at positions 0 and 1 in the clause
// (MiniSat-style). The position of this watch (myPos) is derived at
// propagation time by comparing clauseLits[0] against the false literal,
// avoiding the 4-byte padding that WatchPos uint8 would add (12→8 bytes,
// 5→8 watches per cache line).
type Watch struct {
	ClauseIdx int32  // Clause index: >=0 for original, <0 for learned (-learnedIdx-1)
	Blit      uint32 // Blocking literal (the other watched literal), cached for fast skip
}

// Clause represents a disjunction of literals
type Clause struct {
	Literals []Literal
	Learned  bool
}

// ClauseMetadata packs learned clause metadata into a single struct for cache efficiency.
// Only LBD and PropCount are read by the live deletion code; the other fields were
// removed as dead computation (Activity/Score/ScoreDirty/UseCount/Age/ID were never read).
type ClauseMetadata struct {
	LBD       int  // LBD at time of learning
	PropCount int  // Times caused propagation
}

// CNF represents a CNF formula
type CNF struct {
	NumVars    uint32
	Clauses    []Clause
	NumClauses int

	// Contiguous literal storage for original clauses (optimization)
	// All original clause literals stored in one array for better cache locality
	originalClauseOffsets []int    // Start offset of each clause
	originalClauseSizes   []int    // Number of literals in each clause
	literalPool           []Literal // Contiguous storage for all original clause literals
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
		c.literalPool = append(c.literalPool, lit)
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
func (c *CNF) GetOriginalClauseLiterals(clauseIdx int) (offset int, size int, pool []Literal) {
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
func (c *CNF) GetLiteralPool() []Literal {
	return c.literalPool
}

// RebuildLiteralPool rebuilds the literal pool from Clauses slice
// Call this after preprocessing modifies Clauses directly
func (c *CNF) RebuildLiteralPool() {
	c.literalPool = make([]Literal, 0, c.NumClauses*4)
	c.originalClauseOffsets = make([]int, 0, c.NumClauses)
	c.originalClauseSizes = make([]int, 0, c.NumClauses)

	for _, clause := range c.Clauses {
		offset := len(c.literalPool)
		c.originalClauseOffsets = append(c.originalClauseOffsets, offset)
		c.originalClauseSizes = append(c.originalClauseSizes, len(clause.Literals))

		c.literalPool = append(c.literalPool, clause.Literals...)
	}

	for i := range c.Clauses {
		off := c.originalClauseOffsets[i]
		sz := c.originalClauseSizes[i]
		c.Clauses[i].Literals = c.literalPool[off : off+sz]
	}
}
