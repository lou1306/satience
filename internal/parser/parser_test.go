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

func TestParseMultiLineClause(t *testing.T) {
	// Clause spanning multiple lines (terminated by 0 on second line)
	input := `p cnf 5 1
1 2 3
4 5 0`

	cnf, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if cnf.NumClauses != 1 {
		t.Errorf("Expected 1 clause, got %d", cnf.NumClauses)
	}
}

func TestParseUnterminatedClause(t *testing.T) {
	// File ending without 0 terminator should error
	input := `p cnf 1 2
1 0
-1`

	_, err := Parse(strings.NewReader(input))
	if err == nil {
		t.Error("Expected error for unterminated clause")
	}
}

func TestParseEmptyClause(t *testing.T) {
	// Empty clause (just 0) is valid DIMACS
	input := `p cnf 1 1
0`

	cnf, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if cnf.NumClauses != 1 {
		t.Errorf("Expected 1 clause, got %d", cnf.NumClauses)
	}
}

func TestParseOverflowLiteral(t *testing.T) {
	// Literals exceeding int32 must be rejected to prevent uint32 truncation
	// aliasing (e.g. 4294967297 would truncate to variable 1).
	cases := []string{
		"p cnf 5 1\n4294967297 0\n",  // 2^32 + 1
		"p cnf 5 1\n-4294967297 0\n", // -(2^32 + 1)
		"p cnf 5 1\n9999999999 0\n",  // > int32 max
		"p cnf 5 1\n2147483648 0\n",  // int32 max + 1
	}
	for _, input := range cases {
		_, err := Parse(strings.NewReader(input))
		if err == nil {
			t.Errorf("Expected error for overflow literal in:\n%s", input)
		}
	}
}

func TestParseClauseBeforeHeader(t *testing.T) {
	// Clause data appearing before the problem line is malformed DIMACS.
	// Previously silently dropped; now rejected.
	input := `1 2 0
p cnf 3 2
1 2 0
-1 3 0`

	_, err := Parse(strings.NewReader(input))
	if err == nil {
		t.Error("Expected error for clause data before problem line")
	}
}

func TestParseMaxInt32Literal(t *testing.T) {
	// int32 max is the largest accepted literal (2147483647).
	// With numVars large enough, this should parse cleanly.
	input := "p cnf 2147483647 1\n2147483647 0\n"

	cnf, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if cnf.NumClauses != 1 {
		t.Errorf("Expected 1 clause, got %d", cnf.NumClauses)
	}
}
