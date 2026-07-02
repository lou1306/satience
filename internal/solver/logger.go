package solver

import "fmt"

// LoggerFunc is a function type for logging
type LoggerFunc func(format string, args ...interface{})

// verboseLogger prints formatted output (used when -verbose flag is set)
func verboseLogger(format string, args ...interface{}) {
	fmt.Printf(format, args...)
}

// nopLogger is a no-op function that the compiler can inline and eliminate
func nopLogger(format string, args ...interface{}) {}

// Log provides access to the current logger for solver methods.
// Reads s.verbose to decide whether to format output, avoiding the
// interface{} boxing overhead when verbose is off.
func (s *CDCLSolver) Log(format string, args ...interface{}) {
	if s.verbose {
		verboseLogger(format, args...)
	}
}

// InitLogger is kept for backward compatibility but is now a no-op.
// Logging is controlled per-solver via SetVerbose.
func InitLogger(verbose bool) {
	// No-op: logging is per-instance via s.verbose
}
