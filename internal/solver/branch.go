package solver

import (
	"math"
)

// Branching-heuristic modes. VSIDS is the default and remains the fallback;
// CHB and LRB are alternate per-variable score heuristics selected via -branch.
const (
	branchVSIDS = 0
	branchCHB   = 1
	branchLRB   = 2
)

// branchHeuristic implements conflict-history-based (CHB) and learning-rate
// (LRB) variable selection, alternative to the VSIDS activity heap. Both are
// deterministic (no RNG; exact tie-break by lower variable index, like VSIDS),
// so they preserve the solver's bit-determinism under a fixed seed.
//
//   - CHB: each variable keeps an exponential-recency-weighted score. On a
//     conflict a touched variable's score moves toward 1 with decay
//     pow(y, step-last[v]) where step-last[v] is the gap in conflicts since
//     its previous bump; recently-active variables score highest.
//   - LRB: each variable keeps a moving average of how many conflicts it
//     appears in (conf[v] accumulated, merged into an EWMA at a period), which
//     is a learning-rate signal: variables present in many recent learned
//     clauses are chosen.
//
// Selection uses a max-heap (parallel scores/varIdxs + heapPos), sifted
// incrementally on bump and lazily on selection (assigned variables are sunk to
// -Inf at selection and restored on unassign / re-sifted from the root, so the
// max-unassigned invariant is preserved even though a CHB bump can DECREASE a
// score — unlike VSIDS, whose lazy fix assumed monotone increases).
type branchHeuristic struct {
	mode       int
	scores     []float64 // CHB: recency score in [0,1]; LRB: EWMA conflict rate
	last       []uint64  // CHB: conflict index of each var's last bump
	conf       []uint64  // LRB: conflicts seen in current epoch per var
	step       uint64    // CHB: running conflict counter
	epochCount uint64    // LRB: conflicts since last merge
	varIdxs    []uint32  // heap: variable at each position
	heapPos    []int     // var -> heap position (-1 = absent)
	heapValid  bool
}

const (
	chbDecayBase   = 0.9 // CHB recency decay base (pow(base, gap))
	lrbEwmaAlpha   = 0.9 // LRB EWMA weight for the accumulated epoch
	lrbMergePeriod = 8192
	chbSweepPeriod = 1024 // CHB: global score decay sweep interval (conflicts)
)

// initBranch allocates the per-variable arrays for the chosen mode.
func (s *CDCLSolver) initBranch(mode int) {
	b := &branchHeuristic{mode: mode}
	n := int(s.cnf.NumVars)
	b.scores = make([]float64, n)
	b.heapPos = make([]int, n)
	for i := range b.heapPos {
		b.heapPos[i] = -1
	}
	if mode == branchCHB {
		b.last = make([]uint64, n)
	} else if mode == branchLRB {
		b.conf = make([]uint64, n)
	}
	s.branch = b
}

// buildHeap rebuilds the branch-score max-heap over all variables from their
// current scores (assigned variables are included at their real score; they are
// sunk lazily at selection). Called when invalid or on first use.
func (b *branchHeuristic) buildHeap(assignments []Assignment) {
	b.varIdxs = b.varIdxs[:0]
	for i := range b.scores {
		score := b.scores[i]
		if assignments[i].Level >= 0 {
			score = math.Inf(-1)
		}
		// append then sift up to keep the heap valid during construction
		idx := len(b.varIdxs)
		b.varIdxs = append(b.varIdxs, uint32(i))
		b.heapPos[i] = idx
		b.siftUp(b.heapPos, idx, score, uint32(i))
	}
	b.heapValid = true
}

// siftUp/siftDown restore the max-heap property starting at position `i`.
// Tie-break: equal scores prefer the lower variable index (deterministic).
func (b *branchHeuristic) siftUp(heapPos []int, i int, score float64, v uint32) {
	for i > 0 {
		p := (i - 1) / 2
		ps := b.scores[b.varIdxs[p]]
		pv := b.varIdxs[p]
		if score < ps || (score == ps && v >= pv) {
			break
		}
		b.varIdxs[i] = pv
		heapPos[pv] = i
		i = p
		score = ps
		v = pv
	}
	b.varIdxs[i] = v
	heapPos[v] = i
}

func (b *branchHeuristic) siftDown(heapPos []int, i int) {
	n := len(b.varIdxs)
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		largest := l
		ls := b.scores[b.varIdxs[l]]
		lsv := b.varIdxs[l]
		if r := l + 1; r < n {
			rs := b.scores[b.varIdxs[r]]
			rsv := b.varIdxs[r]
			if rs > ls || (rs == ls && rsv < lsv) {
				largest = r
				ls = rs
				lsv = rsv
			}
		}
		cs := b.scores[b.varIdxs[i]]
		cv := b.varIdxs[i]
		if cs > ls || (cs == ls && cv <= lsv) {
			break
		}
		b.varIdxs[i] = lsv
		heapPos[lsv] = i
		i = largest
		b.varIdxs[i] = cv
		heapPos[cv] = i
	}
}

// removeMax drops the root (used when a var index is out of range).
func (b *branchHeuristic) removeMax(heapPos []int) {
	n := len(b.varIdxs)
	root := b.varIdxs[0]
	heapPos[root] = -1
	if n > 1 {
		last := b.varIdxs[n-1]
		b.varIdxs[0] = last
		heapPos[last] = 0
		b.varIdxs = b.varIdxs[:n-1]
		b.siftDown(heapPos, 0)
	} else {
		b.varIdxs = b.varIdxs[:0]
	}
}

// selectVariable returns the unassigned variable with the highest branch score.
// Assigned variables encountered at the root are sunk to -Inf; an unassigned
// root is always the max because bumps sift and selection re-sifts after any
// score change at the root.
func (s *CDCLSolver) branchSelect(assignments []Assignment) uint32 {
	b := s.branch
	if b == nil || len(b.scores) == 0 {
		return 0
	}
	if !b.heapValid || len(b.varIdxs) == 0 {
		b.buildHeap(assignments)
	}
	n := len(b.varIdxs)
	for len(b.varIdxs) > 0 {
		// Safety: never iterate more than once per heap entry (guards against
		// the all-assigned case where every root keeps coming back as -Inf).
		if n < 0 {
			break
		}
		n--
		v := b.varIdxs[0]
		if int(v) >= len(assignments) {
			b.removeMax(b.heapPos)
			continue
		}
		if assignments[v].Level >= 0 {
			// assigned: sink to -Inf and restore the max-at-root property.
			b.scores[v] = math.Inf(-1)
			b.siftDown(b.heapPos, 0)
			continue
		}
		// Unassigned: restore any sunk score in place, then re-sift so the true
		// max rises (also correct for CHB's score-decreasing bumps).
		real := b.scores[v]
		if real == math.Inf(-1) {
			b.scores[v] = real
			b.siftDown(b.heapPos, 0)
			continue
		}
		return v
	}
	// Fallback: linear scan.
	best := uint32(0)
	bestScore := math.Inf(-1)
	for i := range b.scores {
		if assignments[i].Level < 0 && b.scores[i] > bestScore {
			bestScore = b.scores[i]
			best = uint32(i)
		}
	}
	return best
}

// branchBump is called once per conflict on the variables touched by conflict
// analysis (CHB) or the learned clause (LRB). It advances the running counters
// and re-sifts the affected heap entries.
func (s *CDCLSolver) branchBump(vars []uint32) {
	b := s.branch
	if b == nil || b.mode == branchVSIDS {
		return
	}
	if b.mode == branchCHB {
		b.step++
		if b.step%chbSweepPeriod == 0 {
			// Global time-decay: inactive variables lose score over time so a
			// variable never bumped still eventually drops below active ones.
			// Multiplying every score by a constant PRESERVES their relative
			// order (heap stays valid); crucially `last` is NOT reset, so the
			// recency gap step-last[v] remains meaningful across sweeps.
			for i := range b.scores {
				b.scores[i] *= chbDecayBase
			}
			b.heapValid = false // all scores changed; rebuild on next selection
		}
		for _, v := range vars {
			if int(v) >= len(b.scores) {
				continue
			}
			gap := b.step - b.last[v]
			decay := math.Pow(chbDecayBase, float64(gap))
			b.scores[v] = decay*b.scores[v] + (1 - decay)
			b.last[v] = b.step
		}
	} else { // LRB
		b.epochCount++
		for _, v := range vars {
			if int(v) < len(b.conf) {
				b.conf[v]++
			}
		}
		if b.epochCount >= lrbMergePeriod {
			for i := range b.scores {
				b.scores[i] = lrbEwmaAlpha*b.scores[i] + float64(b.conf[i])
				b.conf[i] = 0
			}
			b.epochCount = 0
			b.heapValid = false // all scores changed
		}
	}
	// Re-sift bumped entries that are present in the heap (their score changed).
	// For CHB the bump may increase or decrease; sift both directions covers it.
	// When a global sweep invalidated the heap, skip (rebuilt at next selection).
	if b.mode == branchCHB && b.heapValid {
		for _, v := range vars {
			if int(v) < len(b.heapPos) && b.heapPos[v] >= 0 {
				pos := b.heapPos[v]
				b.siftUp(b.heapPos, pos, b.scores[v], v)
				b.siftDown(b.heapPos, pos)
			}
		}
	}
}

// branchOnUnassign restores a score that was sunk to -Inf while the var was
// assigned, so the next selection finds it. Only needs to fix entries that are
// present and sunk; the lazy selection path also self-heals.
func (s *CDCLSolver) branchOnUnassign(v uint32) {
	b := s.branch
	if b == nil || b.mode == branchVSIDS {
		return
	}
	if int(v) >= len(b.heapPos) || b.heapPos[v] < 0 {
		return
	}
	if b.scores[v] == math.Inf(-1) {
		pos := b.heapPos[v]
		b.siftUp(b.heapPos, pos, b.scores[v], v)
	}
}

// branchInvalidate marks the branch heap as stale (called on restart / level-0
// resets where many variables are unassigned at once).
func (s *CDCLSolver) branchInvalidate() {
	if s.branch != nil && s.branch.mode != branchVSIDS {
		s.branch.heapValid = false
	}
}

// branchSeedFromVSIDS copies the fully-initialized VSIDS activity into the
// branch scores so the alternative heuristic starts from the SAME structure-
// aware activity as VSIDS (clause-length + occurrence + optional init noise).
// This makes the A/B a clean comparison of the update/direction rule only.
func (s *CDCLSolver) branchSeedFromVSIDS() {
	b := s.branch
	if b == nil || b.mode == branchVSIDS {
		return
	}
	// Scale the structure-weighted activity into [0,1]. CHB scores live on an
	// absolute ~[0,1] EWMA scale; seeding with raw VSIDS magnitudes (hundreds)
	// would let never-bumped variables dominate forever (no time-decay reaches
	// them). LRB uses the same scale for consistency.
	max := 0.0
	for _, a := range s.vsids.activity {
		if a > max {
			max = a
		}
	}
	if max > 0 {
		for i, a := range s.vsids.activity {
			b.scores[i] = a / max
		}
	} else {
		for i := range b.scores {
			b.scores[i] = 0.001
		}
	}
	for i := range b.last {
		b.last[i] = 0
	}
	for i := range b.conf {
		b.conf[i] = 0
	}
	b.heapValid = false
}

// SetBranch selects the variable-selection heuristic: branchVSIDS (default),
// branchCHB, or branchLRB. Must be called before Solve and is the no-op default.
func (s *CDCLSolver) SetBranch(mode int) {
	if mode == branchVSIDS {
		s.branch = nil
		return
	}
	s.initBranch(mode)
}
