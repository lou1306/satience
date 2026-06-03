package parser

import (
	"strings"
	"testing"
)

func TestParseSimple(t *testing.T) {
	input := `p cnf 3 2
1 2 0
-1 3 0`

	cnf, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if cnf.NumVars != 3 {
		t.Errorf("Expected 3 vars, got %d", cnf.NumVars)
	}
	if cnf.NumClauses != 2 {
		t.Errorf("Expected 2 clauses, got %d", cnf.NumClauses)
	}
}

func TestParseComments(t *testing.T) {
	input := `c This is a comment
p cnf 1 1
c Another comment
1 0
c Final comment`

	cnf, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if cnf.NumVars != 1 {
		t.Errorf("Expected 1 var, got %d", cnf.NumVars)
	}
	if cnf.NumClauses != 1 {
		t.Errorf("Expected 1 clause, got %d", cnf.NumClauses)
	}
}

func TestParseNoHeader(t *testing.T) {
	input := `1 2 0`

	_, err := Parse(strings.NewReader(input))
	if err == nil {
		t.Error("Expected error for missing header")
	}
}

func TestParseEmptyClauses(t *testing.T) {
	input := `p cnf 3 0`

	cnf, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if cnf.NumClauses != 0 {
		t.Errorf("Expected 0 clauses, got %d", cnf.NumClauses)
	}
}
