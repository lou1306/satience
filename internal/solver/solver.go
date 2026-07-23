package solver

// Assignment tracks variable assignments, reason clause, and saved phase.
// Packed to 12 bytes (Level int32 + Reason int32 + Value bool + SavedPhase bool)
// so a single cache-line load fetches Level and Reason together. This replaces
// the former separate implication []int32 and savedPhase []bool arrays, which
// each caused additional cache-line writes on every assign/unassign and an
// extra cache-line read on the propagation hot path (reason check).
type Assignment struct {
	Level      int32
	Reason     int32 // >=0 original; <=-5 learned (-learnedIdx-5); -1 decision; -2 unit-prop preprocess; -3 pure-literal; -4 reserved
	Value      bool  // true = positive, false = negative
	SavedPhase bool  // phase saving for next decision
}

// LearnedClauseLoc packs a learned clause's offset and size into 8 bytes so a
// single load fetches both fields. Replaces the former separate
// learnedOffsets/learnedSizes arrays (16 bytes across two allocations, often
// on different cache lines). int32 is safe: offsets max out at ~2M literals
// (100K clauses * ~20 lits) << int32 limit (2.1B), and sizes <= variable count.
type LearnedClauseLoc struct {
	Offset int32 // Start offset in learnedLiterals
	Size   int32 // Number of literals (0 = deleted/tombstone)
}
