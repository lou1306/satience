//go:build debug
// +build debug

package solver

import (
	"fmt"
	"satience/internal/cnf"
)

// Debug constants - enabled in debug builds
const (
	DebugEnabled = true
)

// DebugLog prints debug messages in debug builds
func DebugLog(format string, args ...interface{}) {
	fmt.Printf(format, args...)
}

// VerboseLog prints verbose solver messages in debug builds
func (s *CDCLSolver) VerboseLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// VerbosePrintf prints formatted verbose output in debug builds
func VerbosePrintf(format string, args ...interface{}) {
	fmt.Printf(format, args...)
}

// DebugClauseLog logs learned clause information in debug builds
func (s *CDCLSolver) DebugClauseLog(clauseIdx int, lbd int, literals []cnf.Literal) {
	if s.verbose && clauseIdx < 10 {
		fmt.Printf("c [LEARNED] Clause %d: LBD=%d, size=%d, lits=[", clauseIdx, lbd, len(literals))
		for i, lit := range literals {
			if i > 0 {
				fmt.Printf(" ")
			}
			if lit.IsNegated() {
				fmt.Printf("-%d", lit.Var()+1)
			} else {
				fmt.Printf("%d", lit.Var()+1)
			}
		}
		fmt.Printf("]\n")
	}
}

// DebugConflictLog logs conflict information in debug builds
func (s *CDCLSolver) DebugConflictLog(conflict, level, learned, decisions, propagations int, propsPerDec float64) {
	if s.verbose {
		fmt.Printf("c [verbose] Conflict %d, level %d, learned %d, decisions %d, propagations %d, props/dec %.1f\n",
			conflict, level, learned, decisions, propagations, propsPerDec)
	}
}

// DebugPropagateLog logs propagation information in debug builds
func (s *CDCLSolver) DebugPropagateLog(start, end, count int) {
	if s.verbose && count > 0 {
		fmt.Printf("c [PROPAGATE] Processed trail[%d:%d], found %d propagations\n", start, end, count)
	}
}

// Debug1UIPLog logs 1-UIP conflict analysis in debug builds
func (s *CDCLSolver) Debug1UIPLog(conflict, literals, atLevel, level int) {
	if s.verbose && conflict <= 100 {
		fmt.Printf("c [1-UIP] Conflict %d: %d literals, %d at level %d (target: 1)\n",
			conflict, literals, atLevel, level)
	}
}

// Debug1UIPErrorLog logs 1-UIP errors in debug builds
func (s *CDCLSolver) Debug1UIPErrorLog(conflict, literals, atLevel int, learnedLits []cnf.Literal) {
	if s.verbose {
		fmt.Printf("c [1-UIP ERROR] Conflict %d: Failed to find UIP! Literals: %d, at level %d\n",
			conflict, literals, atLevel)
		if conflict <= 100 {
			for _, lit := range learnedLits {
				v := lit.Var()
				sign := ""
				if lit.IsNegated() {
					sign = "¬"
				}
				fmt.Printf("c   Lit: %sx%d, Level: %d, Reason: %v\n",
					sign, v+1, s.assignments[v].Level, s.implication[v])
			}
		}
	}
}

// DebugPreprocessingLog logs preprocessing info in debug builds
func (s *CDCLSolver) DebugPreprocessingLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// DebugRestartLog logs restart info in debug builds
func (s *CDCLSolver) DebugRestartLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// DebugInprocessLog logs inprocessing info in debug builds
func (s *CDCLSolver) DebugInprocessLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// DebugEquivalenceLog logs equivalence detection in debug builds
func (s *CDCLSolver) DebugEquivalenceLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// DebugPureLiteralLog logs pure literal elimination in debug builds
func (s *CDCLSolver) DebugPureLiteralLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// DebugSubsumptionLog logs subsumption elimination in debug builds
func (s *CDCLSolver) DebugSubsumptionLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}

// DebugIterationLog logs iteration progress in debug builds
func (s *CDCLSolver) DebugIterationLog(format string, args ...interface{}) {
	if s.verbose {
		fmt.Printf(format, args...)
	}
}
