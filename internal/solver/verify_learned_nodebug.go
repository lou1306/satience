//go:build !debug
// +build !debug

package solver

import "satience/internal/cnf"

// verifyLearnedClause is a no-op in non-debug builds.
// The real implementation (3× O(clause_size) checks) lives in verify_learned_debug.go.
func (s *CDCLSolver) verifyLearnedClause(learnedLits []cnf.Literal, allowMultipleAtCurrentLevel bool) bool {
	return true
}
