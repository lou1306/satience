//go:build !debug
// +build !debug

package solver

import "satience/internal/cnf"

// verifyAssertingInvariant is a no-op in non-debug builds.
// The real implementation lives in propagate_asserting_debug.go.
// Enable with: go build -tags debug
func verifyAssertingInvariant(s *CDCLSolver, learnedIdx int, literals []cnf.Literal) {}
