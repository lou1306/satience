package solver

import (
	"container/heap"
	"satience/internal/cnf"
)

// vsidsHeapItem represents a variable in the activity heap
type vsidsHeapItem struct {
	varIdx   uint32
	activity float64
	index    int // index in the heap
}

// vsidsHeap implements heap.Interface for activity-based variable selection
type vsidsHeap []vsidsHeapItem

func (h vsidsHeap) Len() int           { return len(h) }
func (h vsidsHeap) Less(i, j int) bool { return h[i].activity > h[j].activity } // Max-heap
func (h vsidsHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *vsidsHeap) Push(x interface{}) {
	n := len(*h)
	item := x.(vsidsHeapItem)
	item.index = n
	*h = append(*h, item)
}

func (h *vsidsHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = vsidsHeapItem{} // avoid memory leak
	item.index = -1
	*h = old[0 : n-1]
	return item
}

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
	heap                  vsidsHeap // Activity heap for O(log n) selection
	heapValid             bool      // True if heap is up-to-date
}

// NewVSIDS creates a new VSIDS heuristic with clause-length weighted initialization
func NewVSIDS(numVars uint32) *VSIDS {
	maxDecay := 0.999
	initialDecay := 0.95
	return &VSIDS{
		activity:              make([]float64, numVars),
		conflictParticipation: make([]int, numVars),
		decayFactor:           initialDecay,
		inverseDecay:          1.0 / initialDecay,
		useLRB:                false,
		lrbDecayInterval:      1024,
		conflictCount:         0,
		useLBD:                true,
		lbdBonus:              make([]float64, numVars),
		maxDecayFactor:        maxDecay,
		decayIncrement:        (maxDecay - initialDecay) / 10000.0,
		heap:                  make(vsidsHeap, 0, numVars),
		heapValid:             false,
	}
}

// InitializeFromClauses initializes VSIDS activity based on clause participation
// Variables in shorter clauses get MUCH higher activity (more constrained = more important)
// Binary clauses get 100x base weight to strongly bias initial variable selection
func (v *VSIDS) InitializeFromClauses(clauses []cnf.Clause) {
	for _, clause := range clauses {
		baseWeight := 10.0
		if len(clause.Literals) == 2 {
			baseWeight = 100.0
		}
		weight := baseWeight / float64(len(clause.Literals))

		for _, lit := range clause.Literals {
			v.activity[lit.Var()] += weight
		}
	}
	v.heapValid = false // Invalidate heap after modifying activities
}

// buildHeap rebuilds the activity heap from current activity scores
// Only includes unassigned variables
// Includes LBD bonus in activity score for selection
func (v *VSIDS) buildHeap(assignments []Assignment) {
	v.heap = make(vsidsHeap, 0, len(v.activity))

	for i, act := range v.activity {
		if assignments[i].Level == 0 {
			// Add LBD bonus to activity for selection
			effectiveActivity := act + v.lbdBonus[i]
			v.heap = append(v.heap, vsidsHeapItem{
				varIdx:   uint32(i),
				activity: effectiveActivity,
				index:    -1,
			})
		}
	}

	heap.Init(&v.heap)
	v.heapValid = true
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

	// Bonus formula: EXPONENTIAL bonus for lower LBD
	// Glue clauses (LBD<=3) are extremely important - give huge bonus
	// LBD=2: bonus = 10000 (core glue - most important)
	// LBD=3: bonus = 3333 (glue - very important)
	// LBD=4: bonus = 1250
	// LBD=5: bonus = 500
	// LBD=10: bonus = 100
	// This ensures glue clause variables dominate VSIDS selection
	bonus := 20000.0 / float64(lbd*lbd)

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

	// Decay extremely slowly (only 0.1% per conflict) to preserve glue clause importance
	// Glue clauses should influence search for tens of thousands of conflicts
	for i := range v.lbdBonus {
		v.lbdBonus[i] *= 0.999
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
	// Scale bump by clause size: AGGRESSIVE bumps for better differentiation
	// Binary clauses: bump = 200.0
	// Ternary clauses: bump = 133.3
	// 5-literal clauses: bump = 80.0
	// 10-literal clauses: bump = 20.0
	// This focuses search on variables in constrained clauses
	baseBump := 400.0
	bumpAmount := baseBump / float64(len(literals))
	if bumpAmount < 10.0 {
		bumpAmount = 10.0 // Minimum bump for very large clauses
	}

	for _, lit := range literals {
		// Extra boost for variables that appear in conflicts frequently
		// This creates positive feedback: conflicted vars get chosen more
		conflictBoost := 1.0 + float64(v.conflictParticipation[lit.Var()])*0.1
		v.bumpLarge(lit.Var(), bumpAmount*conflictBoost)
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

	// Invalidate heap since all activities changed
	// Heap will be rebuilt on next selectVariableWithHeap call
	v.heapValid = false
}

// selectVariableWithHeap returns the unassigned variable with highest activity using a heap
// This provides O(log n) selection instead of O(n) linear scan
func (v *VSIDS) selectVariableWithHeap(assignments []Assignment) uint32 {
	// Rebuild heap if invalid or empty
	if !v.heapValid || v.heap.Len() == 0 {
		v.buildHeap(assignments)
	}

	// Pop variables until we find an unassigned one
	for v.heap.Len() > 0 {
		item := heap.Pop(&v.heap).(vsidsHeapItem)
		varIdx := int(item.varIdx)

		// Skip if variable is now assigned (stale heap entry)
		if varIdx >= len(assignments) || assignments[varIdx].Level != 0 {
			continue
		}

		// Push it back with updated activity (including LBD bonus)
		item.activity = v.activity[varIdx] + v.lbdBonus[varIdx]
		heap.Push(&v.heap, item)

		return uint32(varIdx)
	}

	// Fallback to linear scan if heap is empty
	return v.selectVariable(assignments)
}

// selectVariable returns the unassigned variable with highest activity (linear scan)
// Uses effective activity (activity + LBD bonus) for selection
func (v *VSIDS) selectVariable(assignments []Assignment) uint32 {
	bestVar := uint32(0)
	bestEffectiveActivity := -1.0

	for i, act := range v.activity {
		if assignments[i].Level == 0 {
			effectiveActivity := act + v.lbdBonus[i]
			if effectiveActivity > bestEffectiveActivity {
				bestEffectiveActivity = effectiveActivity
				bestVar = uint32(i)
			}
		}
	}

	return bestVar
}

// selectVariableWithPhase returns the unassigned variable with highest activity
// and the phase to assign (true=positive, false=negative) based on saved phase
// Uses LRB (conflict participation) if enabled, otherwise VSIDS (activity) with heap
// Can also use LBD-based bonus (variables in low-LBD clauses prioritized)
func (v *VSIDS) selectVariableWithPhase(assignments []Assignment, savedPhase []bool) (uint32, bool) {
	var bestVar uint32

	// Use heap for fast selection when using standard VSIDS or LBD
	if !v.useLRB {
		bestVar = v.selectVariableWithHeap(assignments)
	} else {
		// Use linear scan for LRB (would need separate heap for conflict participation)
		bestVar = v.selectVariable(assignments)
	}

	// Use saved phase if available, otherwise default to true (positive literal)
	phase := true
	if savedPhase != nil && int(bestVar) < len(savedPhase) {
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

// diversify performs aggressive diversification when stuck in local minima
// Completely resets activity while preserving conflict history
func (v *VSIDS) diversify() {
	// Reset activity to initial values (small uniform values)
	for i := range v.activity {
		v.activity[i] = 1.0
	}

	// Reset LBD bonus completely
	for i := range v.lbdBonus {
		v.lbdBonus[i] = 0
	}

	// Keep conflict participation - it's still valuable
	// Scale down more aggressively
	for i := range v.conflictParticipation {
		v.conflictParticipation[i] = v.conflictParticipation[i] / 4
	}

	// Invalidate heap - needs rebuild with new activities
	v.heapValid = false
}
