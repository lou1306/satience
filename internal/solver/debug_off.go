//go:build !debug
// +build !debug

package solver

import "satience/internal/cnf"

// Debug constants - disabled in release builds
const (
	DebugEnabled = false
)

// DebugLog is a no-op in release builds
func DebugLog(format string, args ...interface{}) {
	// No-op in release builds - compiler eliminates dead code
}

// VerboseLog prints verbose solver messages (no-op in release builds)
func (s *CDCLSolver) VerboseLog(format string, args ...interface{}) {
	// No-op in release builds
}

// VerbosePrintf prints formatted verbose output (no-op in release builds)
func VerbosePrintf(format string, args ...interface{}) {
	// No-op in release builds
}

// DebugClauseLog logs learned clause information (no-op in release builds)
func (s *CDCLSolver) DebugClauseLog(clauseIdx int, lbd int, literals []cnf.Literal) {
	// No-op in release builds
}

// DebugConflictLog logs conflict information (no-op in release builds)
func (s *CDCLSolver) DebugConflictLog(conflict, level, learned, decisions, propagations int, propsPerDec float64) {
	// No-op in release builds
}

// DebugPropagateLog logs propagation information (no-op in release builds)
func (s *CDCLSolver) DebugPropagateLog(start, end, count int) {
	// No-op in release builds
}

// Debug1UIPLog logs 1-UIP conflict analysis (no-op in release builds)
func (s *CDCLSolver) Debug1UIPLog(conflict, literals, atLevel, level int) {
	// No-op in release builds
}

// Debug1UIPErrorLog logs 1-UIP errors (no-op in release builds)
func (s *CDCLSolver) Debug1UIPErrorLog(conflict, literals, atLevel int, learnedLits []cnf.Literal) {
	// No-op in release builds
}
