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
// Gated: -parity (default off -> bit-identical), -parity-max-len,
// -parity-budget (hard cap on derived binaries).

import (
	"satience/internal/cnf"
)

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
	return string([]byte{byte(x), byte(x >> 8), byte(y), byte(y >> 8), pb})
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

// analyzeParity detects full parity families over the original clauses, runs
// Gaussian elimination, and appends derived unit/binary clauses to the DB.
// Returns UNSAT on a parity contradiction, else UNKNOWN. SOUND and ADD-ONLY.
func (s *CDCLSolver) analyzeParity() SolveResult {
	if !s.parityEnabled || s.parityMaxArity < 3 {
		return UNKNOWN
	}
	rows, ok := s.detectParityRows()
	if !ok {
		return UNSAT
	}
	if len(rows) == 0 {
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
	seen := make(map[uint32]bool, len(g.binars))
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
			k := uint32(x)*31 + uint32(y)
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

// cacheParityRows detects parity families over the CURRENT (post-preprocessing)
// original clause database and snapshots their supports + constants for on-the-fly
// use during search. Rows detected here are implicates of the formula the search
// works on, so deriving from them at any point is sound. Call before cnf.Clauses
// is released (SolveWithResult sets it nil at search start).
func (s *CDCLSolver) cacheParityRows() {
	if !s.parityOnTheFly || s.parityMaxArity < 3 {
		return
	}
	rows, ok := s.detectParityRows()
	if !ok || len(rows) == 0 {
		return
	}
	s.parityRows = make([][]uint32, len(rows))
	s.parityRowParity = make([]bool, len(rows))
	for i, r := range rows {
		v := make([]uint32, len(r.vars))
		copy(v, r.vars)
		s.parityRows[i] = v
		s.parityRowParity[i] = r.parity
	}
	s.parityRowsFound = len(rows)
	s.Log("c [parity] on-the-fly: %d rows cached for insearch propagation\n", len(rows))
}

// parityPropagate is the on-the-fly parity engine. It sweeps the cached parity
// rows against the current trail and (a) forces a residual unit when a row has
// exactly one unassigned variable, storing a synthesized size-d reason clause,
// or (b) signals a conflict when a row is fully assigned with the wrong parity.
//
// SOUNDNESS: a row  XOR(vars)==p  forbids all true-sets of parity != p. With all
// but one var assigned (acc = XOR of their values), the last var u is forced to
// p XOR acc; the corner "others as-assigned, u != forced" violates the row, so
// the clause  { u=forced } ∪ { v = current }  forbids exactly that forbidden
// corner -> a universal implicate, valid everywhere. Fully-assigned mismatched
// rows give an all-false implicate = a valid conflict clause. Because these are
// ordinary clauses stored through storeLearnedClause, reasons and the BIG work
// normally and reason clauses are protected from deletion.
//
// Returns (acted, conflictClause): acted=true if something happened (a unit was
// enqueued or a conflict found); conflictClause non-nil iff that something is a
// conflict. The caller re-runs propagation after acted to reach the joint fixpoint.
func (s *CDCLSolver) parityPropagate() (bool, *cnf.Clause) {
	if !s.parityOnTheFly || len(s.parityRows) == 0 || s.parityBudget <= 0 {
		return false, nil
	}
	acted := false
	for ri := range s.parityRows {
		if s.parityLearned >= s.parityBudget {
			break
		}
		r := s.parityRows[ri]
		acc := false
		var un uint32
		unCount := 0
		for _, v := range r {
			a := s.assignments[v]
			if a.Level < 0 {
				un = v
				unCount++
			} else if a.Value {
				acc = !acc
			}
		}
		if unCount > 1 {
			continue
		}
		rowParity := s.parityRowParity[ri]

		if unCount == 0 {
			// Fully assigned. Violated iff XOR of values != rowParity.
			if acc == rowParity {
				continue
			}
			// Conflict: all-false implicate over the row's current literals.
			lits := make([]cnf.Literal, 0, len(r))
			for _, v := range r {
				lits = append(lits, cnf.NewLiteral(v, s.assignments[v].Value))
			}
			s.conflictClauseBuf.Literals = lits
			s.conflictClauseBuf.Learned = true
			if s.level == 0 {
				s.emptyClauseFound = true
			}
			return true, &s.conflictClauseBuf
		}

		// unCount == 1: force u = rowParity XOR acc.
		if u, val := un, rowParity != acc; s.assignments[u].Level >= 0 {
			_ = val
			continue
		} else {
			// Synthesize reason clause: position 0 = u's forced literal
			// (unassigned), then each other var's currently-false literal.
			reason := make([]cnf.Literal, 0, len(r))
			reason = append(reason, cnf.NewLiteral(u, !val))
			for _, v := range r {
				if v == u {
					continue
				}
				reason = append(reason, cnf.NewLiteral(v, s.assignments[v].Value))
			}
			s.tmpLearnedLits = s.tmpLearnedLits[:0]
			s.tmpLearnedLits = append(s.tmpLearnedLits, reason...)
			if !s.storeLearnedClause(2) {
				continue
			}
			learnedIdx := s.lastLearnedClauseIdx
			s.parityLearned++
			s.assignments[u] = Assignment{Value: val, Level: int32(s.level), Reason: int32(-learnedIdx - 5), SavedPhase: !val}
			s.litTrue[u*2] = val
			s.litTrue[u*2+1] = !val
			s.trail = append(s.trail, u)
			s.numUnassigned--
			s.propagations++
			acted = true
		}
	}
	return acted, nil
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
