package solver

import (
	"math"
	"satience/internal/cnf"
	"testing"
)

func branchSolver(c *cnf.CNF, mode int) *CDCLSolver {
	s := NewCDCLSolver(c)
	s.SetBranch(mode)
	return s
}

// phpUnsat builds pigeonhole PH(3,2): 3 pigeons, 2 holes -> UNSAT.
func phpUnsat() *cnf.CNF {
	numVars := 3 * 2
	var cls []cnf.Clause
	cell := func(p, h int) int32 { return int32(p*2 + h + 1) }
	for p := 0; p < 3; p++ {
		cls = append(cls, newClause(cell(p, 0), cell(p, 1)))
	}
	for h := 0; h < 2; h++ {
		for p := 0; p < 3; p++ {
			for q := p + 1; q < 3; q++ {
				cls = append(cls, newClause(-cell(p, h), -cell(q, h)))
			}
		}
	}
	return &cnf.CNF{NumVars: uint32(numVars), Clauses: cls, NumClauses: len(cls)}
}

func TestBranchPhpUnsatAllModes(t *testing.T) {
	for _, mode := range []int{branchCHB, branchLRB} {
		s := branchSolver(phpUnsat(), mode)
		if s.Solve() {
			t.Fatalf("mode=%d: php(3,2) should be UNSAT", mode)
		}
		conf := s.conflicts
		t.Logf("mode=%d php UNSAT conflicts=%d", mode, conf)
		if conf > 200000 {
			t.Fatalf("mode=%d: conflict explosion (%d) on tiny pigeonhole", mode, conf)
		}
	}
}

// randomSat builds an n-var random 3-CNF guaranteed satisfiable by a fixed
// hidden assignment (each clause includes at least one literal consistent with
// it). Deterministic LCG so the test is reproducible.
func randomSat(n, m int, seed uint32) *cnf.CNF {
	hidden := make([]bool, n)
	for i := range hidden {
		seed = seed*1103515245 + 12345
		hidden[i] = seed&1 == 1
	}
	var cls []cnf.Clause
	next := func() int {
		seed = seed*1103515245 + 12345
		return int(seed>>16) % n
	}
	for k := 0; k < m; k++ {
		lits := make([]int32, 3)
		allNeg := true
		for i := 0; i < 3; i++ {
			v := next()
			// pick polarity that is consistent with hidden fact (makes clause
			// satisfied by hidden) unless already guaranteed by another lit
			consistent := !(hidden[v]) // literal v true => var(false) ; we flip below
			var lit int32
			if hidden[v] {
				lit = -int32(v + 1)
			} else {
				lit = int32(v + 1)
				allNeg = false
			}
			_ = consistent
			lits[i] = lit
		}
		// ensure at least one positive-to-hidden literal so hidden satisfies it
		if allNeg {
			lits[0] = -lits[0]
		}
		cls = append(cls, newClause(lits...))
	}
	return &cnf.CNF{NumVars: uint32(n), Clauses: cls, NumClauses: len(cls)}
}

func TestBranchSatVerdictAndModel(t *testing.T) {
	c := randomSat(15, 45, 99)
	for _, mode := range []int{branchCHB, branchLRB} {
		s := branchSolver(c, mode)
		sat := s.Solve()
		t.Logf("mode=%d solved=%v conflicts=%d", mode, sat, s.conflicts)
		if !sat {
			t.Fatalf("mode=%d: expected SAT", mode)
		}
		if err := VerifySolution(c, s.assignments, false); err != nil {
			t.Fatalf("mode=%d: model does not verify: %v", mode, err)
		}
	}
}

func TestBranchSelectionUnassigned(t *testing.T) {
	c := phpUnsat()
	s := branchSolver(c, branchCHB)
	// seed activities before search
	for mv := range s.vsids.activity {
		s.vsids.activity[mv] = float64(mv) + 1
	}
	s.branchSeedFromVSIDS()
	for k := 0; k < 100; k++ {
		v := s.branchSelect(s.assignments)
		if int(v) >= len(s.assignments) || s.assignments[v].Level >= 0 {
			t.Fatalf("selection returned assigned/out-of-range var %d on iter %d", v, k)
		}
	}
}

// TestBranchSelectionSinksAssigned: after assigning some vars, selection must
// never return an assigned one even when it has a high score.
func TestBranchSelectionSinksAssigned(t *testing.T) {
	c := phpUnsat()
	s := branchSolver(c, branchCHB)
	for mv := range s.vsids.activity {
		s.vsids.activity[mv] = float64(mv) + 1
	}
	s.branchSeedFromVSIDS()
	// mark vars 0..3 assigned; vars 4..5 stay unassigned
	for i := 0; i < 4 && i < len(s.assignments); i++ {
		s.assignments[i].Level = 1
	}
	v := s.branchSelect(s.assignments)
	if int(v) < 4 {
		t.Fatalf("selection returned assigned var %d", v)
	}
	if int(v) >= len(s.assignments) || s.assignments[v].Level >= 0 {
		t.Fatalf("selection returned invalid var %d", v)
	}
}

func TestBranchDeterminism(t *testing.T) {
	c := phpUnsat()
	var c1, c2 int
	{
		s := branchSolver(c, branchCHB)
		s.Solve()
		c1 = s.conflicts
	}
	{
		s := branchSolver(c, branchCHB)
		s.Solve()
		c2 = s.conflicts
	}
	if c1 != c2 {
		t.Fatalf("CHB not deterministic: %d vs %d", c1, c2)
	}
}

func TestBranchScoresScale(t *testing.T) {
	s := branchSolver(phpUnsat(), branchCHB)
	for mv := range s.vsids.activity {
		s.vsids.activity[mv] = 500.0
	}
	s.branchSeedFromVSIDS()
	for _, sc := range s.branch.scores {
		if !(sc >= 0 && sc <= 1) || math.IsNaN(sc) {
			t.Fatalf("CHB score out of [0,1]: %v", sc)
		}
	}
}
