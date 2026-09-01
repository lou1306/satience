package solver

import (
	"satience/internal/cnf"
	"testing"
)

func TestInprocessDue_TriggerAGate(t *testing.T) {
	c := cnf.CNF{NumVars: 4, NumClauses: 1}
	s := NewCDCLSolver(&c)
	s.SetInprocess(1000, 1000000) // enable, initial gap 1000
	s.inprocessGapCur = 1000
	s.conflictsAtLastInprocess = 0

	// Gap elapsed but no new units -> NOT due (trigger A blocks).
	s.conflicts = 1001
	s.rootUnitsLearned = 10
	s.unitsAtLastInprocess = 0
	if s.inprocessDue() {
		t.Fatal("expected NOT due: gap elapsed but new units (10) < minUnits (16)")
	}

	// Gap elapsed AND enough new units -> due.
	s.rootUnitsLearned = 20
	if !s.inprocessDue() {
		t.Fatal("expected due: gap elapsed and new units (20) >= minUnits (16)")
	}

	// Gap NOT elapsed -> not due even with many units.
	s.conflicts = 500
	if s.inprocessDue() {
		t.Fatal("expected NOT due: conflict gap not elapsed")
	}

	// Excluded -> never due.
	s.inprocessExcluded = true
	s.conflicts = 5000
	if s.inprocessDue() {
		t.Fatal("expected NOT due: instance excluded")
	}

	// Disabled -> never due.
	s.inprocessExcluded = false
	s.SetInprocess(0, 1000000)
	if s.inprocessDue() {
		t.Fatal("expected NOT due: in-processing disabled (period 0)")
	}
}

func TestInprocessDue_AdaptiveGapUsesGapCur(t *testing.T) {
	c := cnf.CNF{NumVars: 4, NumClauses: 1}
	s := NewCDCLSolver(&c)
	s.SetInprocess(1000, 1000000)
	s.inprocessGapCur = 1000
	s.conflictsAtLastInprocess = 100
	s.rootUnitsLearned = 100
	s.unitsAtLastInprocess = 50 // 50 new units

	// At conflicts-1000 gap=1000: not yet (100 > 1000-100... i.e. conflicts < last+gap).
	s.conflicts = 1000
	if s.inprocessDue() {
		t.Fatal("expected NOT due yet (conflicts 1000 < last 100 + gap 1000 = 1100)")
	}
	// Adaptive tightening: once gap is halved to 500, same conflicts become due.
	s.inprocessGapCur = 500
	if !s.inprocessDue() {
		t.Fatal("expected due after gap tightened to 500 (1000 >= 100+500)")
	}
}
