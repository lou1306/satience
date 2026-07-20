package solver

// claDecay implements O(1) clause activity decay (mirrors the VSIDS varInc trick
// in vsids.go:442). Instead of scaling all Activity fields by claDecayFactor
// (O(n) scan over learnedMetadata), we grow claInc: claInc /= claDecayFactor.
// Bumps add claInc, so recent bumps are naturally larger than old ones. The
// activity field is only consulted at deletion time (deleteLearnedClauses), so
// there is no heap to invalidate — no per-conflict selection uses it.
//
// Periodic rescaling prevents claInc from overflowing float64 precision. Fires
// rarely (~every 500K conflicts). Scales all Activity fields and claInc by the
// same factor, preserving relative ordering. Tombstone slots may carry stale
// Activity values but compaction reclaims them, so the scan stays bounded.
func (s *CDCLSolver) claDecay() {
	if !s.claActivityEnabled {
		return
	}
	s.claInc /= s.claDecayFactor
	if s.claInc > 1e100 {
		scale := 1.0 / s.claInc
		for i := range s.learnedMetadata {
			s.learnedMetadata[i].Activity *= scale
		}
		s.claInc = 1.0
	}
}
