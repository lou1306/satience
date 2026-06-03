package solver

import "satience/internal/cnf"

// VSIDS implements the VSIDS (Variable State Independent Decaying Sum) heuristic
type VSIDS struct {
	activity      []float64 // Activity score for each variable
	decayFactor   float64   // Decay factor (typically 0.95)
	inverseDecay  float64   // 1/decay for efficiency
}

// NewVSIDS creates a new VSIDS heuristic
func NewVSIDS(numVars uint32) *VSIDS {
	return &VSIDS{
		activity:     make([]float64, numVars),
		decayFactor:  0.95,
		inverseDecay: 1.0 / 0.95,
	}
}

// bump increases the activity of a variable
func (v *VSIDS) bump(varIdx uint32) {
	v.activity[varIdx] += 1.0
}

// bumpClause increases activity for all variables in a clause
func (v *VSIDS) bumpClause(literals []cnf.Literal) {
	for _, lit := range literals {
		v.bump(lit.Var())
	}
}

// decay decays all activity scores
func (v *VSIDS) decay() {
	for i := range v.activity {
		v.activity[i] *= v.decayFactor
	}
}

// selectVariable returns the unassigned variable with highest activity
func (v *VSIDS) selectVariable(assignments []Assignment) uint32 {
	bestVar := uint32(0)
	bestActivity := -1.0

	for i, act := range v.activity {
		if assignments[i].Level == 0 && act > bestActivity {
			bestActivity = act
			bestVar = uint32(i)
		}
	}

	return bestVar
}

// hasUnassigned checks if there are unassigned variables
func (v *VSIDS) hasUnassigned(assignments []Assignment, numVars uint32) bool {
	for i := uint32(0); i < numVars; i++ {
		if assignments[i].Level == 0 {
			return true
		}
	}
	return false
}
