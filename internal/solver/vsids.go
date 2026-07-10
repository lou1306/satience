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
	varIdx uint32
	score  float64
}

// vsidsHeap is a max-heap of vsidsHeapItem.
// A separate heapPos array (owned by VSIDS) maps variable → heap index for
// O(log n) increaseKey/decreaseKey without full rebuilds.
type vsidsHeap []vsidsHeapItem

// up sifts the item at position i upward to restore the heap property.
func (h *vsidsHeap) up(heapPos []int, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if (*h)[i].score <= (*h)[parent].score {
			break
		}
		(*h)[i], (*h)[parent] = (*h)[parent], (*h)[i]
		heapPos[(*h)[i].varIdx] = i
		heapPos[(*h)[parent].varIdx] = parent
		i = parent
	}
}

// down sifts the item at position i downward to restore the heap property.
func (h *vsidsHeap) down(heapPos []int, i int) {
	n := len(*h)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		right := left + 1
		largest := left
		if right < n && (*h)[right].score > (*h)[left].score {
			largest = right
		}
		if (*h)[i].score >= (*h)[largest].score {
			break
		}
		(*h)[i], (*h)[largest] = (*h)[largest], (*h)[i]
		heapPos[(*h)[i].varIdx] = i
		heapPos[(*h)[largest].varIdx] = largest
		i = largest
	}
}

// insert adds a variable to the heap with the given score.
func (h *vsidsHeap) insert(heapPos []int, varIdx uint32, score float64) {
	pos := len(*h)
	*h = append(*h, vsidsHeapItem{varIdx: varIdx, score: score})
	heapPos[varIdx] = pos
	h.up(heapPos, pos)
}

// removeMax removes and returns the maximum element (root).
func (h *vsidsHeap) removeMax(heapPos []int) vsidsHeapItem {
	n := len(*h)
	item := (*h)[0]
	heapPos[item.varIdx] = -1
	if n > 1 {
		(*h)[0] = (*h)[n-1]
		heapPos[(*h)[0].varIdx] = 0
		*h = (*h)[:n-1]
		h.down(heapPos, 0)
	} else {
		*h = (*h)[:0]
	}
	return item
}

// increaseKey updates the score of varIdx (must be in heap) and sifts up.
func (h *vsidsHeap) increaseKey(heapPos []int, varIdx uint32, score float64) {
	pos := heapPos[varIdx]
	if pos < 0 {
		return
	}
	(*h)[pos].score = score
	h.up(heapPos, pos)
}

// decreaseKey updates the score of varIdx (must be in heap) and sifts down.
func (h *vsidsHeap) decreaseKey(heapPos []int, varIdx uint32, score float64) {
	pos := heapPos[varIdx]
	if pos < 0 {
		return
	}
	(*h)[pos].score = score
	h.down(heapPos, pos)
}

// init builds a heap from an unsorted slice in O(n) time.
func (h *vsidsHeap) init(heapPos []int) {
	n := len(*h)
	for i := n/2 - 1; i >= 0; i-- {
		h.down(heapPos, i)
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
	decayFactor           float64   // Current decay factor (initialDecay -> maxDecayFactor)
	inverseDecay          float64   // 1/decay for efficiency
	useLRB                bool      // Use LRB heuristic instead of pure VSIDS
	lrbDecayInterval      int       // Decay every N conflicts for LRB
	conflictCount         int       // Total conflicts for LRB decay timing
	useLBD                bool      // Use LBD-based activity (variables in low-LBD clauses prioritized)
	lbdBonus              []float64 // Bonus score from appearing in low-LBD clauses
	maxDecayFactor        float64   // Maximum decay factor
	decayIncrement        float64   // Increment per conflict
	heap                  vsidsHeap // Activity heap for O(log n) selection
	heapPos               []int     // Position of each variable in heap (-1 = not in heap)
	heapValid             bool      // True if heap is up-to-date
	decayInterval         int       // Number of conflicts between activity decays
	randomSeed            uint64    // Seed for deterministic random noise (default 0)
	// Symmetry breaking: track activity momentum and decision recency
	lastDecisionConflict   []int     // Last conflict where variable was decided (-1 if never)
	decisionRecencyPenalty []float64 // Penalty for recently decided variables
	// CHB (Conflict History Based) heuristic
	useCHB            bool      // Use CHB instead of VSIDS for variable selection
	conflictFrequency []float64 // Recent conflict frequency per variable (CHB)
	chbDecayFactor    float64   // CHB decay factor (default 0.75 - aggressive decay)
	chbDecayInterval  int       // CHB decay interval (default 50 conflicts)
	chbWeight         float64   // Weight for CHB in hybrid scoring (default 1.0)
	// Configurable parameters (exposed for tuning)
	initialDecayFactor   float64 // Initial decay factor (default 0.95)
	decayRampUpConflicts int     // Conflicts to reach max decay (default 10000)
	lbdBonusScale        float64 // Scale factor for LBD bonus (default 10.0)
	lbdBonusDecay        float64 // Decay factor for LBD bonus (default 0.999)
	baseBumpAmount       float64 // Base bump amount for clauses (default 50.0)
	recencyPenaltyScale  float64 // Scale for recency penalty (default 50.0)
	recencyPenaltyDecay  float64 // Decay for recency penalty (default 0.9)
	recencyWindow        int     // Window for recency penalty (default 5 conflicts)
	clauseInitBaseWeight float64 // Base weight for clause initialization (default 10.0)
	binaryClauseWeight   float64 // Weight for binary clauses (default 100.0)
	activityResetScale   float64 // Scale for activity reset (default 0.5)
}

// NewVSIDS creates a new VSIDS heuristic with clause-length weighted initialization
func NewVSIDS(numVars uint32) *VSIDS {
	// Standard decay settings (MiniSat-style)
	initialDecay := 0.95
	maxDecay := 0.999
	v := &VSIDS{
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
		heapPos:               make([]int, numVars),
		heapValid:             false,
		decayInterval:         DefaultDecayInterval,
		randomSeed:            0,
		// Symmetry breaking initialization
		lastDecisionConflict:   make([]int, numVars),
		decisionRecencyPenalty: make([]float64, numVars),
		// Default parameter values
		initialDecayFactor:   initialDecay,
		decayRampUpConflicts: 10000,
		lbdBonusScale:        10.0,
		lbdBonusDecay:        0.999,
		baseBumpAmount:       50.0,
		recencyPenaltyScale:  50.0,
		recencyPenaltyDecay:  0.9,
		recencyWindow:        5,
		clauseInitBaseWeight: 10.0,
		binaryClauseWeight:   100.0,
		activityResetScale:   0.5,
		// CHB initialization
		useCHB:            false,
		conflictFrequency: make([]float64, numVars),
		chbDecayFactor:    0.75,
		chbDecayInterval:  50,
		chbWeight:         1.0,
	}
	for i := range v.lastDecisionConflict {
		v.lastDecisionConflict[i] = -1
	}
	for i := range v.heapPos {
		v.heapPos[i] = -1
	}
	return v
}

// InitializeFromClauses initializes VSIDS activity based on clause participation
// Variables in shorter clauses get MUCH higher activity (more constrained = more important)
// Binary clauses get binaryClauseWeight multiplier, ternary clauses get special weight
func (v *VSIDS) InitializeFromClauses(clauses []cnf.Clause) {
	for _, clause := range clauses {
		baseWeight := v.clauseInitBaseWeight
		if len(clause.Literals) == 2 {
			baseWeight = v.binaryClauseWeight
		} else if len(clause.Literals) == 3 {
			// Ternary clauses are almost as important as binary - weight them heavily
			baseWeight = v.binaryClauseWeight * 0.5
		}
		weight := baseWeight / float64(len(clause.Literals))

		for _, lit := range clause.Literals {
			v.activity[lit.Var()] += weight
		}
	}
	v.heapValid = false // Invalidate heap after modifying activities
}

// buildHeap rebuilds the activity heap from current activity scores.
// Only includes unassigned variables. Called on init and after restart.
func (v *VSIDS) buildHeap(assignments []Assignment) {
	v.heap = v.heap[:0]
	for i := range v.heapPos {
		v.heapPos[i] = -1
	}
	for i, act := range v.activity {
		if assignments[i].Level < 0 {
			v.heap = append(v.heap, vsidsHeapItem{
				varIdx: uint32(i),
				score:  act + v.lbdBonus[i],
			})
			v.heapPos[i] = len(v.heap) - 1
		}
	}
	v.heap.init(v.heapPos)
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

// EnableCHB enables CHB (Conflict History Based) heuristic
// CHB tracks recent conflict frequency with aggressive decay (default 0.75 every 50 conflicts)
// This focuses search on variables involved in recent conflicts rather than cumulative activity
func (v *VSIDS) EnableCHB() {
	v.useCHB = true
}

// SetRandomSeed sets the seed for deterministic random noise in tie-breaking
func (v *VSIDS) SetRandomSeed(seed uint64) {
	v.randomSeed = seed
}

// TrackDecision records that a variable was decided on at the current conflict
// This is used for symmetry breaking to avoid repeatedly deciding on the same variable
func (v *VSIDS) TrackDecision(varIdx uint32, conflictCount int) {
	if int(varIdx) < len(v.lastDecisionConflict) {
		lastConflict := v.lastDecisionConflict[varIdx]
		if lastConflict >= 0 {
			recency := conflictCount - lastConflict
			// Apply recency penalty if within recency window
			if recency < v.recencyWindow {
				v.decisionRecencyPenalty[varIdx] = v.recencyPenaltyScale / float64(recency+1)
			} else {
				// Decay penalty if outside recency window
				v.decisionRecencyPenalty[varIdx] *= v.recencyPenaltyDecay
			}
		}
		v.lastDecisionConflict[varIdx] = conflictCount
	}
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

// SetInitialDecayFactor sets the initial decay factor (default 0.95)
// Lower values = more aggressive decay = more exploration
func (v *VSIDS) SetInitialDecayFactor(factor float64) {
	if factor < 0.5 || factor > 0.99 {
		factor = 0.95
	}
	v.initialDecayFactor = factor
	v.decayFactor = factor
	v.inverseDecay = 1.0 / factor
}

// SetMaxDecayFactor sets the maximum decay factor (default 0.999)
// Higher values = slower decay = more focused search on important variables
func (v *VSIDS) SetMaxDecayFactor(factor float64) {
	if factor < 0.9 || factor > 1.0 {
		factor = 0.999
	}
	v.maxDecayFactor = factor
	// Recalculate decay increment based on new max
	v.decayIncrement = (v.maxDecayFactor - v.initialDecayFactor) / float64(v.decayRampUpConflicts)
}

// SetDecayRampUpConflicts sets the number of conflicts to reach max decay (default 10000)
func (v *VSIDS) SetDecayRampUpConflicts(conflicts int) {
	if conflicts < 100 {
		conflicts = 100
	}
	v.decayRampUpConflicts = conflicts
	v.decayIncrement = (v.maxDecayFactor - v.initialDecayFactor) / float64(conflicts)
}

// SetLBDBonusScale sets the scale factor for LBD bonus (default 2000.0)
// Higher values = stronger preference for low-LBD (glue) clauses
func (v *VSIDS) SetLBDBonusScale(scale float64) {
	if scale < 0 {
		scale = 0
	}
	v.lbdBonusScale = scale
}
// SetBaseBumpAmount sets the base bump amount for clauses (default 50.0)
// Higher values = more aggressive activity increase for conflict variables
func (v *VSIDS) SetBaseBumpAmount(amount float64) {
	if amount < 1.0 {
		amount = 1.0
	}
	v.baseBumpAmount = amount
}
// Much more aggressive decay to prevent any single variable from dominating
// Decay every conflict (not every 10) with very low base (0.30→0.60)
func (v *VSIDS) SetAggressiveDecay() {
	v.initialDecayFactor = 0.30
	v.maxDecayFactor = 0.60
	v.decayFactor = 0.30
	v.inverseDecay = 1.0 / v.decayFactor
	v.decayIncrement = (v.maxDecayFactor - v.initialDecayFactor) / 5000.0
	v.decayInterval = 1 // Decay every conflict, not every 10
}
// SetClauseInitWeights sets the initialization weights for clauses
// baseWeight: base weight for all clauses (default 10.0)
// binaryWeight: weight multiplier for binary clauses (default 100.0)
func (v *VSIDS) SetClauseInitWeights(baseWeight, binaryWeight float64) {
	if baseWeight < 0 {
		baseWeight = 0
	}
	if binaryWeight < 0 {
		binaryWeight = 0
	}
	v.clauseInitBaseWeight = baseWeight
	v.binaryClauseWeight = binaryWeight
}
// ResetActivityPartial partially resets activity to escape local minima
// scale: fraction of activity to keep (0.0-1.0)
// Adds random noise to break ties and prevent immediate re-convergence
func (v *VSIDS) ResetActivityPartial(scale float64) {
	if scale < 0.0 {
		scale = 0.0
	}
	if scale > 1.0 {
		scale = 1.0
	}
	for i := range v.activity {
		// Keep partial activity
		v.activity[i] *= scale
		// Add random noise (0-10% of original) to break ties
		noise := float64(i%100) / 1000.0
		v.activity[i] += noise
	}
	v.heapValid = false // Force heap rebuild
}

// onUnassign re-inserts a variable into the heap after it is unassigned
// (e.g. by backtrack). If the variable is already in the heap (it was
// propagated, not yet popped), this is a no-op.
func (v *VSIDS) onUnassign(varIdx uint32) {
	if v.heapPos[varIdx] < 0 {
		score := v.activity[varIdx] + v.lbdBonus[varIdx]
		v.heap.insert(v.heapPos, varIdx, score)
	}
}

// bumpLBD adds LBD bonus to variables in a learned clause
// Lower LBD = higher bonus (glue clauses are most important)
func (v *VSIDS) bumpLBD(literals []cnf.Literal, lbd int) {
	if !v.useLBD {
		return
	}

	// Bonus formula: bonus = lbdBonusScale / (lbd^2)
	// LBD=2: bonus = lbdBonusScale/4
	// LBD=3: bonus = lbdBonusScale/9
	bonus := v.lbdBonusScale / float64(lbd*lbd)

	for _, lit := range literals {
		v.lbdBonus[lit.Var()] += bonus
		// Incrementally update heap position.
		score := v.activity[lit.Var()] + v.lbdBonus[lit.Var()]
		v.heap.increaseKey(v.heapPos, lit.Var(), score)
	}
}

// decayLBD decays LBD bonus scores.
// Gated by decayInterval (same as activity decay) to avoid O(n) every conflict.
// Called after decay() which increments conflictCount, so the gate check is valid.
func (v *VSIDS) decayLBD() {
	if !v.useLBD {
		return
	}
	if v.conflictCount%v.decayInterval != 0 {
		return
	}

	for i := range v.lbdBonus {
		v.lbdBonus[i] *= v.lbdBonusDecay
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
	// Incrementally update heap position (O(log n)) instead of invalidating.
	score := v.activity[varIdx] + v.lbdBonus[varIdx]
	v.heap.increaseKey(v.heapPos, varIdx, score)
}

// bumpClause increases activity for all variables in a clause
// Also tracks conflict participation for LRB heuristic
// Bump amount is inversely proportional to clause size - smaller clauses = larger bump
func (v *VSIDS) bumpClause(literals []cnf.Literal, assignments []Assignment) {
	baseBump := v.baseBumpAmount
	bumpAmount := baseBump / float64(len(literals))
	minBump := v.baseBumpAmount * 0.04 // 2.0 when baseBump=50.0
	if bumpAmount < minBump {
		bumpAmount = minBump
	}

	for _, lit := range literals {
		v.bumpLarge(lit.Var(), bumpAmount)

		// LRB/CHB bookkeeping — only when those heuristics are active.
		// When useLRB/useCHB are false (the default), skip the per-variable
		// conflictParticipation increment and the non-standard anti-lock-in
		// reset (hard reset to 1.0 + decreaseKey when > 500), which are pure
		// overhead serving no purpose under VSIDS-only selection.
		if v.useLRB || v.useCHB {
			v.conflictParticipation[lit.Var()]++
			if v.conflictParticipation[lit.Var()] > 500 {
				v.activity[lit.Var()] = 1.0
				v.conflictParticipation[lit.Var()] = 0
				score := 1.0 + v.lbdBonus[lit.Var()]
				v.heap.decreaseKey(v.heapPos, lit.Var(), score)
			}
		}

		if v.useCHB {
			v.conflictFrequency[lit.Var()] += bumpAmount
		}
	}
	v.conflictCount++

	// Periodic decay for LRB scores
	if v.useLRB && v.conflictCount%v.lrbDecayInterval == 0 {
		v.decayLRB(assignments)
	}

	// CHB: Periodic decay for conflict frequency
	if v.useCHB && v.conflictCount%v.chbDecayInterval == 0 {
		v.decayCHB(assignments)
	}
}

// decayLRB decays LRB conflict participation scores
func (v *VSIDS) decayLRB(assignments []Assignment) {
	for i := range v.conflictParticipation {
		// Skip pre-assigned variables (level 0) - not selectable
		if assignments[i].Level == 0 {
			continue
		}
		v.conflictParticipation[i] = v.conflictParticipation[i] / 2
	}
}

// decayCHB decays CHB conflict frequency scores (aggressive decay)
// CHB uses much more aggressive decay than VSIDS to focus on recent conflicts
func (v *VSIDS) decayCHB(assignments []Assignment) {
	for i := range v.conflictFrequency {
		// Skip pre-assigned variables (level 0) - not selectable
		if assignments[i].Level == 0 {
			continue
		}
		v.conflictFrequency[i] *= v.chbDecayFactor
	}
}

// decay decays all activity scores periodically (MiniSat-style)
// This creates strong differentiation between important and unimportant variables
// Decay factor starts at 0.95 and increases toward max for focused search
// Only decays every v.decayInterval conflicts to reduce overhead
func (v *VSIDS) decay(assignments []Assignment) {
	v.conflictCount++

	// Lazy decay: only decay every decayInterval conflicts
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

	// OPTIMIZATION: Skip pre-assigned variables (level 0) - they're not selectable
	for i := range v.activity {
		if assignments[i].Level == 0 {
			continue
		}
		v.activity[i] *= v.decayFactor
		if v.useLRB {
			v.decisionRecencyPenalty[i] *= v.recencyPenaltyDecay
		}
	}

	// Invalidate the heap. The heap key is activity + lbdBonus, and decay
	// only scales activity (not lbdBonus), so the relative ordering can change.
	// Rebuilding every decayInterval (10) conflicts is O(n/10) per conflict —
	// far cheaper than the old O(n) per-conflict rebuild.
	v.heapValid = false
}

// selectVariableWithHeap returns the unassigned variable with highest activity.
// Uses an incremental heap: pop the max, skip assigned (discard them — they'll
// be re-inserted on backtrack via onUnassign). The selected variable is NOT
// re-inserted (it's about to be assigned); it will be re-inserted on backtrack.
func (v *VSIDS) selectVariableWithHeap(assignments []Assignment) uint32 {
	if !v.heapValid || len(v.heap) == 0 {
		v.buildHeap(assignments)
	}

	for len(v.heap) > 0 {
		item := v.heap.removeMax(v.heapPos)
		varIdx := item.varIdx

		if int(varIdx) >= len(assignments) || assignments[varIdx].Level >= 0 {
			continue // assigned — discard, re-inserted on backtrack
		}
		return varIdx
	}

	// Fallback to linear scan if heap is empty
	return v.selectVariable(assignments)
}

// selectVariable returns the unassigned variable with highest activity (linear scan)
// Uses effective activity (activity + LBD bonus) for selection
// Adds randomization to break ties and avoid variable lock-in on random instances
// CHB: Uses conflict frequency instead of VSIDS activity when enabled
func (v *VSIDS) selectVariable(assignments []Assignment) uint32 {
	bestVar := uint32(0)
	bestEffectiveActivity := -1.0

	for i := range v.activity {
		if assignments[i].Level < 0 {
			v.randomSeed ^= v.randomSeed << 13
			v.randomSeed ^= v.randomSeed >> 7
			v.randomSeed ^= v.randomSeed << 17
			noise := (float64(v.randomSeed&0xFFFFFFFF)/float64(0xFFFFFFFF) - 0.5) * 0.01

			// CHB: Use conflict frequency instead of VSIDS activity
			var baseActivity float64
			if v.useCHB {
				baseActivity = v.conflictFrequency[i]
			} else {
				baseActivity = v.activity[i]
			}

			effectiveActivity := baseActivity + v.lbdBonus[i] + noise*(baseActivity+v.lbdBonus[i])
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
		if assignments[i].Level < 0 {
			return true
		}
	}
	return false
}
