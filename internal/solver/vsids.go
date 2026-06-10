package solver

import "satience/internal/cnf"

// VSIDS implements the VSIDS (Variable State Independent Decaying Sum) heuristic
// with optional LRB (Learning Rate Based) conflict participation tracking
// and LBD-based activity (variables in low-LBD clauses get higher activity)
//
// Decay factor tuning:
// - Starts at 0.95 (aggressive decay to explore variables quickly)
// - Increases toward 0.999 over 10k conflicts (slower decay to focus on important vars)
// - This allows rapid initial exploration followed by focused search on critical variables
type VSIDS struct {
	activity              []float64 // Activity score for each variable
	conflictParticipation []int     // Number of conflicts each variable participates in
	decayFactor           float64   // Decay factor (0.95 -> 0.999)
	inverseDecay          float64   // 1/decay for efficiency
	useLRB                bool      // Use LRB heuristic instead of pure VSIDS
	lrbDecayInterval      int       // Decay every N conflicts
	conflictCount         int       // Total conflicts for LRB decay timing
	useLBD                bool      // Use LBD-based activity (variables in low-LBD clauses prioritized)
	lbdBonus              []float64 // Bonus score from appearing in low-LBD clauses
	maxDecayFactor        float64   // Maximum decay factor (0.999)
	decayIncrement        float64   // Increment per conflict
}

// NewVSIDS creates a new VSIDS heuristic with clause-length weighted initialization
func NewVSIDS(numVars uint32) *VSIDS {
	// Standard MiniSat/GLUCOSE decay parameters
	// Start with 0.95 decay (aggressive) and increase toward 0.999
	// But we'll use PERIODIC decay to prevent activity from vanishing
	maxDecay := 0.99
	initialDecay := 0.90
	return &VSIDS{
		activity:              make([]float64, numVars),
		conflictParticipation: make([]int, numVars),
		decayFactor:           initialDecay,
		inverseDecay:          1.0 / initialDecay,
		useLRB:                false, // Default to VSIDS
		lrbDecayInterval:      1024,  // Decay every 1024 conflicts
		conflictCount:         0,
		useLBD:                true,  // Enable LBD-based activity by default
		lbdBonus:              make([]float64, numVars),
		maxDecayFactor:        maxDecay,
		decayIncrement:        (maxDecay - initialDecay) / 5000.0,  // Faster increase
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

// EnableLBD enables LBD-based activity (variables in low-LBD clauses prioritized)
func (v *VSIDS) EnableLBD() {
	v.useLBD = true
}

// bumpLBD adds LBD bonus to variables in a learned clause
// Lower LBD = higher bonus (glue clauses are most important)
func (v *VSIDS) bumpLBD(literals []cnf.Literal, lbd int) {
	if !v.useLBD {
		return
	}
	
	// Bonus formula: MUCH higher bonus for lower LBD
	// Glue clauses (LBD<=3) are extremely important - give huge bonus
	// LBD=2: bonus = 1000 (core glue - most important)
	// LBD=3: bonus = 500 (glue - very important)
	// LBD=4: bonus = 250
	// LBD=5: bonus = 125
	// LBD=10: bonus = 50
	// This ensures glue clause variables dominate VSIDS selection
	bonus := 2000.0 / float64(lbd)
	
	for _, lit := range literals {
		v.lbdBonus[lit.Var()] += bonus
	}
}

// decayLBD decays LBD bonus scores (called every conflict)
// Use very slow decay to preserve glue clause importance
func (v *VSIDS) decayLBD() {
	if !v.useLBD {
		return
	}
	
	// Decay very slowly (only 1% per conflict) to preserve glue clause importance
	// Glue clauses should influence search for thousands of conflicts
	for i := range v.lbdBonus {
		v.lbdBonus[i] *= 0.99
	}
}

// bump increases the activity of a variable
func (v *VSIDS) bump(varIdx uint32) {
	v.activity[varIdx] += 1.0
}

// bumpLarge increases the activity of a variable by a larger amount
// Used for variables in conflict clauses to make them more likely to be chosen
func (v *VSIDS) bumpLarge(varIdx uint32, amount float64) {
	v.activity[varIdx] += amount
}

// bumpClause increases activity for all variables in a clause
// Also tracks conflict participation for LRB heuristic
// Bump amount is inversely proportional to clause size - smaller clauses = larger bump
func (v *VSIDS) bumpClause(literals []cnf.Literal) {
	// Scale bump by clause size: smaller clauses get larger bumps
	// Binary clauses: bump = 5.0
	// Ternary clauses: bump = 3.33
	// 10-literal clauses: bump = 1.0
	// This focuses search on variables in constrained clauses
	baseBump := 10.0
	bumpAmount := baseBump / float64(len(literals))
	if bumpAmount < 0.5 {
		bumpAmount = 0.5 // Minimum bump for very large clauses
	}
	
	for _, lit := range literals {
		v.bumpLarge(lit.Var(), bumpAmount)
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

// decay decays all activity scores every conflict (MiniSat-style)
// This creates strong differentiation between important and unimportant variables
// Decay factor starts at 0.95 and increases toward max for focused search
func (v *VSIDS) decay() {
	// Gradually increase decay factor toward max
	if v.decayFactor < v.maxDecayFactor {
		v.decayFactor += v.decayIncrement
		if v.decayFactor > v.maxDecayFactor {
			v.decayFactor = v.maxDecayFactor
		}
		v.inverseDecay = 1.0 / v.decayFactor
	}
	
	// Apply decay to all activity scores EVERY CONFLICT
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
// Can also use LBD-based bonus (variables in low-LBD clauses prioritized)
func (v *VSIDS) selectVariableWithPhase(assignments []Assignment, savedPhase []bool) (uint32, bool) {
	bestVar := uint32(0)
	bestScore := -1.0

	for i := range assignments {
		if assignments[i].Level != 0 {
			continue
		}
		
		// Calculate score based on enabled heuristics
		var score float64
		if v.useLRB {
			score = float64(v.conflictParticipation[i])
		} else if v.useLBD {
			// Combine base activity with LBD bonus
			score = v.activity[i] + v.lbdBonus[i]
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

// ActivityForDebug returns the activity score for a variable (for debugging)
func (v *VSIDS) ActivityForDebug(varIdx uint32) float64 {
	if int(varIdx) >= len(v.activity) {
		return 0
	}
	return v.activity[varIdx]
}

// ConflictParticipationForDebug returns the conflict participation count (for debugging)
func (v *VSIDS) ConflictParticipationForDebug(varIdx uint32) int {
	if int(varIdx) >= len(v.conflictParticipation) {
		return 0
	}
	return v.conflictParticipation[varIdx]
}

// resetActivity resets activity scores to prevent choosing the same variables repeatedly
// Called on restart. We scale down activity rather than zeroing it completely,
// which preserves some learned information while allowing exploration of new variables.
func (v *VSIDS) resetActivity() {
	// Scale down activity by 50% instead of zeroing - preserves important variables
	// but allows other variables to compete
	for i := range v.activity {
		v.activity[i] *= 0.5
	}
	
	// Reset LBD bonus completely - glue clause importance changes after restart
	for i := range v.lbdBonus {
		v.lbdBonus[i] = 0
	}
	
	// Keep conflict participation for LRB - it's valuable long-term information
	// Scale it down too
	for i := range v.conflictParticipation {
		v.conflictParticipation[i] = v.conflictParticipation[i] / 2
	}
	// Don't reset conflictCount - it's used for decay timing
}
