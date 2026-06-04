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

// BinaryClause represents a binary clause (2 literals) in compact form
// Stored as two uint32 values for cache efficiency
type BinaryClause struct {
	Lit1 uint32 // First literal (raw uint32)
	Lit2 uint32 // Second literal (raw uint32)
}

// TernaryClause represents a ternary clause (3 literals) in compact form
type TernaryClause struct {
	Lit1 uint32
	Lit2 uint32
	Lit3 uint32
}

// Clause represents a disjunction of literals
type Clause struct {
	Literals []Literal
	Learned  bool
}

// CNF represents a CNF formula with optimized storage for short clauses
type CNF struct {
	NumVars        uint32
	Clauses        []Clause
	NumClauses     int
	BinaryClauses  []BinaryClause  // Binary clauses stored separately for fast propagation
	TernaryClauses []TernaryClause // Ternary clauses stored separately
}

// NewCNF creates a new CNF formula
func NewCNF(numVars uint32, numClauses int) *CNF {
	return &CNF{
		NumVars:        numVars,
		Clauses:        make([]Clause, 0, numClauses),
		NumClauses:     0,
		BinaryClauses:  make([]BinaryClause, 0, numClauses/2),
		TernaryClauses: make([]TernaryClause, 0, numClauses/3),
	}
}

// AddClause adds a clause to the CNF formula and categorizes it by size
func (c *CNF) AddClause(literals []Literal, learned bool) {
	c.Clauses = append(c.Clauses, Clause{
		Literals: literals,
		Learned:  learned,
	})
	c.NumClauses++
	
	// Store binary and ternary clauses in optimized format
	switch len(literals) {
	case 2:
		c.BinaryClauses = append(c.BinaryClauses, BinaryClause{
			Lit1: uint32(literals[0]),
			Lit2: uint32(literals[1]),
		})
	case 3:
		c.TernaryClauses = append(c.TernaryClauses, TernaryClause{
			Lit1: uint32(literals[0]),
			Lit2: uint32(literals[1]),
			Lit3: uint32(literals[2]),
		})
	}
}

// ClearShortClauses clears the binary and ternary clause caches
// Called when clauses are modified during preprocessing
func (c *CNF) ClearShortClauses() {
	c.BinaryClauses = nil
	c.TernaryClauses = nil
}

// RebuildShortClauses rebuilds the binary and ternary clause caches
// Should be called after preprocessing modifies clauses
func (c *CNF) RebuildShortClauses() {
	c.BinaryClauses = make([]BinaryClause, 0, len(c.Clauses)/2)
	c.TernaryClauses = make([]TernaryClause, 0, len(c.Clauses)/3)
	
	for _, clause := range c.Clauses {
		if clause.Learned {
			continue // Only index original clauses
		}
		switch len(clause.Literals) {
		case 2:
			c.BinaryClauses = append(c.BinaryClauses, BinaryClause{
				Lit1: uint32(clause.Literals[0]),
				Lit2: uint32(clause.Literals[1]),
			})
		case 3:
			c.TernaryClauses = append(c.TernaryClauses, TernaryClause{
				Lit1: uint32(clause.Literals[0]),
				Lit2: uint32(clause.Literals[1]),
				Lit3: uint32(clause.Literals[2]),
			})
		}
	}
}
