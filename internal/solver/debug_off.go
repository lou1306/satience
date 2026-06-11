//go:build !debug
// +build !debug

package solver

import "satience/internal/cnf"

// Debug constants - disabled in release builds
const (
	DebugEnabled = false
)

// DebugLog is a no-op in release builds - completely eliminated by compiler
func DebugLog(format string, args ...interface{}) {}

// VerboseLog prints verbose solver messages - no-op in release builds
func (s *CDCLSolver) VerboseLog(format string, args ...interface{}) {}

// VerbosePrintf prints formatted verbose output - no-op in release builds
func VerbosePrintf(format string, args ...interface{}) {}

// VerbosePrintln prints a line of verbose output - no-op in release builds
func VerbosePrintln(msg string) {}

// DebugClauseLog logs learned clause information - no-op in release builds
func (s *CDCLSolver) DebugClauseLog(clauseIdx int, lbd int, literals []cnf.Literal) {}

// DebugConflictLog logs conflict information - no-op in release builds
func (s *CDCLSolver) DebugConflictLog(conflict, level, learned, decisions, propagations int, propsPerDec float64) {}

// DebugPropagateLog logs propagation information - no-op in release builds
func (s *CDCLSolver) DebugPropagateLog(start, end, count int) {}

// Debug1UIPLog logs 1-UIP conflict analysis - no-op in release builds
func (s *CDCLSolver) Debug1UIPLog(conflict, literals, atLevel, level int) {}

// Debug1UIPErrorLog logs 1-UIP errors - no-op in release builds
func (s *CDCLSolver) Debug1UIPErrorLog(conflict, literals, atLevel int, learnedLits []cnf.Literal) {}

// DebugPreprocessingLog logs preprocessing info - no-op in release builds
func (s *CDCLSolver) DebugPreprocessingLog(format string, args ...interface{}) {}

// DebugRestartLog logs restart info - no-op in release builds
func (s *CDCLSolver) DebugRestartLog(format string, args ...interface{}) {}

// DebugInprocessLog logs inprocessing info - no-op in release builds
func (s *CDCLSolver) DebugInprocessLog(format string, args ...interface{}) {}

// DebugEquivalenceLog logs equivalence detection - no-op in release builds
func (s *CDCLSolver) DebugEquivalenceLog(format string, args ...interface{}) {}

// DebugPureLiteralLog logs pure literal elimination - no-op in release builds
func (s *CDCLSolver) DebugPureLiteralLog(format string, args ...interface{}) {}

// DebugSubsumptionLog logs subsumption elimination - no-op in release builds
func (s *CDCLSolver) DebugSubsumptionLog(format string, args ...interface{}) {}

// DebugIterationLog logs iteration progress - no-op in release builds
func (s *CDCLSolver) DebugIterationLog(format string, args ...interface{}) {}
