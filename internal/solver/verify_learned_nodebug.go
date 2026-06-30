//go:build !debug

package solver

import "satience/internal/cnf"

// VerifyLearnedClause is a no-op in non-debug builds
func (s *CDCLSolver) VerifyLearnedClause(literals []cnf.Literal, conflictNum int) bool {
	return true
}

// VerifyAllLearnedClauses is a no-op in non-debug builds
func (s *CDCLSolver) VerifyAllLearnedClauses() {
}
