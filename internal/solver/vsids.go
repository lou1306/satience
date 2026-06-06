package solver

import "satience/internal/cnf"

// VSIDS implements the VSIDS (Variable State Independent Decaying Sum) heuristic
// with optional LRB (Learning Rate Based) conflict participation tracking
type VSIDS struct {
	activity            []float64 // Activity score for each variable
	conflictParticipation []int   // Number of conflicts each variable participates in
	decayFactor         float64   // Decay factor (typically 0.95)
	inverseDecay        float64   // 1/decay for efficiency
	useLRB              bool      // Use LRB heuristic instead of pure VSIDS
	lrbDecayInterval    int       // Decay LRB scores every N conflicts
	conflictCount       int       // Total conflicts for LRB decay timing
}

// NewVSIDS creates a new VSIDS heuristic with clause-length weighted initialization
func NewVSIDS(numVars uint32) *VSIDS {
	return &VSIDS{
		activity:              make([]float64, numVars),
		conflictParticipation: make([]int, numVars),
		decayFactor:           0.95,
		inverseDecay:          1.0 / 0.95,
		useLRB:                false, // Default to VSIDS
		lrbDecayInterval:      1024,  // Decay every 1024 conflicts
		conflictCount:         0,
	}
}

// InitializeFromClauses initializes VSIDS activity based on clause participation
// Variables in shorter clauses get higher activity (more constrained = more important)
func (v *VSIDS) InitializeFromClauses(clauses []cnf.Clause) {
	for _, clause := range clauses {
		weight := 1.0 / float64(len(clause.Literals)) // Shorter clauses = higher weight
		for _, lit := range clause.Literals {
			v.activity[lit.Var()] += weight
		}
	}
}

// EnableLRB enables LRB (Learning Rate Based) heuristic
func (v *VSIDS) EnableLRB() {
	v.useLRB = true
}

// bump increases the activity of a variable
func (v *VSIDS) bump(varIdx uint32) {
	v.activity[varIdx] += 1.0
}

// bumpClause increases activity for all variables in a clause
// Also tracks conflict participation for LRB heuristic
func (v *VSIDS) bumpClause(literals []cnf.Literal) {
	for _, lit := range literals {
		v.bump(lit.Var())
		// Track conflict participation for LRB
		v.conflictParticipation[lit.Var()]++
	}
	v.conflictCount++
	
	// Periodic decay for LRB scores
	if v.useLRB && v.conflictCount%v.lrbDecayInterval == 0 {
		v.decayLRB()
	}
}

// decayLRB decays LRB conflict participation scores
func (v *VSIDS) decayLRB() {
	for i := range v.conflictParticipation {
		v.conflictParticipation[i] = v.conflictParticipation[i] / 2
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

// selectVariableWithPhase returns the unassigned variable with highest activity
// and the phase to assign (true=positive, false=negative) based on saved phase
// Uses LRB (conflict participation) if enabled, otherwise VSIDS (activity)
func (v *VSIDS) selectVariableWithPhase(assignments []Assignment, savedPhase []bool) (uint32, bool) {
	bestVar := uint32(0)
	bestScore := -1.0

	for i := range assignments {
		if assignments[i].Level != 0 {
			continue
		}
		
		// Use LRB score (conflict participation) or VSIDS activity
		var score float64
		if v.useLRB {
			score = float64(v.conflictParticipation[i])
		} else {
			score = v.activity[i]
		}
		
		if score > bestScore {
			bestScore = score
			bestVar = uint32(i)
		}
	}

	// Use saved phase if available, otherwise default to true (positive literal)
	phase := true
	if savedPhase != nil {
		phase = savedPhase[bestVar]
	}

	return bestVar, phase
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
