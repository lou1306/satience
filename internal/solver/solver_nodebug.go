//go:build !debug
// +build !debug

package solver

// verifyClauseIndices is a no-op in non-debug builds
// Enable with: go build -tags debug
func verifyClauseIndices(s *CDCLSolver) bool {
	return true
}
