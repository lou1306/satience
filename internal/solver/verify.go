package solver

import (
	"fmt"
	"satience/internal/cnf"
)

// VerifyModel checks that a complete assignment satisfies all clauses
func (s *CDCLSolver) VerifyModel(reason string) error {
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		if s.assignments[varIdx].Level < 0 {
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

// VerifySolution verifies a SAT solution
func VerifySolution(cnfFormula *cnf.CNF, assignments []Assignment, verbose bool) error {
	for varIdx := uint32(0); varIdx < cnfFormula.NumVars; varIdx++ {
		if assignments[varIdx].Level < 0 {
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
