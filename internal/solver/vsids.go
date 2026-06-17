package solver

import (
	"satience/internal/cnf"
)

// DefaultDecayInterval is the default number of conflicts between VSIDS activity decays
// Value of 10 provides good balance: frequent decay keeps focus on recent conflicts
// Lower values = more focus on recent conflicts, better for structured instances
const DefaultDecayInterval = 10

// vsidsHeapItem represents a variable in the activity heap
type vsidsHeapItem struct {
	varIdx   uint32
	activity float64
}

// vsidsHeap is a custom max-heap for activity-based variable selection
// Implemented without container/heap to avoid interface{} type assertions
type vsidsHeap []vsidsHeapItem

// up restores the heap property by moving an element up the tree
// Used after inserting a new element at the end
func (h *vsidsHeap) up(pos int) {
	for pos > 0 {
		parent := (pos - 1) / 2
		if (*h)[pos].activity <= (*h)[parent].activity {
			break
		}
		(*h)[pos], (*h)[parent] = (*h)[parent], (*h)[pos]
		pos = parent
	}
}

// down restores the heap property by moving an element down the tree
// Used after removing the root element
func (h *vsidsHeap) down(pos int) {
	n := len(*h)
	for {
		left := 2*pos + 1
		if left >= n {
			break
		}
		right := left + 1
		largest := left
		if right < n && (*h)[right].activity > (*h)[left].activity {
			largest = right
		}
		if (*h)[pos].activity >= (*h)[largest].activity {
			break
		}
		(*h)[pos], (*h)[largest] = (*h)[largest], (*h)[pos]
		pos = largest
	}
}

// push adds an element to the heap
func (h *vsidsHeap) push(item vsidsHeapItem) {
	*h = append(*h, item)
	h.up(len(*h) - 1)
}

// pop removes and returns the maximum element (root of the heap)
func (h *vsidsHeap) pop() vsidsHeapItem {
	n := len(*h)
	// Swap root with last element
	(*h)[0], (*h)[n-1] = (*h)[n-1], (*h)[0]
	item := (*h)[n-1]
	*h = (*h)[:n-1]
	if len(*h) > 0 {
		h.down(0)
	}
	return item
}

// init builds a heap from an unsorted slice in O(n) time
func (h *vsidsHeap) init() {
	n := len(*h)
	// Start from the last non-leaf node and heapify down
	for i := n/2 - 1; i >= 0; i-- {
		h.down(i)
	}
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
	decayInterval         int       // Number of conflicts between activity decays
	randomSeed            uint64    // Seed for deterministic random noise (default 0)
}

// NewVSIDS creates a new VSIDS heuristic with clause-length weighted initialization
func NewVSIDS(numVars uint32) *VSIDS {
	// Standard decay settings (MiniSat-style)
	// Bump amounts reduced to prevent activity explosion
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
		decayInterval:         DefaultDecayInterval,
		randomSeed:            0, // Default seed for deterministic randomness
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

// InitializeFromConflicts is DEPRECATED - investigated but did not improve performance
// Tested on PHP instances with various conflict counts (10, 20, 50)
// Result: Always worse than clause-structure initialization
// PHP 6p5h: 4167 conflicts (conflict-based) vs 2364 conflicts (clause-based)
//
// Reason: Learning clauses during initialization pollutes the clause database
// and biases search in suboptimal direction. Clause-structure initialization
// (short clauses = higher activity) is already well-tuned for diverse instances.
//
// Keeping this function for future reference and potential re-investigation.
func (v *VSIDS) InitializeFromConflicts(solver *CDCLSolver, numConflicts int) {
	// Implementation removed - see deprecation note above
	_ = solver
	_ = numConflicts
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
			})
		}
	}

	// Build heap in O(n) time
	v.heap.init()
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

// SetRandomSeed sets the seed for deterministic random noise in tie-breaking
func (v *VSIDS) SetRandomSeed(seed uint64) {
	v.randomSeed = seed
}

// SetDecayInterval sets the number of conflicts between activity decays
// Higher values = fewer heap rebuilds but slower activity differentiation
// Lower values = more frequent decay but more heap rebuilds
// Default is 10, which provides good balance for most instances
func (v *VSIDS) SetDecayInterval(interval int) {
	if interval < 1 {
		interval = 1
	}
	v.decayInterval = interval
}

// bumpLBD adds LBD bonus to variables in a learned clause
// Lower LBD = higher bonus (glue clauses are most important)
func (v *VSIDS) bumpLBD(literals []cnf.Literal, lbd int) {
	if !v.useLBD {
		return
	}

	// Bonus formula: MODERATE bonus for lower LBD
	// Reduced from 20000 to 2000 to prevent activity explosion on random instances
	// LBD=2: bonus = 500 (was 10000)
	// LBD=3: bonus = 222 (was 3333)
	// LBD=4: bonus = 125 (was 1250)
	// LBD=10: bonus = 20 (was 100)
	// Still prioritizes glue clauses but doesn't dominate VSIDS on random instances
	bonus := 2000.0 / float64(lbd*lbd)

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
	// Scale bump by clause size: BALANCED bumps for mixed instance types
	// Reduced from 400 to 50 to prevent activity explosion while maintaining effectiveness
	// Binary clauses: bump = 25.0
	// Ternary clauses: bump = 16.7
	// 5-literal clauses: bump = 10.0
	// 10-literal clauses: bump = 5.0
	baseBump := 50.0
	bumpAmount := baseBump / float64(len(literals))
	if bumpAmount < 2.0 {
		bumpAmount = 2.0 // Minimum bump for very large clauses
	}

	for _, lit := range literals {
		// Moderate boost for variables that appear in conflicts frequently
		// Scaled down from 0.1 to 0.05 to prevent runaway feedback
		conflictBoost := 1.0 + float64(v.conflictParticipation[lit.Var()])*0.05
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

// decay decays all activity scores periodically (MiniSat-style)
// This creates strong differentiation between important and unimportant variables
// Decay factor starts at 0.95 and increases toward max for focused search
// Only decays every v.decayInterval conflicts to reduce heap rebuild overhead
func (v *VSIDS) decay() {
	v.conflictCount++
	
	// Lazy decay: only decay every decayInterval conflicts
	// This reduces heap rebuilds while maintaining good variable selection quality
	if v.conflictCount%v.decayInterval != 0 {
		return
	}
	
	// Gradually increase decay factor toward max
	if v.decayFactor < v.maxDecayFactor {
		v.decayFactor += v.decayIncrement
		if v.decayFactor > v.maxDecayFactor {
			v.decayFactor = v.maxDecayFactor
		}
		v.inverseDecay = 1.0 / v.decayFactor
	}

	// Apply decay to all activity scores
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
	if !v.heapValid || len(v.heap) == 0 {
		v.buildHeap(assignments)
	}

	// Pop variables until we find an unassigned one
	for len(v.heap) > 0 {
		item := v.heap.pop()
		varIdx := int(item.varIdx)

		// Skip if variable is now assigned (stale heap entry)
		if varIdx >= len(assignments) || assignments[varIdx].Level != 0 {
			continue
		}

		// Push it back with current activity (no noise)
		// Variables with higher activity naturally stay near top of heap
		item.activity = v.activity[varIdx] + v.lbdBonus[varIdx]
		v.heap.push(item)

		return uint32(varIdx)
	}

	// Fallback to linear scan if heap is empty
	return v.selectVariable(assignments)
}

// selectVariable returns the unassigned variable with highest activity (linear scan)
// Uses effective activity (activity + LBD bonus) for selection
// Adds randomization to break ties and avoid variable lock-in on random instances
func (v *VSIDS) selectVariable(assignments []Assignment) uint32 {
	bestVar := uint32(0)
	bestEffectiveActivity := -1.0

	for i, act := range v.activity {
		if assignments[i].Level == 0 {
			// Add small deterministic noise (±0.5%) to break ties
			// Use XORShift64 for deterministic noise (seeded from v.randomSeed, default 0)
			v.randomSeed ^= v.randomSeed << 13
			v.randomSeed ^= v.randomSeed >> 7
			v.randomSeed ^= v.randomSeed << 17
			// Convert to float64 in range [0, 1) and scale to ±0.5% noise
			noise := (float64(v.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF) - 0.5) * 0.01 * (act + v.lbdBonus[i])
			effectiveActivity := act + v.lbdBonus[i] + noise
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

// diversifyAggressive performs very aggressive diversification when completely stuck
// Resets all scores and adds deterministic noise to break symmetry
func (v *VSIDS) diversifyAggressive() {
	// Reset activity with deterministic values to break symmetry
	// Use XORShift64 for deterministic noise (seeded from v.randomSeed, default 0)
	for i := range v.activity {
		v.randomSeed ^= v.randomSeed << 13
		v.randomSeed ^= v.randomSeed >> 7
		v.randomSeed ^= v.randomSeed << 17
		// Convert to float64 in range [0, 1)
		v.activity[i] = 0.5 + float64(v.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF)*0.5
	}

	// Reset LBD bonus completely
	for i := range v.lbdBonus {
		v.lbdBonus[i] = 0
	}

	// Reset conflict participation more aggressively
	for i := range v.conflictParticipation {
		v.conflictParticipation[i] = v.conflictParticipation[i] / 8
	}

	// Reset decay factor to initial value to encourage exploration
	v.decayFactor = 0.88

	// Invalidate heap - needs rebuild with new activities
	v.heapValid = false
}
