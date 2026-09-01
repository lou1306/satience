package solver

import (
	"fmt"
	"os"
)

// Instance-structure classification and adaptive-search parameter selection
// that runs once before the CDCL search. Split out of solver_cdcl.go for
// structure; same CDCLSolver state, no behavior change.

// getAdaptivePreprocessingConfig returns preprocessing config based on the
// cached structureScore (set by classifyInstance, which must have run first).
// Since the -uniform-secondary fresh-rail 50-draw heldout validated that the
// random-like preprocessing disable and the density/50K rescue are
// distributionally neutral, preprocessing is now ALWAYS the standard unit-prop
// single-pass config (all former branches returned this same value except the
// random-like disable, which is removed). The dense-binary skipBVE /
// skipPolarityPhase / skipSubsumption gates in preprocessAggressive still gate
// individual techniques independently.
func (s *CDCLSolver) getAdaptivePreprocessingConfig() PreprocessingConfig {
	return PreprocessingConfig{
		EnableUnitProp: true,
		MaxPasses:      1,
	}
}

// classifyInstance analyzes the formula structure and caches classifier output
// (structureScore, polarityImbalance, longClauseRatio, skipPolarityPhase,
// skipBVE) plus sets search parameters (VSIDS decay, restartBase, Glucose
// restart gating) that depend only on instance structure — NOT on the
// preprocessing decision.
//
// This is split from getAdaptivePreprocessingConfig so that the search-parameter
// tuning is available even when preprocessing is skipped (SolveWithoutPreprocessing).
// Previously -no-preprocess skipped BOTH preprocessing AND adaptive tuning,
// making it a polluted diagnostic axis. classifyInstance is read-only on s.cnf.
//
// Must be called once, before search begins, in every solve entry point that
// runs the CDCL loop (SolveWithResult, SolveWithoutPreprocessing).
func (s *CDCLSolver) classifyInstance() {
	structure := s.analyzeInstanceStructure()

	// Cache classifier output for downstream consumers (initVSIDSOccurrenceBonus
	// gates the polarity-based initial phase on these metrics; the level-capped
	// restart gate reads longClauseRatio; the governor gates on binaryRatio and
	// structureScore).
	s.structureScore = structure.StructuredScore
	s.polarityImbalance = structure.PolarityImbalance
	s.longClauseRatio = structure.LongClauseRatio
	s.binaryRatio = structure.BinaryRatio

	// Dense binary instances: the BIG is highly connected, so the default phase
	// propagates to a solution quickly (0 conflicts observed on 32baec6a, a
	// 2500v/49-density/100%-binary instance). The occurrence-based polarity
	// override fights the implication structure and sends the search into
	// thousands of conflicts. Skip it. Sparse binary instances (e.g. 8202af80,
	// density 24.5) still benefit from the override, so the density threshold
	// (35) separates the two regimes. Under -uniform (uniformDefaults) these
	// per-instance gates are locked off: everything runs the un-gated path.
	if !s.uniformDefaults {
		s.skipPolarityPhase = structure.BinaryRatio > 0.9 && structure.Density > 35.0
	}

	// BVE is pure overhead on dense binary instances: it hits the resolvent
	// budget eliminating only 7-14% of variables while spending 4-15s on
	// resolvent generation + post-BVE rebuild. The highly-connected BIG is
	// navigated in <2s by watch-based propagation alone. 8202af80 (density
	// 24.5, 99.8% binary): 16.7s→1.4s. bb34f22f (density 3.12, 67% binary) is
	// NOT gated (density ≤ 10) — it's the instance VE was tuned for.
	// The gate is the ONE classifier decision kept under -uniform when
	// uniformKeepBVE is set, to isolate its distributional value.
	if !s.uniformDefaults || s.uniformKeepBVE {
		s.skipBVE = structure.BinaryRatio > 0.95 && structure.Density > 10.0
		// In-processing classifier: in-process BVE has HIGH yield on dense-binary
		// instances yet is destructive to search there (de2b: +2246 eliminations
		// but TMO; same class as skipBVE). Exclude the dense-binary/BVE-hostile
		// class outright so even an enabled in-processing never runs on them.
		s.inprocessExcluded = s.skipBVE
	}

	if !s.uniformDefaults {
		// Subsumption is O(clauses × occurrences × clause-length). On very dense
		// instances (density > 60) the occurrence lists are huge, making each pass
		// take seconds while the instance often solves in <0.1s without it.
		// ramlb_6_6 (density 149): subsumption 5s, search 0.03s. kcliquebin_6
		// (density 298): subsumption 2s, search 0.01s. ramlb_5_5 (density 74):
		// subsumption 0.4s, search 0.01s. The threshold of 60 is above the
		// highest-density suite instance (32baec6a, density 49).
		s.skipSubsumption = structure.Density > 60.0
	}

	// NOTE: The former size-adaptive subsumption-period (<500 vars -> 50) and
	// the useBumpAnalyze gate (analyze_toclear for random/low-structure families)
	// were REMOVED after a fresh-rail 50-draw heldout showed they are
	// distributionally neutral (variant -uniform-secondary PASSED all 25
	// families with no new TMO and no median regression). Subsumption period
	// stays at the CLI/default 100; useBumpAnalyze stays off (bumpClause-only)
	// unless forced via -ms-analyze. This removes two overfit magic knobs.

	// Classifier telemetry: emit every structural gate decision on stderr so the
	// audit (and future regression checks) can read all decisions at once,
	// independent of -verbose / -stats. Must stay read-only on s.cnf (as the rest
	// of classifyInstance) and not perturb the search.
	preproc := "on"
	if s.structureScore < 0.7 && s.binaryRatio <= 0.5 &&
		!(s.structuredDensityGate > 0 && s.cnf.NumVars > 0 && float64(s.cnf.NumClauses)/float64(s.cnf.NumVars) >= s.structuredDensityGate) &&
		int(s.cnf.NumVars) <= s.preprocessingMaxVars {
		preproc = "off"
	}
	flp := "off"
	if s.structureScore < 0.7 && s.binaryRatio <= 0.5 {
		flp = "on"
	}
	big := true
	if s.structureScore < 0.7 && s.binaryRatio > 0.4 {
		big = false
	}
	lbdScale := 200000.0 * s.binaryRatio / float64(s.cnf.NumVars)
	if lbdScale < 10.0 {
		lbdScale = 10.0
	}
	fmt.Fprintf(os.Stderr,
		"c classify: score=%.3f binRatio=%.3f ternary=%.3f long=%.3f density=%.1f imb=%.2f regOcc=%t | preproc=%s bumpAnalyze=%t skipBVE=%t skipPolarity=%t skipSubsumption=%t FLP=%s BIG=%t subperiod=%d lbdScale=%.0f\n",
		s.structureScore, s.binaryRatio, structure.TernaryRatio, s.longClauseRatio,
		structure.Density, s.polarityImbalance, structure.RegularOccurrence,
		preproc, s.useBumpAnalyze, s.skipBVE, s.skipPolarityPhase, s.skipSubsumption,
		flp, big, s.subsumptionPeriod, lbdScale)
}

// analyzeInstanceStructure computes metrics to detect structured vs random instances
// Structured instances (Tseitin, hardware, combinatorial) benefit from aggressive preprocessing
// Random instances benefit from lightweight preprocessing only
func (s *CDCLSolver) analyzeInstanceStructure() InstanceStructure {
	structure := InstanceStructure{}

	// Density: clauses / vars
	if s.cnf.NumVars > 0 {
		structure.Density = float64(s.cnf.NumClauses) / float64(s.cnf.NumVars)
	}

	// Clause size distribution and polarity counts (single pass over clauses).
	binaryCount := 0
	ternaryCount := 0
	longCount := 0
	smallCount := 0
	posCount := make([]int, s.cnf.NumVars)
	negCount := make([]int, s.cnf.NumVars)

	// Track whether long clauses have varied sizes. Random k-SAT (k≥4) has
	// all long clauses the same size; structured instances have varied sizes.
	// Used to penalize longScore for random k-SAT misclassified as structured.
	firstLongSize := 0
	longSizeVaried := false

	for i := 0; i < s.cnf.NumClauses; i++ {
		offset, size := s.cnf.GetOriginalClauseInfo(i)
		if size == 2 {
			binaryCount++
			smallCount++
		} else if size == 3 {
			ternaryCount++
			smallCount++
		} else if size > 3 {
			longCount++
			if firstLongSize == 0 {
				firstLongSize = size
			} else if size != firstLongSize {
				longSizeVaried = true
			}
		}
		lits := s.cnf.GetLiteralPool()[offset : offset+size]
		for _, lit := range lits {
			if lit.IsNegated() {
				negCount[lit.Var()]++
			} else {
				posCount[lit.Var()]++
			}
		}
	}

	if s.cnf.NumClauses > 0 {
		structure.BinaryRatio = float64(binaryCount) / float64(s.cnf.NumClauses)
		structure.TernaryRatio = float64(ternaryCount) / float64(s.cnf.NumClauses)
		structure.LongClauseRatio = float64(longCount) / float64(s.cnf.NumClauses)
		structure.SmallClauseRatio = float64(smallCount) / float64(s.cnf.NumClauses)
	}

	// Polarity imbalance: mean per-variable |pos-neg|/(pos+neg).
	// 0 = perfectly balanced (occurrence-based polarity is noise), 1 = pure
	// (one polarity absent). Used to gate the polarity-based initial phase.
	imbSum := 0.0
	imbCount := 0
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		t := posCount[i] + negCount[i]
		if t > 0 {
			diff := float64(posCount[i] - negCount[i])
			if diff < 0 {
				diff = -diff
			}
			imbSum += diff / float64(t)
			imbCount++
		}
	}
	if imbCount > 0 {
		structure.PolarityImbalance = imbSum / float64(imbCount)
	}

	// Regular-occurrence detection. In a k-regular structured formula every
	// appearing variable occurs exactly the same number of times (identical
	// node degree structure). Random k-SAT (Poisson-distributed occurrences)
	// never has near-equal per-variable counts. This is a high-precision,
	// low-recall signal: it only exempts provably-regular instances, so random
	// families are never touched by the exemption.
	minOcc := -1
	maxOcc := -1
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		n := posCount[i] + negCount[i]
		if n == 0 {
			continue
		}
		if minOcc < 0 {
			minOcc, maxOcc = n, n
		} else {
			if n < minOcc {
				minOcc = n
			}
			if n > maxOcc {
				maxOcc = n
			}
		}
	}
	if minOcc > 0 {
		meanOcc := float64(minOcc+maxOcc) / 2.0
		if float64(maxOcc-minOcc)/meanOcc < 0.02 {
			structure.RegularOccurrence = true
		}
	}

	// Structured score: weighted combination of metrics
	// Key insight: structured instances have EITHER many binary clauses OR many
	// long (>3 literal) clauses with varied sizes. Random k-SAT has uniform clause
	// sizes (all k-literal), zero binary, zero long clauses.
	//
	// Components:
	// 1. Size score (weight 0.6): max(binary ratio, long-clause ratio). Structured
	//    instances score high on at least one; random k-SAT scores 0 on both.
	// 2. Density (weight 0.2): moderate contribution
	// 3. Mixed sizes (weight 0.2): structured instances have varied clause sizes

	binaryScore := structure.BinaryRatio
	longScore := structure.LongClauseRatio
	// Penalize uniform-size long clauses: random k-SAT (k≥4) has all long
	// clauses the same size, which is not "structured". Without this, rand4sat
	// (density 9.8, 100% long, all size 4) scores 0.80 and gets the structured
	// path (restartBase=200, no aggressive decay) — but it needs the random
	// path. Penalizing to 0.3 drops the score to ~0.38, below the 0.70
	// threshold. Structured instances (e.g. 274099073, density 15.6, 99.4%
	// long with VARIED sizes) keep full longScore.
	// EXEMPTION: provably-regular uniform-long families (e.g. Tseitin on a
	// regular graph — all clauses same size AND every variable occurs equally
	// often) are structured parity/theory instances, not random k-SAT. Without
	// the exemption they are misclassified as "random k-SAT k≥4" (wrong decay,
	// preprocessing disabled). RegularOccurrence is high-precision (random
	// instances never satisfy it), so routing these to the structured path is
	// safe and does not reintroduce the rand4sat misclassification.
	if longCount > 0 && !longSizeVaried && !structure.RegularOccurrence {
		longScore *= 0.3
	}
	// Ternary-heavy structured instances (e.g., graph coloring) score low on
	// binary/long signals but are still structured — they need unit propagation
	// and Glucose restarts, not the aggressive random-config decay/restart.
	// Weight at 0.65: pure ternary (0.65) + density + mixed → ~0.75, above the
	// 0.70 threshold. 88% ternary + 11% binary (69d72f81) → 0.74, was 0.66.
	ternaryScore := structure.TernaryRatio * 0.65

	sizeScore := binaryScore
	if longScore > sizeScore {
		sizeScore = longScore
	}
	if ternaryScore > sizeScore {
		sizeScore = ternaryScore
	}

	densityScore := 0.0
	if structure.Density > 0 {
		// Normalize: density of 5+ gets full score
		densityScore = structure.Density / 5.0
		if densityScore > 1.0 {
			densityScore = 1.0
		}
	}

	// Mixed size score: penalize uniform distributions
	// If all clauses are same size (e.g., all ternary), this is 0
	// If mixed (binary + ternary + larger), this approaches 1.0
	mixedSizeScore := 0.0
	if structure.BinaryRatio > 0 && structure.TernaryRatio > 0 {
		// Has both binary and ternary - good mix
		mixedSizeScore = 1.0
	} else if structure.BinaryRatio > 0 && structure.LongClauseRatio > 0 {
		// Binary + long (no ternary) - also structured (e.g., rphp family)
		mixedSizeScore = 0.7
	} else if structure.BinaryRatio > 0 || structure.TernaryRatio > 0 {
		// Has some small clauses but not mixed
		mixedSizeScore = 0.3
	}

	structure.StructuredScore = sizeScore*0.6 + densityScore*0.2 + mixedSizeScore*0.2

	return structure
}
