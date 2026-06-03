package solver

// LubyRestarts implements Luby's restart strategy
// Sequence: 1, 1, 2, 1, 1, 2, 4, 1, 1, 2, 1, 1, 2, 4, 8, ...
type LubyRestarts struct {
	unit        int // Base unit for restarts (conflicts between restarts)
	conflicts   int // Conflicts since last restart
	restartNum  int // Current restart number
}

// NewLubyRestarts creates a new Luby restart strategy
func NewLubyRestarts(unit int) *LubyRestarts {
	return &LubyRestarts{
		unit:      unit,
		conflicts: 0,
		restartNum: 1,
	}
}

// AddConflict records a conflict and returns true if restart is needed
func (l *LubyRestarts) AddConflict() bool {
	l.conflicts++
	target := l.luby(l.restartNum) * l.unit
	
	if l.conflicts >= target {
		l.conflicts = 0
		l.restartNum++
		return true
	}
	return false
}

// luby returns the i-th value in the Luby sequence
func (l *LubyRestarts) luby(i int) int {
	// Find the largest k such that 2^k - 1 < i
	k := 1
	for (1 << k) - 1 < i {
		k++
	}
	k--
	
	// Calculate position in the sequence
	base := (1 << k) - 1
	pos := i - base - 1
	
	if pos == 0 {
		return 1 << k
	}
	
	return l.luby(pos)
}

// Reset resets the restart counter
func (l *LubyRestarts) Reset() {
	l.conflicts = 0
	l.restartNum = 1
}
