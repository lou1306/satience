package solver

import (
	"fmt"
	"satience/internal/cnf"
)

// VerifyWatchInvariants checks that watched literals are maintained correctly
func (s *CDCLSolver) VerifyWatchInvariants(reason string) error {
	if !s.watchInitialized {
		return nil
	}

	clauseWatchCount := make(map[*cnf.Clause]int)

	for litIdx := 0; litIdx < len(s.watchLists); litIdx++ {
		watches := s.watchLists[litIdx]
		for i, watch := range watches {
			// Skip nil watches (deleted clauses)
			if watch.Clause == nil {
				continue
			}

			clause := watch.Clause
			blitIdx := int(watch.Blit)

			if blitIdx < 0 || blitIdx >= len(s.watchLists) {
				return fmt.Errorf("watch %d at litIdx %d has invalid blitIdx %d (%s)", i, litIdx, blitIdx, reason)
			}

			clauseWatchCount[clause]++

			if len(clause.Literals) < 2 {
				return fmt.Errorf("clause has %d literals (need at least 2 for watches) (%s)", len(clause.Literals), reason)
			}

			found := false
			for _, lit := range clause.Literals {
				idx := cnf.LitToIndex(lit)
				if idx == litIdx {
					found = true
					break
				}
			}

			if !found {
				return fmt.Errorf("watch at litIdx %d doesn't match any literal in clause (lits=%v) (%s)", litIdx, clause.Literals, reason)
			}

			found = false
			for _, lit := range clause.Literals {
				idx := cnf.LitToIndex(lit)
				if idx == blitIdx {
					found = true
					break
				}
			}

			if !found {
				return fmt.Errorf("blitIdx %d doesn't match any literal in clause (lits=%v) (%s)", blitIdx, clause.Literals, reason)
			}

			if litIdx == blitIdx {
				return fmt.Errorf("watch at litIdx %d has same blitIdx (clause) (%s)", litIdx, reason)
			}
		}
	}

	for clause, count := range clauseWatchCount {
		if count != 2 {
			return fmt.Errorf("clause %p has %d watches instead of 2 (%s)", clause, count, reason)
		}
	}

	return nil
}

// VerificationConfig controls which verification checks are enabled
type VerificationConfig struct {
	EnableTrailChecks         bool
	Enable1UIPChecks          bool
	EnableLearnedClauseChecks bool
	EnableLBDChecks           bool
	EnablePhaseChecks         bool
	Verbose                   bool
}

// DefaultVerificationConfig returns a config with all checks enabled
func DefaultVerificationConfig() VerificationConfig {
	return VerificationConfig{
		EnableTrailChecks:         true,
		Enable1UIPChecks:          true,
		EnableLearnedClauseChecks: true,
		EnableLBDChecks:           true,
		EnablePhaseChecks:         true,
		Verbose:                   true,
	}
}

// VerifyTrail checks trail invariants
func (s *CDCLSolver) VerifyTrail(reason string) error {
	seen := make([]bool, s.cnf.NumVars)
	for _, varIdx := range s.trail {
		if seen[varIdx] {
			return fmt.Errorf("duplicate variable %d in trail (%s)", varIdx, reason)
		}
		seen[varIdx] = true
	}

	prevLevel := 0
	for _, varIdx := range s.trail {
		level := s.assignments[varIdx].Level
		if level < prevLevel {
			return fmt.Errorf("trail level decreased: var %d at level %d, prev was %d (%s)",
				varIdx, level, prevLevel, reason)
		}
		prevLevel = level
	}

	return nil
}

// Verify1UIP checks that a learned clause satisfies the 1-UIP property
func (s *CDCLSolver) Verify1UIP(learnedClause cnf.Clause, conflictLevel int, reason string) error {
	if len(learnedClause.Literals) == 0 {
		return fmt.Errorf("learned clause is empty (%s)", reason)
	}

	literalsAtCurrentLevel := 0
	for _, lit := range learnedClause.Literals {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level == conflictLevel {
			literalsAtCurrentLevel++
		}
	}

	if literalsAtCurrentLevel != 1 {
		return fmt.Errorf("1-UIP violation: learned clause has %d literals at level %d, expected 1 (%s)",
			literalsAtCurrentLevel, conflictLevel, reason)
	}

	return nil
}

// VerifyLBD independently calculates LBD
func (s *CDCLSolver) VerifyLBD(clause cnf.Clause, expectedLBD int, reason string) error {
	if len(clause.Literals) == 0 {
		return fmt.Errorf("cannot calculate LBD of empty clause (%s)", reason)
	}

	levels := make(map[int]bool)
	for _, lit := range clause.Literals {
		varIdx := lit.Var()
		level := s.assignments[varIdx].Level
		levels[level] = true
	}

	calculatedLBD := len(levels)
	if calculatedLBD != expectedLBD {
		return fmt.Errorf("LBD mismatch: calculated %d, stored %d (%s)",
			calculatedLBD, expectedLBD, reason)
	}

	return nil
}

// VerifyNoContradictions checks that no clause is falsified
func (s *CDCLSolver) VerifyNoContradictions(reason string) error {
	for _, clause := range s.cnf.Clauses {
		allFalse := true
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			if s.assignments[varIdx].Level == 0 {
				allFalse = false
				break
			}
			litIsTrue := (s.assignments[varIdx].Value != lit.IsNegated())
			if litIsTrue {
				allFalse = false
				break
			}
		}
		if allFalse {
			return fmt.Errorf("clause is falsified by current assignments (%s)", reason)
		}
	}

	for i := range s.learnedOffsets {
		if s.learnedSizes[i] == 0 {
			continue
		}
		clause := cnf.Clause{Literals: s.getLearnedClauseLiterals(i), Learned: true}
		allFalse := true
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			if s.assignments[varIdx].Level == 0 {
				allFalse = false
				break
			}
			litIsTrue := (s.assignments[varIdx].Value != lit.IsNegated())
			if litIsTrue {
				allFalse = false
				break
			}
		}
		if allFalse {
			return fmt.Errorf("learned clause is falsified by current assignments (%s)", reason)
		}
	}

	return nil
}

// VerifyModel checks that a complete assignment satisfies all clauses
func (s *CDCLSolver) VerifyModel(reason string) error {
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		if s.assignments[varIdx].Level == 0 {
			return fmt.Errorf("variable %d is unassigned in model (%s)", varIdx, reason)
		}
	}

	for _, clause := range s.cnf.Clauses {
		satisfied := false
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			litIsTrue := (s.assignments[varIdx].Value != lit.IsNegated())
			if litIsTrue {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return fmt.Errorf("clause is not satisfied by model (%s)", reason)
		}
	}

	for i := range s.learnedOffsets {
		if s.learnedSizes[i] == 0 {
			continue
		}
		clause := cnf.Clause{Literals: s.getLearnedClauseLiterals(i), Learned: true}
		satisfied := false
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			litIsTrue := (s.assignments[varIdx].Value != lit.IsNegated())
			if litIsTrue {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return fmt.Errorf("learned clause is not satisfied by model (%s)", reason)
		}
	}

	return nil
}

// RunAllVerifications runs all verification checks
func (s *CDCLSolver) RunAllVerifications(config VerificationConfig, context string) error {
	if config.EnableTrailChecks {
		if err := s.VerifyTrail(context); err != nil {
			return err
		}
	}

	if err := s.VerifyNoContradictions(context); err != nil {
		return err
	}

	return nil
}

// VerifySolution verifies a SAT solution
func VerifySolution(cnfFormula *cnf.CNF, assignments []Assignment, verbose bool) error {
	for varIdx := uint32(0); varIdx < cnfFormula.NumVars; varIdx++ {
		if assignments[varIdx].Level == 0 {
			return fmt.Errorf("variable %d is unassigned", varIdx)
		}
	}

	for clauseID, clause := range cnfFormula.Clauses {
		satisfied := false
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			litIsTrue := (assignments[varIdx].Value != lit.IsNegated())
			if litIsTrue {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return fmt.Errorf("clause %d is not satisfied", clauseID)
		}
	}

	if verbose {
		fmt.Printf("c [verify] All %d clauses satisfied\n", cnfFormula.NumClauses)
	}

	return nil
}
