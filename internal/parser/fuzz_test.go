package parser

import (
	"strings"
	"testing"
)

// FuzzParser feeds random bytes to the DIMACS parser and asserts no panic.
// The parser must reject malformed input with an error, never crash.
func FuzzParser(f *testing.F) {
	// Seed corpus: valid DIMACS, malformed input, edge cases.
	seeds := []string{
		// Valid
		"p cnf 3 2\n1 2 0\n-1 3 0\n",
		"p cnf 1 1\n0\n",
		"p cnf 0 0\n",
		"c comment\np cnf 3 1\n1 2 3 0\n",
		"p cnf 5 1\n1 2 3\n4 5 0\n",
		// Malformed
		"1 2 0\np cnf 3 2\n1 2 0\n-1 3 0\n",
		"p cnf 3 1\n4294967297 0\n",
		"p cnf 3 1\n9999999999999999 0\n",
		"p cnf 3 5 garbage\n1 2 0\n",
		"p cnf 3 1\n1 2 0\n3 4 0\n5 0\n",
		"p wcnf 3 1\n1 0\n",
		"p cnf 3 1\n1 0\n0\n",
		"p cnf 3 1\n+1 0\n",
		"p cnf 3 1\n-0\n",
		// Empty / garbage
		"",
		"\x00\x01\x02",
		"p cnf 3 1\n\xff\xfe 0\n",
		"p cnf 3 1\n1 2 3 4 5 6 7 8 9 10 0\n",
		"p cnf 100000 1\n50000 0\n",
		"p cnf 2147483647 1\n2147483647 0\n",
		"p cnf 2147483647 1\n2147483648 0\n",
		"p cnf 3 1\n-1 -2 -3 0\n",
		"p cnf 3 2\n1 -1 0\n2 -2 0\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data string) {
		// The parser must not panic on any input.
		cnf, err := Parse(strings.NewReader(data))
		if err != nil {
			return // Error is the correct response to malformed input
		}
		// If parsing succeeded, the result must be structurally valid.
		if cnf == nil {
			t.Fatal("Parse returned nil CNF without error")
		}
		// NumVars and NumClauses should be sane
		if int(cnf.NumVars) < 0 {
			t.Fatalf("negative NumVars: %d", cnf.NumVars)
		}
		if cnf.NumClauses < 0 {
			t.Fatalf("negative NumClauses: %d", cnf.NumClauses)
		}
		// Every literal in every clause must reference a valid variable
		for _, clause := range cnf.Clauses {
			for _, lit := range clause.Literals {
				if int(lit.Var()) >= int(cnf.NumVars) {
					t.Fatalf("literal var %d >= NumVars %d", lit.Var(), cnf.NumVars)
				}
			}
		}
	})
}
