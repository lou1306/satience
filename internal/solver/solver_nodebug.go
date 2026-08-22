//go:build !debug
// +build !debug

package solver

import "satience/internal/cnf"

// verifyClauseIndices is a no-op in non-debug builds
// Enable with: go build -tags debug
func verifyClauseIndices(s *CDCLSolver) bool {
	return true
}

// verifyOriginalWatchNotBothFalse is a no-op in non-debug builds
// (see solver_debug.go for the real check).
func verifyOriginalWatchNotBothFalse(s *CDCLSolver, literals []cnf.Literal, watch0, watch1 int) {}
