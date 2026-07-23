package solver

import "fmt"

// verboseLogger prints formatted output (used when -verbose flag is set)
func verboseLogger(format string, args ...interface{}) {
	fmt.Printf(format, args...)
}

// Log provides access to the current logger for solver methods.
// Reads s.verbose to decide whether to format output, avoiding the
// interface{} boxing overhead when verbose is off.
func (s *CDCLSolver) Log(format string, args ...interface{}) {
	if s.verbose {
		verboseLogger(format, args...)
	}
}
