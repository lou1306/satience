package solver

// parity.go — XOR / parity preprocessing via GF(2) Gaussian elimination.
//
// IMPORTANT distinction: a single CNF clause over k>1 variables is NOT a parity
// constraint (it forbids ONE corner of the hypercube, whereas a XOR equation
// forbids half of it). Parity/XOR structure lives in GROUPS of clauses: a full
// "parity family" over a variable support S of size d is the set of all
// 2^(d-1) clauses whose polarity patterns are exactly those of one XOR
// equation over S (the `algebra_xor` / Tseitin encodings, e.g. a Tseitin vertex
// of degree d yields 2^(d-1) size-d clauses). This file DETECTS those families
// and runs Gaussian elimination over GF(2) to derive sound consequences.
//
// SOUNDNESS: this is ADD-ONLY. It only derives logical consequences (units and
// binary equivalences) and appends them to the original clause database. It
// never removes a variable or clause and never touches model reconstruction, so
// satisfiability is preserved and the existing BVE reconstruction path is
// unaffected. A derived unit/binary is a consequence of clauses already present;
// appending it cannot change satisfiability. This is the safe, decidable first
// increment (full variable substitution / Shatter is a follow-up).
//
// Gated: -parity (default ON; -parity=false disables), -parity-max-len,
// -parity-budget (hard cap on derived binaries).

import (
	"math/bits"

	"satience/internal/cnf"
)

// parityMaxArityMax caps the maximum supported parity-family support size.
// It bounds the family completeness expectation 2^(d-1) and the per-literal
// code bitsets (cod) so `1 << (d-1)` and `1 << bi` can never overflow, and
// keeps the family-size requirement (2^(d-1) clauses) feasible.
const parityMaxArityMax = 20

// verifyParityMaxArity bounds the per-family exhaustive verification oracle.
// d <= 10 means at most 2^10 assignments per family — trivial. Larger families
// are impractical to gather in the DB (2^(d-1) clauses) and are covered by the
// integrated solver tests, so they are skipped by the oracle.
const verifyParityMaxArity = 10

// parityRow is a single GF(2) equation  XOR(vars) == parity. vars is sorted
// ascending; duplicate vars never occur.
type parityRow struct {
	vars   []uint32
	parity bool
}

// gaussState is a GF(2) linear basis over parity rows.
//
// Representation: basis[pivot] is the equation  pivot ⊕ XOR(vars) == parity,
// i.e. vars are the pivot's "other" variables and DO NOT include the pivot.
// pivot is chosen as the LARGEST variable of a row.
type gaussState struct {
	basis  map[uint32]parityRow
	units  []cnf.Literal    // derived unit clause literal (assign this = true)
	binars [][2]cnf.Literal // derived binary clause literal pairs
	unsat  bool
}

func newGaussState() *gaussState {
	return &gaussState{basis: make(map[uint32]parityRow)}
}

// xorRows returns (v1 XOR v2, p1 XOR p2) using symmetric difference. Inputs must
// be sorted with unique vars.
func xorRows(v1, v2 []uint32, p1, p2 bool) ([]uint32, bool) {
	out := make([]uint32, 0, len(v1)+len(v2))
	i, j := 0, 0
	for i < len(v1) && j < len(v2) {
		if v1[i] < v2[j] {
			out = append(out, v1[i])
			i++
		} else if v2[j] < v1[i] {
			out = append(out, v2[j])
			j++
		} else {
			i++
			j++
		}
	}
	out = append(out, v1[i:]...)
	out = append(out, v2[j:]...)
	return out, p1 != p2
}

// normRow sorts vars ascending and cancels duplicate pairs (a var appearing an
// even number of times disappears).
func normRow(vars []uint32, parity bool) ([]uint32, bool) {
	if len(vars) <= 1 {
		return vars, parity
	}
	sorted := make([]uint32, len(vars))
	copy(sorted, vars)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	dst := sorted[:0]
	for i := 0; i < len(sorted); {
		j := i
		for j < len(sorted) && sorted[j] == sorted[i] {
			j++
		}
		if (j-i)%2 == 1 {
			dst = append(dst, sorted[i])
		}
		i = j
	}
	return dst, parity
}

// insert folds a parity row into the basis via forward Gaussian elimination.
func (g *gaussState) insert(vars []uint32, parity bool) {
	cur, cp := vars, parity
	for len(cur) > 0 {
		pivot := cur[len(cur)-1] // largest var
		base, ok := g.basis[pivot]
		if !ok {
			g.basis[pivot] = parityRow{vars: cur[:len(cur)-1], parity: cp}
			return
		}
		nv, np := xorRows(cur[:len(cur)-1], base.vars, cp, base.parity)
		cur, cp = normRow(nv, np)
	}
	if cp {
		g.unsat = true
	}
}

// finalize performs back-substitution to reduced row echelon form, then emits
// units (a row with no other vars: pivot = parity) and binary consequences (a
// row with exactly one other var: pivot ⊕ v = parity). Sound: only logical
// consequences of the input rows.
func (g *gaussState) finalize() {
	pivots := make([]uint32, 0, len(g.basis))
	for pv := range g.basis {
		pivots = append(pivots, pv)
	}
	// descending so larger pivots are cleared from smaller-pivot rows last-first
	sortUint32Desc(pivots)

	for _, pv := range pivots {
		B := g.basis[pv] // pivot ⊕ XOR(B.vars) == B.parity
		for _, rho := range pivots {
			if rho == pv {
				continue
			}
			R := g.basis[rho]
			idx := indexOf(R.vars, pv)
			if idx < 0 {
				continue
			}
			// eliminate pv from R: R = R XOR (B with pivot included)
			nv := append([]uint32(nil), R.vars...)
			nv = append(nv[:idx], nv[idx+1:]...) // drop pv
			cur, cp := xorRows(nv, B.vars, R.parity, B.parity)
			cur, cp = normRow(cur, cp)
			g.basis[rho] = parityRow{vars: cur, parity: cp}
		}
	}

	seen := make(map[string]bool, len(g.basis))
	for _, pv := range pivots {
		r := g.basis[pv] // pivot ⊕ XOR(r.vars) == r.parity
		switch len(r.vars) {
		case 0:
			// pivot = r.parity  -> unit
			if r.parity {
				g.units = append(g.units, cnf.NewLiteral(pv, false)) // pivot true
			} else {
				g.units = append(g.units, cnf.NewLiteral(pv, true)) // pivot false
			}
		case 1:
			k := rowKey(pv, r.vars[0], r.parity)
			if seen[k] {
				continue
			}
			seen[k] = true
			a, b := litForYParity(pv, r.vars[0], r.parity)
			g.binars = append(g.binars, a, b)
		}
	}
}

func indexOf(vars []uint32, v uint32) int {
	for i, x := range vars {
		if x == v {
			return i
		}
	}
	return -1
}

func sortUint32Desc(v []uint32) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] > v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func rowKey(a, b uint32, p bool) string {
	x, y := a, b
	if y < x {
		x, y = y, x
	}
	var pb byte
	if p {
		pb = 1
	}
	// Full 32-bit width for both variables so vars > 65535 are not conflated
	// (a 16-bit key silently collapses distinct rows on large instances).
	return string([]byte{
		byte(x), byte(x >> 8), byte(x >> 16), byte(x >> 24),
		byte(y), byte(y >> 8), byte(y >> 16), byte(y >> 24),
		pb})
}

// litForYParity builds the two binary-consequence clauses for n1⊕n2 == p.
// p false (n1==n2): (n1∨¬n2),(¬n1∨n2).  p true (n1!=n2): (n1∨n2),(¬n1∨¬n2).
func litForYParity(n1, n2 uint32, p bool) ([2]cnf.Literal, [2]cnf.Literal) {
	a := cnf.NewLiteral(n1, false)
	na := cnf.NewLiteral(n1, true)
	b := cnf.NewLiteral(n2, false)
	nb := cnf.NewLiteral(n2, true)
	if !p {
		return [2]cnf.Literal{a, nb}, [2]cnf.Literal{na, b}
	}
	return [2]cnf.Literal{a, b}, [2]cnf.Literal{na, nb}
}

// derivedBinaryKey canonicalizes an unordered pair of literals into a wide,
// collision-free-and-non-overflowing key for deduplicating derived equivalence
// clauses. The two literals are ordered by (Var, polarity) and all four Var
// bytes are kept, so distinct pairs on large instances are never conflated.
func derivedBinaryKey(a, b cnf.Literal) string {
	if b.Var() < a.Var() || (b.Var() == a.Var() && b.IsNegated()) {
		a, b = b, a
	}
	var k [9]byte
	aV := uint32(a.ToDimacs())
	bV := uint32(b.ToDimacs())
	for i := 0; i < 4; i++ {
		k[i] = byte(aV >> (8 * i))
		k[4+i] = byte(bV >> (8 * i))
	}
	k[8] = 1
	return string(k[:])
}

// verifyParityRows is the independent soundness oracle for parity detection.
// detectParityRows encodes the family's XOR constant with a non-obvious `!p`
// inversion; a sign error there would feed a wrong row into Gaussian
// elimination and can produce a FALSE UNSAT (g.unsat). This function re-derives
// each detected family's constant straight from the clause semantics WITHOUT
// reusing that inversion, by exhaustive enumeration over the family support.
//
// For each detected row with support S (size d <= verifyParityMaxArity) it
// gathers the clauses whose variable support is exactly S and checks that:
//   - exactly 2^(d-1) of the 2^d assignments satisfy all those clauses (the
//     family is complete), and
//   - every satisfying assignment has the SAME true-count parity, and
//   - that common parity equals the row's stored parity (the XOR constant).
//
// Returns false if any family fails, in which case callers must not derive or
// trust any parity consequence (see analyzeParity).
func (s *CDCLSolver) verifyParityRows(rows []parityRow) bool {
	// Group original clauses by their (variable-ordered) support so each family
	// is checked against exactly the clauses that gave rise to its row.
	type gClause struct {
		lits []cnf.Literal
	}
	fams := make(map[string][]gClause)
	for i := range s.cnf.Clauses {
		lits := s.cnf.Clauses[i].Literals
		d := len(lits)
		if d < 3 || d > verifyParityMaxArity {
			continue
		}
		sup := make([]uint32, 0, d)
		for _, l := range lits {
			sup = append(sup, l.Var())
		}
		for i := 1; i < len(sup); i++ {
			for j := i; j > 0 && sup[j] < sup[j-1]; j-- {
				sup[j], sup[j-1] = sup[j-1], sup[j]
			}
		}
		fams[supportKey(sup)] = append(fams[supportKey(sup)], gClause{lits: lits})
	}

	for _, r := range rows {
		d := len(r.vars)
		if d < 3 || d > verifyParityMaxArity {
			continue // impractical to enumerate; covered by integrated tests
		}
		clauses := fams[supportKey(r.vars)]
		if len(clauses) != 1<<(d-1) {
			s.Log("c [parity] verify: family of size %d is not complete (%d clauses)\n", d, len(clauses))
			return false
		}
		// var index of each support variable for assignment lookup.
		varIndex := make(map[uint32]int, d)
		for i, v := range r.vars {
			varIndex[v] = i
		}
		nAllowed := 0
		allowedParity := -1
		for mask := 0; mask < 1<<d; mask++ {
			satAll := true
			for _, cl := range clauses {
				cok := false
				for _, l := range cl.lits {
					iv := varIndex[l.Var()]
					assigned := (mask>>iv)&1 == 1
					if l.IsNegated() != assigned {
						cok = true
						break
					}
				}
				if !cok {
					satAll = false
					break
				}
			}
			if satAll {
				nAllowed++
				pc := bits.OnesCount(uint(mask)) & 1
				if allowedParity == -1 {
					allowedParity = pc
				} else if allowedParity != pc {
					s.Log("c [parity] verify: family support %v has mixed satisfying parities\n", r.vars)
					return false
				}
			}
		}
		want := 1 << (d - 1)
		if nAllowed != want {
			s.Log("c [parity] verify: expected %d satisfying assignments, got %d for support %v\n",
				want, nAllowed, r.vars)
			return false
		}
		if allowedParity != -1 && (allowedParity == 1) != r.parity {
			s.Log("c [parity] verify: row parity mismatch for support %v (got allowed parity %d)\n",
				r.vars, allowedParity)
			return false
		}
	}
	return true
}


// Gaussian elimination, and appends derived unit/binary clauses to the DB.
// Returns UNSAT on a parity contradiction, else UNKNOWN. SOUND and ADD-ONLY.
func (s *CDCLSolver) analyzeParity() SolveResult {
	if !s.parityEnabled || s.parityMaxArity < 3 {
		return UNKNOWN
	}
	if s.parityMaxArity > parityMaxArityMax {
		s.parityMaxArity = parityMaxArityMax
	}
	rows, ok := s.detectParityRows()
	if !ok {
		// detectParityRows currently always returns ok=true (see there); this
		// path is retained for future use and would signal UNSAT directly.
		return UNSAT
	}
	if len(rows) == 0 {
		return UNKNOWN
	}

	// Independent soundness oracle: re-derive each detected family's equation
	// constant straight from the clause semantics, without trusting the
	// detectParityRows `!p` inversion. A wrong row constant, fed into Gaussian
	// elimination, can produce a false UNSAT via g.unsat, so on any mismatch we
	// refuse to derive anything rather than risk it.
	if !s.verifyParityRows(rows) {
		s.Log("c [parity] verifyParityRows failed -> skipping parity derivation (no risk of false UNSAT)\n")
		return UNKNOWN
	}

	g := newGaussState()
	for _, r := range rows {
		if g.unsat {
			break
		}
		g.insert(r.vars, r.parity)
	}
	if g.unsat {
		s.Log("c [parity] UNSAT via parity contradiction (%d rows)\n", len(rows))
		return UNSAT
	}
	g.finalize()

	s.parityRowsFound += len(rows)
	s.parityUnits += len(g.units)
	s.addDerivedParityClauses(g)
	s.Log("c [parity] rows=%d units=%d binaries=%d\n", len(rows), len(g.units), s.parityBinaries)
	return UNKNOWN
}

// addDerivedParityClauses appends derived units and binaries to the original
// DB, skipping duplicates, capped by parityBudget. Units added as unit clauses
// are then assigned by the existing unit-propagation preprocessing stage.
func (s *CDCLSolver) addDerivedParityClauses(g *gaussState) {
	if s.parityBudget <= 0 {
		return
	}
	seen := make(map[string]bool, len(g.binars))
	for _, lit := range g.units {
		s.cnf.Clauses = append(s.cnf.Clauses, cnf.Clause{Literals: []cnf.Literal{lit}})
		s.cnf.NumClauses++
	}
	added := 0
	for i := 0; i < len(g.binars); i += 2 {
		if added >= s.parityBudget {
			break
		}
		c1 := g.binars[i]
		c2 := g.binars[i+1]
		// add both clauses of the equivalence pair (dedup by normalized sig)
		for _, pair := range [][2]cnf.Literal{c1, c2} {
			x, y := pair[0], pair[1]
			if x.Var() == y.Var() {
				continue
			}
			// Canonical, full-width dedup key: order-normalized vars + polarity
			// bits, so x∨y and y∨x (and the same pair of normally-polar vars) are
			// never double-added and no 32-bit overflow can collapse distinct rows.
			k := derivedBinaryKey(x, y)
			if seen[k] {
				continue
			}
			seen[k] = true
			s.cnf.Clauses = append(s.cnf.Clauses, cnf.Clause{Literals: []cnf.Literal{x, y}})
			s.cnf.NumClauses++
			added++
		}
	}
	s.parityBinaries += added
}

// detectParityRows scans original clauses, groups them by sorted variable
// support, and emits a parity row for every support carrying a COMPLETE parity
// family (2^(d-1) clauses over d vars whose polarity patterns are exactly one
// XOR equation). ok=false signals UNSAT (unused here; reserved).
func (s *CDCLSolver) detectParityRows() ([]parityRow, bool) {
	type entry struct {
		support []uint32
		code    int
	}
	groups := make(map[string][]entry)

	for i := range s.cnf.Clauses {
		clause := s.cnf.Clauses[i].Literals
		d := len(clause)
		if d < 3 || d > s.parityMaxArity {
			continue
		}
		sup := make([]uint32, 0, d)
		neg := make(map[uint32]bool, d)
		for _, l := range clause {
			sup = append(sup, l.Var())
			neg[l.Var()] = l.IsNegated()
		}
		for i := 1; i < len(sup); i++ {
			for j := i; j > 0 && sup[j] < sup[j-1]; j-- {
				sup[j], sup[j-1] = sup[j-1], sup[j]
			}
		}
		key := supportKey(sup)
		// code bit b set iff the literal of support[b] is negated (support-ordered)
		cod := 0
		for bi := 0; bi < d; bi++ {
			if neg[sup[bi]] {
				cod |= 1 << bi
			}
		}
		groups[key] = append(groups[key], entry{support: sup, code: cod})
	}

	rows := make([]parityRow, 0)
	for _, es := range groups {
		d := len(es[0].support)
		if d < 3 {
			continue
		}
		if len(es) != (1 << (d - 1)) {
			continue
		}
		codes := make([]int, 0, len(es))
		for _, e := range es {
			codes = append(codes, e.code)
		}
		p, ok := isParityFamily(codes, d)
		if !ok {
			continue
		}
		// The clause patterns have popcount parity p, i.e. they FORBID assignments
		// whose true-set has parity p. The XOR equation satisfied by the allowed
		// assignments therefore has constant 1 XOR p (inverse). Storing p directly
		// would invert every row and make SAT parity systems compose into a false
		// contradiction.
		rows = append(rows, parityRow{vars: es[0].support, parity: !p})
	}
	return rows, true
}

func supportKey(sup []uint32) string {
	buf := make([]byte, 0, len(sup)*4)
	for _, v := range sup {
		buf = append(buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return string(buf)
}

// isParityFamily reports whether codes are exactly all d-bit codes of a single
// popcount parity, and returns that parity.
func isParityFamily(codes []int, d int) (bool, bool) {
	need := 1 << (d - 1)
	if len(codes) != need {
		return false, false
	}
	for _, wantParity := range []bool{false, true} {
		seen := make(map[int]bool, len(codes))
		ok := true
		for _, c := range codes {
			pop := 0
			for t := c; t != 0; t &= t - 1 {
				pop++
			}
			if (pop%2 == 1) != wantParity {
				ok = false
				break
			}
			if seen[c] {
				ok = false
				break
			}
			seen[c] = true
		}
		if ok {
			return wantParity, true
		}
	}
	return false, false
}
