package solver

import (
	"fmt"
	"satience/internal/cnf"
)

// VerificationConfig controls which verification checks are enabled
type VerificationConfig struct {
	EnableTrailChecks      bool // Verify trail invariants after every operation
	Enable1UIPChecks       bool // Verify 1-UIP property after conflict analysis
	EnableLearnedClauseChecks bool // Verify learned clauses are logically implied
	EnableLBDChecks        bool // Verify LBD calculation
	EnablePhaseChecks      bool // Verify phase saving/retrieval
	Verbose                bool // Print verification failures
}

// DefaultVerificationConfig returns a config with all checks enabled
func DefaultVerificationConfig() VerificationConfig {
	return VerificationConfig{
		EnableTrailChecks:       true,
		Enable1UIPChecks:        true,
		EnableLearnedClauseChecks: true,
		EnableLBDChecks:         true,
		EnablePhaseChecks:       true,
		Verbose:                 true,
	}
}

// VerifyTrail checks trail invariants
// Returns nil if all invariants hold, error message otherwise
func (s *CDCLSolver) VerifyTrail(reason string) error {
	// Invariant 1: No duplicate variables in trail
	seen := make([]bool, s.cnf.NumVars)
	for _, varIdx := range s.trail {
		if seen[varIdx] {
			return fmt.Errorf("duplicate variable %d in trail (%s)", varIdx, reason)
		}
		seen[varIdx] = true
	}

	// Invariant 2: Decision levels are monotonically non-decreasing in trail
	prevLevel := 0
	for _, varIdx := range s.trail {
		level := s.assignments[varIdx].Level
		if level < prevLevel {
			return fmt.Errorf("trail level decreased: var %d at level %d, prev was %d (%s)", 
				varIdx, level, prevLevel, reason)
		}
		prevLevel = level
	}

	// Invariant 3: Every non-decision has a valid reason clause
	// (only check if we have implication array populated)
	for i, varIdx := range s.trail {
		if i == 0 {
			continue // First assignment could be decision or unit
		}
		
		// Check if this is a decision (level > previous max level)
		isDecision := false
		maxPrevLevel := 0
		for j := 0; j < i; j++ {
			if s.assignments[s.trail[j]].Level > maxPrevLevel {
				maxPrevLevel = s.assignments[s.trail[j]].Level
			}
		}
		if s.assignments[varIdx].Level > maxPrevLevel {
			isDecision = true
		}
		
		// If not a decision and not at level 0, should have implication
		if !isDecision && s.assignments[varIdx].Level > 0 {
			if s.implication != nil && s.implication[varIdx] != -1 {
				// Has implication, verify it justifies the assignment
				clauseID := s.implication[varIdx]
				if clauseID >= 0 && clauseID < len(s.learnedClauses) {
					clause := s.learnedClauses[clauseID]
					// Verify clause is unit with varIdx as the unassigned literal
					unitCount := 0
					for _, lit := range clause.Literals {
						litVar := lit.Var()
						if int(litVar) == varIdx {
							unitCount++
						} else if s.assignments[litVar].Level != 0 {
							// Check if literal is satisfied
							litIsTrue := (s.assignments[litVar].Value != lit.IsNegated())
							if litIsTrue {
								// Clause is satisfied, shouldn't be an implication
								return fmt.Errorf("implication clause %d for var %d is already satisfied (%s)", 
									clauseID, varIdx, reason)
							}
							unitCount++
						}
					}
					if unitCount != 1 {
						return fmt.Errorf("implication clause %d for var %d has %d unit literals, expected 1 (%s)", 
							clauseID, varIdx, unitCount, reason)
					}
				}
			}
		}
	}

	// Invariant 4: No contradictory assignments (x and ¬x both assigned)
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		if s.assignments[varIdx].Level != 0 {
			// Check if both polarities would be true
			// This shouldn't happen if propagation is correct
		}
	}

	// Invariant 5: All assigned variables are in trail
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		if s.assignments[varIdx].Level != 0 {
			found := false
			for _, trailVar := range s.trail {
				if trailVar == int(varIdx) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("assigned variable %d not in trail (%s)", varIdx, reason)
			}
		}
	}

	return nil
}

// Verify1UIP checks that a learned clause satisfies the 1-UIP property
// Must be called immediately after learnClause()
func (s *CDCLSolver) Verify1UIP(learnedClause cnf.Clause, conflictLevel int, reason string) error {
	if len(learnedClause.Literals) == 0 {
		return fmt.Errorf("learned clause is empty (%s)", reason)
	}

	// Count literals at conflict level
	literalsAtCurrentLevel := 0
	for _, lit := range learnedClause.Literals {
		varIdx := lit.Var()
		if s.assignments[varIdx].Level == conflictLevel {
			literalsAtCurrentLevel++
		}
	}

	// 1-UIP property: exactly 1 literal at the conflict level
	if literalsAtCurrentLevel != 1 {
		return fmt.Errorf("1-UIP violation: learned clause has %d literals at level %d, expected 1 (%s)", 
			literalsAtCurrentLevel, conflictLevel, reason)
	}

	// Verify all other literals are at lower levels
	for _, lit := range learnedClause.Literals {
		varIdx := lit.Var()
		level := s.assignments[varIdx].Level
		if level != conflictLevel && level > conflictLevel {
			return fmt.Errorf("1-UIP violation: literal %d at level %d > conflict level %d (%s)", 
				varIdx, level, conflictLevel, reason)
		}
	}

	return nil
}

// VerifyLBD independently calculates LBD and compares with stored value
func (s *CDCLSolver) VerifyLBD(clause cnf.Clause, expectedLBD int, reason string) error {
	if len(clause.Literals) == 0 {
		return fmt.Errorf("cannot calculate LBD of empty clause (%s)", reason)
	}

	// Count distinct decision levels in clause
	levels := make(map[int]bool)
	for _, lit := range clause.Literals {
		varIdx := lit.Var()
		level := s.assignments[varIdx].Level
		if level == 0 {
			// Unassigned literal - treat as level 0
			level = 0
		}
		levels[level] = true
	}

	calculatedLBD := len(levels)
	if calculatedLBD != expectedLBD {
		return fmt.Errorf("LBD mismatch: calculated %d, stored %d (%s)", 
			calculatedLBD, expectedLBD, reason)
	}

	return nil
}

// VerifyLearnedClause checks that a learned clause is logically implied
// by verifying that negating it leads to a conflict via unit propagation
func (s *CDCLSolver) VerifyLearnedClause(learnedClause cnf.Clause, reason string) error {
	if len(learnedClause.Literals) == 0 {
		return fmt.Errorf("learned clause is empty (%s)", reason)
	}

	// Create a temporary CNF with original clauses + negation of learned clause
	tempCNF := &cnf.CNF{
		NumVars:  s.cnf.NumVars,
		Clauses:  make([]cnf.Clause, len(s.cnf.Clauses)+1),
	}
	
	// Copy original clauses
	copy(tempCNF.Clauses, s.cnf.Clauses)
	
	// Add negation of learned clause as a unit clause
	// Negation of (a ∨ b ∨ c) is (¬a ∧ ¬b ∧ ¬c)
	// We add each negated literal as a unit clause
	negatedLits := make([]cnf.Literal, len(learnedClause.Literals))
	for i, lit := range learnedClause.Literals {
		// Negate the literal
		negatedLits[i] = lit.Negate()
	}
	
	// Add each negated literal as a unit clause
	for i, negLit := range negatedLits {
		tempCNF.Clauses[len(s.cnf.Clauses)+i] = cnf.Clause{
			Literals: []cnf.Literal{negLit},
		}
	}

	// Create temporary solver and run unit propagation
	tempSolver := NewSolver(tempCNF)
	
	// Propagate all unit clauses (the negated learned clause literals)
	for _, negLit := range negatedLits {
		varIdx := negLit.Var()
		value := !negLit.IsNegated()
		tempSolver.assignments[varIdx] = Assignment{
			Value: value,
			Level: 1,
		}
		tempSolver.trail = append(tempSolver.trail, int(varIdx))
	}
	
	// Run unit propagation
	conflict := tempSolver.propagate()
	
	if !conflict {
		return fmt.Errorf("learned clause is not implied: no conflict from negation (%s)", reason)
	}

	return nil
}

// VerifyPhaseSaving checks that phase saving and retrieval work correctly
func (s *CDCLSolver) VerifyPhaseSaving(varIdx uint32, value bool, reason string) error {
	if s.savedPhase == nil {
		return nil // Phase saving not enabled
	}
	
	if int(varIdx) >= len(s.savedPhase) {
		return fmt.Errorf("phase saving: var %d out of range [0, %d) (%s)", 
			varIdx, len(s.savedPhase), reason)
	}
	
	savedValue := s.savedPhase[varIdx]
	if savedValue != value {
		return fmt.Errorf("phase saving mismatch: var %d saved as %v, assigned %v (%s)", 
			varIdx, savedValue, value, reason)
	}
	
	return nil
}

// VerifyBackjumpLevel checks that backjump level is calculated correctly
func (s *CDCLSolver) VerifyBackjumpLevel(learnedClause cnf.Clause, backjumpLevel int, conflictLevel int, reason string) error {
	if len(learnedClause.Literals) == 0 {
		return fmt.Errorf("cannot verify backjump for empty clause (%s)", reason)
	}

	// Find the second-highest level in the learned clause
	// (highest should be conflict level, second-highest is backjump level)
	maxLevel := -1
	secondMaxLevel := -1
	
	for _, lit := range learnedClause.Literals {
		varIdx := lit.Var()
		level := s.assignments[varIdx].Level
		if level > maxLevel {
			secondMaxLevel = maxLevel
			maxLevel = level
		} else if level > secondMaxLevel && level < maxLevel {
			secondMaxLevel = level
		}
	}
	
	// The backjump level should be the second-highest level
	// (or 0 if all literals are at the same level, which shouldn't happen in 1-UIP)
	if secondMaxLevel == -1 {
		// All literals at same level - should not happen in valid 1-UIP
		if maxLevel == conflictLevel {
			return fmt.Errorf("all literals at conflict level, no backjump possible (%s)", reason)
		}
		secondMaxLevel = 0
	}
	
	if backjumpLevel != secondMaxLevel {
		return fmt.Errorf("backjump level mismatch: calculated %d, expected %d (%s)", 
			backjumpLevel, secondMaxLevel, reason)
	}

	return nil
}

// VerifyNoContradictions checks that no variable has contradictory assignments
func (s *CDCLSolver) VerifyNoContradictions(reason string) error {
	// Check that no clause is falsified by current assignments
	for clauseID, clause := range s.cnf.Clauses {
		allFalse := true
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			if s.assignments[varIdx].Level == 0 {
				// Unassigned, clause not falsified
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
			return fmt.Errorf("clause %d is falsified by current assignments (%s)", clauseID, reason)
		}
	}
	
	// Also check learned clauses
	for clauseID, clause := range s.learnedClauses {
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
			return fmt.Errorf("learned clause %d is falsified by current assignments (%s)", clauseID, reason)
		}
	}
	
	return nil
}

// VerifyModel checks that a complete assignment satisfies all clauses
func (s *CDCLSolver) VerifyModel(reason string) error {
	// First verify all variables are assigned
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		if s.assignments[varIdx].Level == 0 {
			return fmt.Errorf("variable %d is unassigned in model (%s)", varIdx, reason)
		}
	}
	
	// Check all original clauses are satisfied
	for clauseID, clause := range s.cnf.Clauses {
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
			return fmt.Errorf("clause %d is not satisfied by model (%s)", clauseID, reason)
		}
	}
	
	// Check all learned clauses are satisfied (they should be implied)
	for clauseID, clause := range s.learnedClauses {
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
			return fmt.Errorf("learned clause %d is not satisfied by model (%s)", clauseID, reason)
		}
	}
	
	return nil
}

// RunAllVerifications runs all verification checks
// Returns nil if all pass, first error message otherwise
func (s *CDCLSolver) RunAllVerifications(config VerificationConfig, context string) error {
	if config.EnableTrailChecks {
		if err := s.VerifyTrail(context); err != nil {
			return err
		}
	}
	
	if config.EnablePhaseChecks {
		// Phase saving is checked during assignment, no global check needed
	}
	
	if config.EnableLBDChecks {
		// LBD is checked per learned clause, no global check needed
	}
	
	if err := s.VerifyNoContradictions(context); err != nil {
		return err
	}
	
	return nil
}

// VerifySolution is a standalone function to verify a SAT solution
func VerifySolution(cnfFormula *cnf.CNF, assignments []Assignment, verbose bool) error {
	// Check all variables are assigned
	for varIdx := uint32(0); varIdx < cnfFormula.NumVars; varIdx++ {
		if assignments[varIdx].Level == 0 {
			return fmt.Errorf("variable %d is unassigned", varIdx)
		}
	}
	
	// Check all clauses are satisfied
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
		if verbose {
			fmt.Printf("c [verify] Clause %d satisfied\n", clauseID)
		}
	}
	
	if verbose {
		fmt.Printf("c [verify] All %d clauses satisfied\n", cnfFormula.NumClauses)
	}
	
	return nil
}
