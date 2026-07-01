package solver

import (
	"fmt"
	"satience/internal/cnf"
	"testing"
)

func TestPureLiteralSoundness1(t *testing.T) {
	// Test: Pure literal should not create false UNSAT
	// Clauses: (x1 ∨ x2), (¬x2 ∨ x3), (x1 ∨ x3)
	// x1: appears only positively -> pure
	// x2: appears both ways -> not pure
	// x3: appears only positively -> pure
	// Setting x1=true satisfies clauses 1 and 3
	// Clause 2 remains: (¬x2 ∨ x3)
	// x3 is still pure positive, setting x3=true satisfies clause 2
	// All clauses satisfied -> SAT
	clauses := []cnf.Clause{
		newClause(1, 2),  // x1 ∨ x2
		newClause(-2, 3), // ¬x2 ∨ x3
		newClause(1, 3),  // x1 ∨ x3
	}

	cnfFormula := &cnf.CNF{
		NumVars:    3,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(cnfFormula)
	s.verbose = true

	fmt.Println("=== Test: Pure literal soundness ===")
	fmt.Println("Before:")
	for i, c := range s.cnf.Clauses {
		fmt.Printf("  Clause %d: %v\n", i, c.Literals)
	}

	result := s.pureLiteralElimination()
	fmt.Printf("Result: %v\n", result)
	fmt.Println("After:")
	for i, c := range s.cnf.Clauses {
		fmt.Printf("  Clause %d: %v\n", i, c.Literals)
	}
	fmt.Printf("Assignments: x1=%v (level %d), x2=%v (level %d), x3=%v (level %d)\n",
		s.assignments[0].Value, s.assignments[0].Level,
		s.assignments[1].Value, s.assignments[1].Level,
		s.assignments[2].Value, s.assignments[2].Level)

	// Should be SAT (all clauses satisfied by pure literals)
	if result != SAT {
		t.Errorf("Expected SAT, got %v", result)
	}
}

func TestPureLiteralSoundness2(t *testing.T) {
	// Test: Pure literal with conflict detection
	// Clauses: (x1), (¬x1 ∨ x2), (¬x2)
	// x1: pure positive (only in clause 1)
	// x2: appears both ways (¬x2 in clause 3, x2 in clause 2)
	// Setting x1=true satisfies clause 1
	// Clause 2 becomes (x2) after removing ¬x1
	// Now x2 is pure positive (only in clause 2)
	// Setting x2=true satisfies clause 2
	// But clause 3 is (¬x2), which becomes empty -> UNSAT
	clauses := []cnf.Clause{
		newClause(1),     // x1
		newClause(-1, 2), // ¬x1 ∨ x2
		newClause(-2),    // ¬x2
	}

	cnfFormula := &cnf.CNF{
		NumVars:    2,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(cnfFormula)
	s.verbose = true

	fmt.Println("\n=== Test: Pure literal with conflict ===")
	fmt.Println("Before:")
	for i, c := range s.cnf.Clauses {
		fmt.Printf("  Clause %d: %v\n", i, c.Literals)
	}

	result := s.pureLiteralElimination()
	fmt.Printf("Result: %v\n", result)

	// This is actually SATISFIABLE: x1=true, x2=false
	// Clause 1: x1 = true ✓
	// Clause 2: ¬x1 ∨ x2 = false ∨ false = false ✗
	// Wait, that's wrong. Let me think again...
	// x1=true makes clause 2: ¬x1 ∨ x2 = false ∨ x2 = x2
	// So clause 2 becomes unit (x2)
	// x2 is NOT pure anymore (appears in clause 2 positively, clause 3 negatively)
	// So pure literal elimination should stop and return UNKNOWN

	if result == UNSAT {
		t.Errorf("Expected SAT or UNKNOWN, got UNSAT (false conflict)")
	}
}


func TestPureLiteralRealBug(t *testing.T) {
	// This is the actual bug: pure literal elimination on real instances
	// was causing soundness issues. Let's test a simple case that should work.

	// Instance: (x1 ∨ x2), (x1 ∨ ¬x2), (¬x1 ∨ x3), (¬x1 ∨ ¬x3)
	// This is UNSAT (it's actually a contradiction)
	// x1: both polarities
	// x2: both polarities
	// x3: both polarities
	// No pure literals, should return UNKNOWN
	clauses := []cnf.Clause{
		newClause(1, 2),   // x1 ∨ x2
		newClause(1, -2),  // x1 ∨ ¬x2
		newClause(-1, 3),  // ¬x1 ∨ x3
		newClause(-1, -3), // ¬x1 ∨ ¬x3
	}

	cnfFormula := &cnf.CNF{
		NumVars:    3,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(cnfFormula)
	s.verbose = true

	fmt.Println("\n=== Test: No pure literals (UNSAT instance) ===")
	result := s.pureLiteralElimination()
	fmt.Printf("Result: %v\n", result)
	fmt.Printf("Remaining clauses: %d\n", len(s.cnf.Clauses))

	// Should return UNKNOWN (no pure literals found)
	if result != UNKNOWN {
		t.Errorf("Expected UNKNOWN (no pure literals), got %v", result)
	}

	// No variables should be assigned
	for i := uint32(0); i < 3; i++ {
		if s.assignments[i].Level >= 0 {
			t.Errorf("Var %d should not be assigned, got level %d", i+1, s.assignments[i].Level)
		}
	}
}

func TestPureLiteralSoundnessCritical(t *testing.T) {
	// Critical soundness test: Pure literal should not cause false UNSAT
	// This tests the actual bug that was reported

	// Create a SAT instance where pure literal elimination might cause issues
	// Clauses: (x1 ∨ x2), (x1 ∨ x3), (x2 ∨ x3 ∨ x4), (x4)
	// x1: pure positive
	// x2: pure positive (after x1 assigned)
	// x3: pure positive (after x1 assigned)
	// x4: pure positive
	// All pure, should be SAT
	clauses := []cnf.Clause{
		newClause(1, 2),    // x1 ∨ x2
		newClause(1, 3),    // x1 ∨ x3
		newClause(2, 3, 4), // x2 ∨ x3 ∨ x4
		newClause(4),       // x4
	}

	cnfFormula := &cnf.CNF{
		NumVars:    4,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(cnfFormula)
	result := s.Solve()

	if !result {
		t.Errorf("Expected SAT, got UNSAT (false negative)")
	}

	// Verify the model
	assignments := s.GetAssignments()
	for i, c := range clauses {
		satisfied := false
		for _, lit := range c.Literals {
			varIdx := lit.Var()
			litTrue := (!lit.IsNegated() && assignments[varIdx].Value) || (lit.IsNegated() && !assignments[varIdx].Value)
			if litTrue {
				satisfied = true
				break
			}
		}
		if !satisfied {
			t.Errorf("Clause %d not satisfied: %v", i, c.Literals)
		}
	}
}

func TestPureLiteralWithPreprocessing(t *testing.T) {
	// Test pure literal elimination through full preprocessing pipeline
	// This is how it's actually used

	clauses := []cnf.Clause{
		newClause(1, 2),  // x1 ∨ x2
		newClause(-1, 3), // ¬x1 ∨ x3
	}

	cnfFormula := &cnf.CNF{
		NumVars:    3,
		Clauses:    clauses,
		NumClauses: len(clauses),
	}

	s := NewCDCLSolver(cnfFormula)
	result := s.SolveWithPreprocessing()

	if result != SAT {
		t.Errorf("Expected SAT through preprocessing, got %v", result)
	}
}
