package solver

// Assignment tracks variable assignments.
// Packed to 8 bytes (Level int32 + Value bool) so a single cache-line load
// fetches both fields. This replaces the former separate varLevel []int cache
// (which caused a second cache miss on every variable lookup in the
// propagation hot path) and halves the per-variable memory footprint.
type Assignment struct {
	Level int32
	Value bool // true = positive, false = negative
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
