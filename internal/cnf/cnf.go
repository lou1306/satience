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
// (MiniSat-style). The watch position (myPos) is packed into ClauseIdx
// bit 30, avoiding a clause-data cache miss to derive it at propagation
// time. Watch is 8 bytes (8 per cache line).
type Watch struct {
	ClauseIdx int32  // Bit 31: learned flag, bit 30: myPos, bits 0-29: clause index
	Blit      uint32 // Blocking literal (the other watched literal), cached for fast skip
}

// Clause represents a disjunction of literals
type Clause struct {
	Literals []Literal
	Learned  bool
}

// ClauseLoc packs a clause's offset and size into the literal pool as two int32s
// (8 bytes), so a single load fetches both fields. Used for original-clause SoA
// access in the propagation hot path, replacing the 32-byte Clause struct load.
type ClauseLoc struct {
	Offset int32
	Size   int32
}

// ClauseMetadata packs learned clause metadata into a single struct for cache efficiency.
// LBD is int32 (values are small: LBD ≤ clause size).
// SearchHint caches the last-known replacement position for the watched-literal
// replacement scan (probe-then-scan optimization). 0 = no hint (scan from pos 2).
// Activity is VSIDS-style decayed clause activity used to order deletion
// candidates within LBD tiers (0 = pure FIFO when claActivityEnabled=false).
type ClauseMetadata struct {
	LBD        int32   // LBD at time of learning
	SearchHint int32   // Last-known replacement position in clause (0 = no hint)
	Activity   float64 // VSIDS-style decayed activity for deletion ordering
}

// CNF represents a CNF formula
type CNF struct {
	NumVars    uint32
	Clauses    []Clause
	NumClauses int

	// Contiguous literal storage for original clauses (optimization)
	// All original clause literals stored in one array for better cache locality
	originalClauseOffsets []int       // Start offset of each clause
	originalClauseSizes   []int       // Number of literals in each clause
	originalClauseLocs    []ClauseLoc // Packed (Offset, Size) per clause — 8B vs 32B Clause struct
	literalPool           []Literal   // Contiguous storage for all original clause literals
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

// GetOriginalClauseLocs returns the packed (Offset, Size) array for original clauses
func (c *CNF) GetOriginalClauseLocs() []ClauseLoc {
	return c.originalClauseLocs
}

// RebuildLiteralPool rebuilds the literal pool from Clauses slice
// Call this after preprocessing modifies Clauses directly
func (c *CNF) RebuildLiteralPool() {
	c.literalPool = make([]Literal, 0, c.NumClauses*4)
	c.originalClauseOffsets = make([]int, 0, c.NumClauses)
	c.originalClauseSizes = make([]int, 0, c.NumClauses)
	c.originalClauseLocs = make([]ClauseLoc, 0, c.NumClauses)

	for _, clause := range c.Clauses {
		offset := len(c.literalPool)
		size := len(clause.Literals)
		c.originalClauseOffsets = append(c.originalClauseOffsets, offset)
		c.originalClauseSizes = append(c.originalClauseSizes, size)
		c.originalClauseLocs = append(c.originalClauseLocs, ClauseLoc{
			Offset: int32(offset),
			Size:   int32(size),
		})

		c.literalPool = append(c.literalPool, clause.Literals...)
	}

	for i := range c.Clauses {
		off := c.originalClauseOffsets[i]
		sz := c.originalClauseSizes[i]
		c.Clauses[i].Literals = c.literalPool[off : off+sz]
	}
}
