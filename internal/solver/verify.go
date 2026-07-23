package solver

import (
	"fmt"
	"satience/internal/cnf"
)

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
