package solver

import (
	"satience/internal/cnf"
	"testing"
)

func govTestSolver() *CDCLSolver {
	s := NewCDCLSolver(&cnf.CNF{NumVars: 4, NumClauses: 0})
	s.structureScore = 0.8
	s.polarityImbalance = 0.2
	s.longClauseRatio = 0.3
	s.geometricRestarts = false
	s.restartBase = 200
	return s
}

// fireDet4 runs detector4 winReq times at winLBD (building ring history) with
// glue=0 so the stagnation predicate is exercised, then returns the solver.
func fireDet4(s *CDCLSolver, winLBD float64, times int) {
	for i := 0; i < times; i++ {
		s.detector4LbdStagnation(winLBD, 0.0)
	}
}

func TestGovDefaultRejectsHighPolImb(t *testing.T) {
	// Default relaxed bounds (=hard): PolImb>=0.4 is still a hard reject.
	s := govTestSolver()
	s.polarityImbalance = 0.5
	before := s.restartBase
	fireDet4(s, 20.0, 6)
	if s.restartBase != before {
		t.Fatalf("default: high PolImb should be rejected; base changed %d->%d", before, s.restartBase)
	}
}

func TestGovDefaultRejectsLowStruct(t *testing.T) {
	s := govTestSolver()
	s.structureScore = 0.6
	before := s.restartBase
	fireDet4(s, 20.0, 6)
	if s.restartBase != before {
		t.Fatalf("default: structureScore<0.7 should be rejected; base changed")
	}
}

func TestGovRelaxedBandFiresOnSevere(t *testing.T) {
	s := govTestSolver()
	s.govStagRelaxPolImb = 0.6 // open band [0.4,0.6)
	s.polarityImbalance = 0.5  // inside band -> relaxed
	s.SetGovernorSeverity(4.0, 1)
	fireDet4(s, 20.0, 5)
	if s.restartBase != s.govStagBase {
		t.Fatalf("relaxed+severe should fire (one-shot -> base %d); got %d", s.govStagBase, s.restartBase)
	}
}

func TestGovRelaxedBandNoFireMild(t *testing.T) {
	s := govTestSolver()
	s.govStagRelaxPolImb = 0.6
	s.polarityImbalance = 0.5 // relaxed
	s.SetGovernorSeverity(4.0, 1)
	before := s.restartBase
	fireDet4(s, 13.0, 5) // 13 < bar(12+4=16) -> not high enough for relaxed band
	if s.restartBase != before {
		t.Fatalf("relaxed but mild LBD should NOT fire; base changed")
	}
}

func TestGovGraduatedStepsNotJumps(t *testing.T) {
	s := govTestSolver()
	s.SetGovernorGraduated(true, 2.0, 3)
	// graduation: govStagWin=3 -> fires on the 3rd window (200/2=100), exactly once.
	fireDet4(s, 20.0, 3)
	if s.restartBase != 100 {
		t.Fatalf("graduated should step to 100, got %d", s.restartBase)
	}
	if !s.govSaveBaseSet || s.govSaveBase != 200 {
		t.Fatalf("graduated should save original base 200; save=%d set=%v", s.govSaveBase, s.govSaveBaseSet)
	}
}

func TestGovGraduatedRecovery(t *testing.T) {
	s := govTestSolver()
	s.SetGovernorGraduated(true, 2.0, 3)
	fireDet4(s, 20.0, 3) // one fire: restartBase -> 100, save=200
	// 3 improving windows -> back off 100 -> 200 (full recovery, clear save)
	for i := 0; i < 3; i++ {
		s.detector4Recovery(5.0, 0.02)
	}
	if s.restartBase != 200 {
		t.Fatalf("graduated should recover to 200 after 3 improving windows; got %d", s.restartBase)
	}
	if s.govSaveBaseSet {
		t.Fatalf("after full recovery, saved base should be cleared")
	}
}

func TestGovDefaultLegacyOneShot(t *testing.T) {
	// Legacy (graduated off): a normal-band fire jumps straight to govStagBase.
	s := govTestSolver()
	fireDet4(s, 20.0, 4)
	if s.restartBase != s.govStagBase {
		t.Fatalf("legacy one-shot should jump to base %d, got %d", s.govStagBase, s.restartBase)
	}
}
