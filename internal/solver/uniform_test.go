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
		a := uint32(i*7 % 200)
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

// TestUniformDefaults_DisabledIsNoop verifies the default (flag off) leaves the
// classifier free to set the per-instance gates.
func TestUniformDefaults_DisabledIsNoop(t *testing.T) {
	c := cnf.CNF{NumVars: 200, NumClauses: 0}
	for i := 0; i < 2500; i++ {
		a := uint32(i*7 % 200)
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
