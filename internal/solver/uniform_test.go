package solver

import (
	"satience/internal/cnf"
	"testing"
)

// TestUniformDefaults_NeutralizeClassifier verifies that SetUniformDefaults(true)
// forces every per-instance classifier bifurcation to a fixed value AND that
// classifyInstance does not overwrite them back.
func TestUniformDefaults_NeutralizeClassifier(t *testing.T) {
	// Dense binary-heavy instance: would set skipBVE=true, skipPolarityPhase=true
	// via classifyInstance.
	c := cnf.CNF{NumVars: 200, NumClauses: 0}
	for i := 0; i < 2500; i++ {
		a := uint32(i * 7 % 200)
		b := uint32(i*11%199 + 1)
		c.AddClause([]cnf.Literal{
			cnf.NewLiteral(a, i%2 == 0),
			cnf.NewLiteral(b, i%3 == 0),
		}, false)
	}
	s := NewCDCLSolver(&c)
	s.SetUniformDefaults(true)

	// Setter must neutralize all gates to deterministic fixed values.
	if s.skipBVE {
		t.Fatal("uniform: skipBVE must be false")
	}
	if s.skipPolarityPhase {
		t.Fatal("uniform: skipPolarityPhase must be false")
	}
	if s.skipSubsumption {
		t.Fatal("uniform: skipSubsumption must be false")
	}
	if s.inprocessExcluded {
		t.Fatal("uniform: inprocessExcluded must be false")
	}
	if s.useBumpAnalyze {
		t.Fatal("uniform: useBumpAnalyze must be false (bumpClause-only)")
	}
	if !s.useBumpAnalyzeOverride {
		t.Fatal("uniform: useBumpAnalyzeOverride must be set so classifier cannot revert")
	}
	if s.subsumptionPeriod != 100 || !s.subsumptionPeriodSet {
		t.Fatal("uniform: fixed subsumption period 100 must be locked")
	}
	if !s.lbdScaleOverride {
		t.Fatal("uniform: lbdScaleOverride must be set (fixed LBD scale)")
	}

	// classifyInstance recomputes structure and would normally set skipBVE=true
	// for this dense-binary instance; under uniform it must not revert the gates.
	s.classifyInstance()
	if s.skipBVE {
		t.Fatal("uniform: classifyInstance reverted skipBVE to true")
	}
	if s.skipPolarityPhase {
		t.Fatal("uniform: classifyInstance reverted skipPolarityPhase to true")
	}

	// Preprocessing config: uniform always enables unit-prop single pass, even
	// for a "random-like" structure that would otherwise disable preprocessing.
	s.structureScore = 0.3
	s.binaryRatio = 0.3
	cfg := s.getAdaptivePreprocessingConfig()
	if !cfg.EnableUnitProp {
		t.Fatal("uniform: preprocessing EnableUnitProp must always be true")
	}
	if cfg.MaxPasses != 1 {
		t.Fatalf("uniform: MaxPasses must be 1, got %d", cfg.MaxPasses)
	}
}

// TestUniformDefaults_KeepBVEIsolatesOneGate verifies that combining
// -uniform with uniformKeepBVE re-enables ONLY the dense-binary skipBVE gate
// while leaving every other classifier bifurcation neutralized.
func TestUniformDefaults_KeepBVEIsolatesOneGate(t *testing.T) {
	c := cnf.CNF{NumVars: 200, NumClauses: 0}
	for i := 0; i < 2500; i++ {
		a := uint32(i * 7 % 200)
		b := uint32(i*11%199 + 1)
		c.AddClause([]cnf.Literal{
			cnf.NewLiteral(a, i%2 == 0),
			cnf.NewLiteral(b, i%3 == 0),
		}, false)
	}
	s := NewCDCLSolver(&c)
	s.SetUniformDefaults(true)
	s.SetUniformKeepBVE(true)

	if !s.uniformDefaults {
		t.Fatal("uniformDefaults must be on")
	}
	s.classifyInstance() // dense-binary (density 12.5, binRatio 1.0) => skipBVE re-derived

	if !s.skipBVE {
		t.Fatal("keep-bve: skipBVE must be re-derived true on dense-binary instance")
	}
	if !s.inprocessExcluded {
		t.Fatal("keep-bve: inprocessExcluded must follow skipBVE")
	}
	if s.skipPolarityPhase {
		t.Fatal("keep-bve: skipPolarityPhase must stay neutralized (false)")
	}
	if s.skipSubsumption {
		t.Fatal("keep-bve: skipSubsumption must stay neutralized (false)")
	}
	if s.useBumpAnalyze {
		t.Fatal("keep-bve: useBumpAnalyze must stay neutralized (false)")
	}
	if s.subsumptionPeriod != 100 || !s.subsumptionPeriodSet {
		t.Fatal("keep-bve: subsumption period must stay fixed at 100")
	}
}

// TestUniformSecondary_NeutralizesOnlySecondaryRules verifies that
// -uniform-secondary locks OFF only the secondary rules while keeping the
// dense-binary/long-clause gates classified.
func TestUniformSecondary_NeutralizesOnlySecondaryRules(t *testing.T) {
	c := cnf.CNF{NumVars: 200, NumClauses: 0}
	for i := 0; i < 2500; i++ {
		a := uint32(i * 7 % 200)
		b := uint32(i*11%199 + 1)
		c.AddClause([]cnf.Literal{
			cnf.NewLiteral(a, i%2 == 0),
			cnf.NewLiteral(b, i%3 == 0),
		}, false)
	}
	s := NewCDCLSolver(&c)
	s.SetUniformSecondary(true)

	if !s.uniformSecondary {
		t.Fatal("uniformSecondary must be on")
	}
	if s.subsumptionPeriod != 100 || !s.subsumptionPeriodSet {
		t.Fatal("secondary: subsumption period must be locked at 100")
	}
	if !s.useBumpAnalyzeOverride || s.useBumpAnalyze {
		t.Fatal("secondary: useBumpAnalyze must be locked off (bumpClause-only)")
	}
	if s.lbdScaleOverride {
		t.Fatal("secondary: adaptive LBD scale must stay classified (no lbdScaleOverride)")
	}

	s.classifyInstance() // dense-binary -> dense gate (skipBVE) remains classified
	if !s.skipBVE {
		t.Fatal("secondary: skipBVE must stay classified true on dense-binary")
	}
	if s.subsumptionPeriod != 100 {
		t.Fatal("secondary: subsumption period must stay 100 (not reverted to 50)")
	}

	// Preprocessing always on under secondary (random-like disable removed).
	s.structureScore = 0.3
	s.binaryRatio = 0.3
	cfg := s.getAdaptivePreprocessingConfig()
	if !cfg.EnableUnitProp {
		t.Fatal("secondary: preprocessing EnableUnitProp must be true")
	}
}

// TestUniformDefaults_DisabledIsNoop verifies the default (flag off) leaves the
// classifier free to set the per-instance gates.
func TestUniformDefaults_DisabledIsNoop(t *testing.T) {
	c := cnf.CNF{NumVars: 200, NumClauses: 0}
	for i := 0; i < 2500; i++ {
		a := uint32(i * 7 % 200)
		b := uint32(i*11%199 + 1)
		c.AddClause([]cnf.Literal{
			cnf.NewLiteral(a, i%2 == 0),
			cnf.NewLiteral(b, i%3 == 0),
		}, false)
	}
	s := NewCDCLSolver(&c) // uniform defaults off
	if s.uniformDefaults {
		t.Fatal("uniformDefaults must default to false")
	}
	s.classifyInstance()
	if !s.skipBVE {
		t.Fatal("classifier must still set skipBVE=true when uniform is off")
	}
}
