package solver

import "fmt"

// LoggerFunc is a function type for logging
type LoggerFunc func(format string, args ...interface{})

// logFunc is the current logging function, set at initialization
// When verbose is disabled, this points to nopLogger which is compiled out
var logFunc LoggerFunc = nopLogger

// logEnabled tracks whether verbose logging is enabled
var logEnabled bool = false

// InitLogger sets up the logging function based on verbose flag
// Call this once at program startup
func InitLogger(verbose bool) {
	if verbose {
		logFunc = verboseLogger
		logEnabled = true
	} else {
		logFunc = nopLogger
		logEnabled = false
	}
}

// verboseLogger prints formatted output (used when -verbose flag is set)
func verboseLogger(format string, args ...interface{}) {
	fmt.Printf(format, args...)
}

// nopLogger is a no-op function that the compiler can inline and eliminate
// Used when verbose logging is disabled
func nopLogger(format string, args ...interface{}) {
	// No-op: compiler will inline and eliminate calls to this
}

// Log provides access to the current logger for solver methods
func (s *CDCLSolver) Log(format string, args ...interface{}) {
	logFunc(format, args...)
}

// LogEnabled returns true if verbose logging is enabled
func (s *CDCLSolver) LogEnabled() bool {
	return logEnabled
}
