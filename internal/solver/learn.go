package solver

import "satience/internal/cnf"

// ConflictAnalyzer performs conflict analysis to derive learned clauses
type ConflictAnalyzer struct {
	cnf         *cnf.CNF
	assignments []Assignment
	implication []int // implication[var] = clause index that caused the implication
	decisionLevel []int // decisionLevel[var] = level at which var was decided/propagated
}

// NewConflictAnalyzer creates a new conflict analyzer
func NewConflictAnalyzer(cnf *cnf.CNF, assignments []Assignment) *ConflictAnalyzer {
	return &ConflictAnalyzer{
		cnf:           cnf,
		assignments:   assignments,
		implication:   make([]int, cnf.NumVars),
		decisionLevel: make([]int, cnf.NumVars),
	}
}

// SetImplication records that a variable was implied by a clause
func (ca *ConflictAnalyzer) SetImplication(varIdx uint32, clauseIdx int, level int) {
	ca.implication[varIdx] = clauseIdx
	ca.decisionLevel[varIdx] = level
}

// Analyze performs conflict analysis using the 1-UIP (First Unique Implication Point) scheme
// Returns the learned clause and the backtrack level
func (ca *ConflictAnalyzer) Analyze(conflictClause *cnf.Clause, trail []int, trailHead []int, level int, implication []int) (*cnf.Clause, int) {
	// Start with the conflict clause
	learned := make([]cnf.Literal, 0)
	
	// Track literals in the current conflict
	inConflict := make([]bool, ca.cnf.NumVars)
	for _, lit := range conflictClause.Literals {
		inConflict[lit.Var()] = true
	}

	// Count literals at current decision level
	literalsAtLevel := 0
	for _, lit := range conflictClause.Literals {
		if ca.assignments[lit.Var()].Level == level {
			literalsAtLevel++
		}
	}

	// Resolve backwards through the trail
	for i := len(trail) - 1; i >= 0; i-- {
		varIdx := uint32(trail[i])
		assign := ca.assignments[varIdx]

		if !inConflict[varIdx] {
			continue
		}

		// Remove from conflict set
		inConflict[varIdx] = false

		if assign.Level == level {
			// Check if there are any other variables at this level still in conflict
			hasOtherAtLevel := false
			for v, inConf := range inConflict {
				if inConf && ca.assignments[v].Level == level {
					hasOtherAtLevel = true
					break
				}
			}
			
			if literalsAtLevel > 1 && hasOtherAtLevel {
				// Not the UIP yet - resolve with implication clause
				implClause := implication[varIdx]
				if implClause >= 0 && implClause < len(ca.cnf.Clauses) {
					clause := &ca.cnf.Clauses[implClause]
					for _, lit := range clause.Literals {
						if !inConflict[lit.Var()] && ca.assignments[lit.Var()].Level > 0 {
							inConflict[lit.Var()] = true
							if ca.assignments[lit.Var()].Level == level {
								literalsAtLevel++
							}
						}
					}
				}
				// Add negation of this literal to learned clause
				negatedLit := cnf.NewLiteral(varIdx, assign.Value)
				learned = append(learned, negatedLit)
				literalsAtLevel--
			} else {
				// This is the UIP
				negatedLit := cnf.NewLiteral(varIdx, assign.Value)
				learned = append(learned, negatedLit)
				
				// Calculate backtrack level
				backtrackLevel := 0
				for _, lit := range learned {
					litLevel := ca.assignments[lit.Var()].Level
					if litLevel > backtrackLevel && litLevel < level {
						backtrackLevel = litLevel
					}
				}
				return &cnf.Clause{Literals: learned, Learned: true}, backtrackLevel
			}
		} else {
			// Literal from lower level - add to learned clause
			negatedLit := cnf.NewLiteral(varIdx, assign.Value)
			learned = append(learned, negatedLit)
		}
	}

	// Fallback: empty clause (shouldn't happen in normal operation)
	return &cnf.Clause{Literals: learned, Learned: true}, 0
}
