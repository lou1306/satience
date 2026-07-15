package solver

import (
	"math"
	"satience/internal/cnf"
)

// DefaultDecayInterval is the default number of conflicts between VSIDS activity decays
// Value of 5 provides good balance: frequent decay keeps focus on recent conflicts
// Lower values = more focus on recent conflicts, better for structured instances
const DefaultDecayInterval = 5

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
// Tie-breaking: when scores are equal, lower varIdx wins. This makes heap
// ordering independent of insertion order, reducing trajectory sensitivity
// to clause ordering changes.
func (h *vsidsHeap) up(heapPos []int, i int) {
	s := *h
	for i > 0 {
		parent := (i - 1) / 2
		c := s[i]
		p := s[parent]
		if c.score < p.score || (c.score == p.score && c.varIdx >= p.varIdx) {
			break
		}
		s[i], s[parent] = p, c
		heapPos[c.varIdx] = parent
		heapPos[p.varIdx] = i
		i = parent
	}
}

// down sifts the item at position i downward to restore the heap property.
// Tie-breaking: when scores are equal, lower varIdx is preferred.
func (h *vsidsHeap) down(heapPos []int, i int) {
	s := *h
	n := len(s)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		right := left + 1
		largest := left
		if right < n {
			l := s[left]
			r := s[right]
			if r.score > l.score || (r.score == l.score && r.varIdx < l.varIdx) {
				largest = right
			}
		}
		cur := s[i]
		best := s[largest]
		if cur.score > best.score || (cur.score == best.score && cur.varIdx <= best.varIdx) {
			break
		}
		s[i], s[largest] = best, cur
		heapPos[cur.varIdx] = largest
		heapPos[best.varIdx] = i
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
	varInc                float64   // O(1) decay: bump counter (grows as varInc /= decayFactor)
	decayFactor           float64   // Current decay factor (initialDecay -> maxDecayFactor)
	inverseDecay          float64   // 1/decay for efficiency
	conflictCount         int       // Total conflicts for decay timing
	useLBD                bool      // Use LBD-based activity (variables in low-LBD clauses prioritized)
	lbdBonus              []float64 // Bonus score from appearing in low-LBD clauses
	lbdInc                float64   // O(1) decay: LBD bump counter (grows as lbdInc /= lbdBonusDecay)
	maxDecayFactor        float64   // Maximum decay factor
	decayIncrement        float64   // Increment per decay fire
	heap                  vsidsHeap // Activity heap for O(log n) selection
	heapPos               []int     // Position of each variable in heap (-1 = not in heap)
	heapValid             bool      // True if heap is up-to-date
	refreshInterval       int       // Conflicts between heap rebuilds (fixes deeply stale entries)
	decayInterval         int       // Number of conflicts between activity decays
	randomSeed            uint64    // Seed for deterministic random noise (default 0)
	// CHB (Conflict History Based) heuristic
	useCHB            bool      // Use CHB instead of VSIDS for variable selection
	conflictFrequency []float64 // Recent conflict frequency per variable (CHB)
	chbDecayFactor    float64   // CHB decay factor (default 0.75 - aggressive decay)
	chbDecayInterval  int       // CHB decay interval (default 25 conflicts)
	chbWeight         float64   // Weight for CHB in hybrid scoring (default 1.0)
	// Configurable parameters (exposed for tuning)
	initialDecayFactor   float64 // Initial decay factor (default 0.979)
	decayRampUpConflicts int     // Conflicts to reach max decay (default 25000)
	lbdBonusScale        float64 // Scale factor for LBD bonus (default 10.0)
	lbdBonusDecay        float64 // Decay factor for LBD bonus (default 0.9998)
	baseBumpAmount       float64 // Base bump amount for clauses (default 25.0)
	clauseInitBaseWeight float64 // Base weight for clause initialization (default 10.0)
	binaryClauseWeight   float64 // Weight for binary clauses (default 100.0)
}

// NewVSIDS creates a new VSIDS heuristic with clause-length weighted initialization
func NewVSIDS(numVars uint32) *VSIDS {
	// O(1) decay: varInc grows as varInc /= decayFactor each conflict.
	// Activities are bumped by varInc (not baseBumpAmount), so recent bumps
	// are naturally larger than old ones — no O(n) scan needed.
	// decayInterval=1 (decay every conflict) is free since decay is O(1).
	// Parameters compensated for 5x more frequent decay vs old interval=5:
	//   initialDecay: 0.90^(1/5) ≈ 0.9792, maxDecay: 0.999^(1/5) ≈ 0.9998
	//   lbdBonusDecay: 0.999^(1/5) ≈ 0.9998, rampUp: 5000 × 5 = 25000
	initialDecay := 0.9792
	maxDecay := 0.9998
	v := &VSIDS{
		activity:              make([]float64, numVars),
		varInc:                25.0, // Initial bump amount (matches baseBumpAmount)
		decayFactor:           initialDecay,
		inverseDecay:          1.0 / initialDecay,
		conflictCount:         0,
		useLBD:                true,
		lbdBonus:              make([]float64, numVars),
		lbdInc:                1.0,
		maxDecayFactor:        maxDecay,
		decayIncrement:        (maxDecay - initialDecay) / 25000.0,
		heap:                  make(vsidsHeap, 0, numVars),
		heapPos:               make([]int, numVars),
		heapValid:             false,
		refreshInterval:       2000,
		decayInterval:         1, // O(1) decay — fire every conflict
		randomSeed:            0,
		// Default parameter values
		initialDecayFactor:   initialDecay,
		decayRampUpConflicts: 25000,
		lbdBonusScale:        10.0,
		lbdBonusDecay:        0.9998,
		baseBumpAmount:       25.0,
		clauseInitBaseWeight: 10.0,
		binaryClauseWeight:   100.0,
		// CHB initialization
		useCHB:            false,
		conflictFrequency: make([]float64, numVars),
		chbDecayFactor:    0.75,
		chbDecayInterval:  25,
		chbWeight:         1.0,
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
// ALL variables are included: unassigned at their real score, assigned at
// -Inf (sunk to bottom). This ensures the heap never empties during search,
// eliminating frequent O(n) rebuilds. Assigned entries are sunk lazily during
// selection and restored on unassign.
func (v *VSIDS) buildHeap(assignments []Assignment) {
	v.heap = v.heap[:0]
	for i := range v.heapPos {
		v.heapPos[i] = -1
	}
	for i, act := range v.activity {
		var score float64
		if assignments[i].Level < 0 {
			score = act + v.lbdBonus[i]
		} else {
			score = math.Inf(-1)
		}
		v.heap = append(v.heap, vsidsHeapItem{
			varIdx: uint32(i),
			score:  score,
		})
		v.heapPos[i] = len(v.heap) - 1
	}
	v.heap.init(v.heapPos)
	v.heapValid = true
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
	v.varInc = amount // varInc starts at baseBumpAmount, grows via O(1) decay
}
// Much more aggressive decay to prevent any single variable from dominating
// With O(1) decay, decayInterval=1 is already the default.
func (v *VSIDS) SetAggressiveDecay() {
	v.initialDecayFactor = 0.30
	v.maxDecayFactor = 0.60
	v.decayFactor = 0.30
	v.inverseDecay = 1.0 / v.decayFactor
	v.decayIncrement = (v.maxDecayFactor - v.initialDecayFactor) / 5000.0
	v.decayInterval = 1
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

// onUnassign restores a variable's heap entry after it is unassigned (e.g. by
// backtrack). If the entry was sunk (assigned → -Inf), restore its real score
// and sift up. If the entry was not sunk (assigned but never reached the heap
// root), leave it stale — the score will be fixed by the periodic refresh
// (every refreshInterval conflicts) or lazily on selection if it reaches the
// root. Always sifting up on every unassign is too expensive: deep backtracks
// unassign hundreds of variables, each costing O(log n). The periodic refresh
// handles stale entries in bulk at O(n) every 2000 conflicts.
func (v *VSIDS) onUnassign(varIdx uint32) {
	pos := v.heapPos[varIdx]
	if pos < 0 {
		return
	}
	if v.heap[pos].score == math.Inf(-1) {
		v.heap[pos].score = v.activity[varIdx] + v.lbdBonus[varIdx]
		v.heap.up(v.heapPos, pos)
	}
}

// bumpLBD adds LBD bonus to variables in a learned clause
// Lower LBD = higher bonus (glue clauses are most important)
// Uses lbdInc (grows over time via O(1) decay) as a multiplier
func (v *VSIDS) bumpLBD(literals []cnf.Literal, lbd int) {
	if !v.useLBD {
		return
	}

	// Bonus formula: bonus = lbdInc * lbdBonusScale / (lbd^2)
	bonus := v.lbdInc * v.lbdBonusScale / float64(lbd*lbd)

	for _, lit := range literals {
		v.lbdBonus[lit.Var()] += bonus
		// Lazy heap: don't call increaseKey. Score fixed on selection/unassign.
	}
}

// decayLBD implements O(1) LBD bonus decay (varInc trick).
// Grows lbdInc so new LBD bumps are relatively larger. The heap key
// (activity + lbdBonus) doesn't change on decay, so the heap stays valid.
func (v *VSIDS) decayLBD() {
	if !v.useLBD {
		return
	}
	v.lbdInc /= v.lbdBonusDecay

	if v.lbdInc > 1e100 {
		scale := 1.0 / v.lbdInc
		for i := range v.lbdBonus {
			v.lbdBonus[i] *= scale
		}
		v.lbdInc = 1.0
		v.heapValid = false
	}
}

// bump increases the activity of a variable
func (v *VSIDS) bump(varIdx uint32) {
	v.activity[varIdx] += 1.0
}

// bumpLarge increases the activity of a variable by a larger amount
// Used for variables in conflict clauses to make them more likely to be chosen.
// Lazy heap: does NOT call increaseKey. The heap entry's stored score becomes
// stale (stored < actual). The stale entry is fixed lazily when it reaches the
// root of the heap during selection (if unassigned) or on unassign (if sunk).
func (v *VSIDS) bumpLarge(varIdx uint32, amount float64) {
	v.activity[varIdx] += amount
}

// bumpClause increases activity for all variables in a clause
// Bump amount is inversely proportional to clause size - smaller clauses = larger bump
// Uses varInc (grows over time via O(1) decay) instead of fixed baseBumpAmount
func (v *VSIDS) bumpClause(literals []cnf.Literal, assignments []Assignment) {
	bumpAmount := v.varInc / float64(len(literals))
	minBump := v.varInc * 0.04
	if bumpAmount < minBump {
		bumpAmount = minBump
	}

	for _, lit := range literals {
		v.bumpLarge(lit.Var(), bumpAmount)

		if v.useCHB {
			v.conflictFrequency[lit.Var()] += bumpAmount
		}
	}
	v.conflictCount++

	// Periodic heap refresh: rebuild the heap to fix deeply stale entries
	// (variables bumped many times but never reached the heap root). The
	// rebuild is O(n) but only fires every refreshInterval conflicts.
	if v.conflictCount%v.refreshInterval == 0 {
		v.heapValid = false
	}

	// CHB: Periodic decay for conflict frequency
	if v.useCHB && v.conflictCount%v.chbDecayInterval == 0 {
		v.decayCHB(assignments)
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

// decay implements O(1) activity decay (MiniSat varInc trick).
// Instead of scaling all activity[i] by decayFactor (O(n) scan + O(n) heap
// rebuild), we grow varInc: varInc /= decayFactor. Bumps add varInc, so
// recent bumps are naturally larger than old ones. The heap key
// (activity + lbdBonus) doesn't change on decay, so the heap stays valid.
// Periodic rescaling prevents varInc from overflowing float64 precision.
// conflictCount is incremented in bumpClause (called before decay in
// handleConflict), so decay must NOT increment it again.
func (v *VSIDS) decay(assignments []Assignment) {
	// Gradually increase decay factor toward max
	if v.decayFactor < v.maxDecayFactor {
		v.decayFactor += v.decayIncrement
		if v.decayFactor > v.maxDecayFactor {
			v.decayFactor = v.maxDecayFactor
		}
		v.inverseDecay = 1.0 / v.decayFactor
	}

	// O(1) decay: grow varInc so new bumps are relatively larger
	v.varInc /= v.decayFactor

	// Rescale when varInc gets too large to prevent float precision loss.
	// This fires rarely (~every 500K conflicts). Scales all activities and
	// varInc by the same factor, preserving relative ordering — the heap
	// key ratios are unchanged, so no rebuild needed.
	if v.varInc > 1e100 {
		scale := 1.0 / v.varInc
		for i := range v.activity {
			v.activity[i] *= scale
		}
		v.varInc = 1.0
		v.heapValid = false
	}
}

// selectVariableWithHeap returns the unassigned variable with highest activity.
// Lazy heap with sink approach:
// - Peek at the root. If assigned, sink it to -Inf (it stays in the heap but
//   drops to the bottom). If unassigned but stale (activity was bumped since
//   last heap update), fix the score in-place. If unassigned and current,
//   return it (without removeMax — decide() will assign it, and the next
//   select will sink it).
// - The heap never empties (entries are sunk, not removed), eliminating
//   frequent O(n) buildHeap calls.
// - onUnassign restores sunk entries' real scores and sifts them up.
func (v *VSIDS) selectVariableWithHeap(assignments []Assignment) uint32 {
	if !v.heapValid || len(v.heap) == 0 {
		v.buildHeap(assignments)
	}

	for len(v.heap) > 0 {
		item := v.heap[0]
		varIdx := item.varIdx

		if int(varIdx) >= len(assignments) {
			v.heap.removeMax(v.heapPos)
			continue
		}

		if assignments[varIdx].Level >= 0 {
			// Assigned — sink to bottom so it doesn't reappear at the root.
			v.heap[0].score = math.Inf(-1)
			v.heap.down(v.heapPos, 0)
			continue
		}

		// Unassigned — check if score is stale (activity was bumped).
		actualScore := v.activity[varIdx] + v.lbdBonus[varIdx]
		if item.score != actualScore {
			// Stale — update score. Bumps only increase activity, so
			// actualScore > stored score. At the root, sift-up is a no-op,
			// so the entry stays at position 0 with the updated score.
			v.heap[0].score = actualScore
			continue
		}

		// Unassigned and up-to-date — return it. Don't remove from heap;
		// decide() will assign it, and the next select will sink it.
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
	bestVar := v.selectVariableWithHeap(assignments)

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
