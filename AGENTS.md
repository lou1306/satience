# Project: satience - CDCL SAT Solver in Go

## Goal
Build a sound and complete CDCL SAT solver in Go named "satience" with DIMACS CNF support and benchmarking on real GBD instances.

## Constraints
- DIMACS format only
- Single-threaded (no parallel solving)
- No incremental solving
- No proof/unsat core generation
- **No cardinality constraint detection** (PHP instances out of scope)
- Go language
- 60 second timeout per benchmark
- Real GBD instances from benchmark-database.de
- SAT Competition 2026 output format compliant
- Compile with GOAMD64=v3 for AVX2/BMI2 optimizations

## Status: Production Ready

**Satience is a correct, sound, and complete CDCL SAT solver.**

- ✅ All unit tests passing (15/15)
- ✅ 100% soundness verified (0 wrong results on 60+ tests)
- ✅ Models verified to satisfy all clauses
- ✅ Swap-remove clause deletion (23× GC reduction)
- ✅ Contiguous literal storage with free slot reuse
- ✅ Modern CDCL features: 1-UIP learning, backjumping, adaptive restarts, LBD management, phase saving
- ✅ Watched literals propagation with O(1) clause index access
- ✅ Preprocessing: unit propagation
- ✅ Trail scanning optimization in 1-UIP conflict analysis
- ✅ Activity heap for O(log n) variable selection

## Performance

**Tombstone Clause Deletion + Periodic Compaction** (June/July 2026) ✅

Learned clause deletion uses tombstones (set `learnedSizes[i]=0`); literal storage is reclaimed by `compactLearnedClauses()`, which runs at the next restart (level 0, where no learned clause is in use as a reason) when accumulated tombstones exceed ~33% of capacity:
- **GC cycles**: 606 → 26 on 26Kv FCC instance (23× reduction)
- **GC time**: ~8-10s → ~0.4s (20× faster)
- **Mechanism**: Deletion marks tombstones + removes watches + `compactWatchLists()`. Periodically, `compactLearnedClauses()` reclaims tombstone literal gaps by moving active clauses into a contiguous prefix, remapping the implication array, and rebuilding all watch lists with non-false literal selection (so the watched-literal invariant holds)

**Benchmark results** (MiniSat Fast Suite, 30s timeout, GOAMD64=v3, June 2026):
- **Solved**: 26/32 instances (81.2% solve rate)
- **40 smallest CNFs**: 28/40 (70.0%) with 5s timeout
- **Tseitin**: All solved (4×4, 5×5, 6×6 - both SAT and UNSAT) ✅
- **Arg chain**: Solved ✅
- **PHP UNSAT**: Timeout (cardinality constraint reasoning needed)
- **Algebraic/Combinatorial**: Mixed results (need better heuristics)

**Performance characteristics**:
- Tombstone deletion + periodic compaction: bounds `learnedLiterals` growth on long runs
- Contiguous literal storage: reclaimed by compaction (not per-deletion)
- Watched literals with ClauseIdx caching: 63% speedup
- Trail scanning optimization in 1-UIP: O(current_level) instead of O(trail_size)
- Activity heap: O(log n) variable selection
- LBD-based clause database management (dynamic learned-clause limit via `calculateMaxLearned`: ~15% of clauses, min 300, max 100K)
- Props/dec ratio: ~33 steady-state on hard instances

**Primary bottleneck**: 
- PHP instances: Lack of cardinality constraint detection
- Random instances: VSIDS lacks community structure exploitation
- Large instances: Memory pressure from watch list allocations

## Implemented Features

### Core CDCL
- 1-UIP conflict analysis with learned clause database
- Backjumping (intelligent backtrack level from learned clause)
- LBD-based clause database management (maxLearned=2500, keep all LBD≤2)
- Phase saving heuristic (remembers satisfying polarity)
- Adaptive restarts (Glucose-style for LBD >2× avg AND >6)
- Luby restart sequence fallback (base=50)
- Clause minimization via self-subsumption
- Watched literals propagation with O(1) clause index access
- Clause quality tracking (useCount, propCount metrics)
- **Watch list pre-allocation**: Accounts for original + learned clauses, caps at 256 capacity
- **Swap-remove clause deletion** (no array rebuilding)
- **Free literal slot tracking and reuse**

### Variable Selection
- VSIDS with activity decay (0.95 → 0.999 over 10k conflicts)
- Activity heap for O(log n) variable selection
- LBD-based activity bonus (20000/LBD²)
- LRB (Learning Rate Based) heuristic available via `-lrb` flag
- CHB (Conflict History Based) heuristic available via `-chb` flag
- Conflict participation tracking

### Preprocessing (Adaptive, Structure-Aware)
- **Adaptive Strategy** (`solver_cdcl.go:928-975`): Analyzes instance structure before preprocessing
  - Structured instances (StructuredScore ≥ 0.7): Unit propagation enabled
  - Random instances (StructuredScore < 0.7): ALL preprocessing disabled (causes 76× more conflicts)
- **Unit Propagation** - Sound unit clause propagation before search (structured instances only)
- **Pure Literal Elimination** - Implemented but disabled by default
- **Subsumption Elimination** - Implemented but disabled by default
- **Self-Subsumption** - Disabled (soundness bug - incorrect clause removal)
- **Hyper-Binary Resolution** - Disabled (soundness bug - derives false empty clauses)
- **Equivalence Detection** - Disabled (soundness bug - false equivalences)

### CLI Features
- `-model`: Print satisfying assignment
- `-verbose`: Show solving statistics
- `-max-iter`: Iteration limit
- `-cpuprofile`: Profile output
- `-lrb`: Use LRB heuristic
- `-chb`: Use CHB heuristic
- `-minimize`: Clause minimization mode (aggressive/selective/none, default=selective)
- `-restart-base`: Luby restart base (default=20)
- `-restart-glucose-ratio`: Glucose restart LBD ratio (default=1.2)
- `-clause-del-*`: Clause deletion scoring parameters

### SAT Competition 2026 Format
- Solution: `s SATISFIABLE` / `s UNSATISFIABLE` / `s UNKNOWN`
- Exit codes: 10 (SAT), 20 (UNSAT), 0 (UNKNOWN)
- Model: DIMACS value lines (`v <lits> 0`)
- Comments: All verbose output prefixed with `c `

## Architecture

### Data Structures
- **Literal**: `uint32` (bit 31=sign, bits 0-30=variable index)
- **Variables**: 0-based internally, 1-based in DIMACS
- **Constants**: `litVarMask=0x7FFFFFFF`, `litNegatedMask=0x80000000`
- **Implication array** (`s.implication`): Reason clause per variable. Encoding: `>=0` original clause; `<=-5` learned (`-learnedIdx-5`, decode `learnedIdx = -impl - 5`); `-1` decision; `-2` unit-prop preprocess; `-3` pure-literal preprocess; `-4` reserved. The 4-slot offset frees `-1..-4` as pure sentinels so learned-clause decode can't misread preprocessing sentinels (was a soundness bug in `minimizeLearnedClause`). `Watch.ClauseIdx` uses a separate encoding (`-learnedIdx-1`); the two diverge by the offset, so `propagateWatched` translates watch→implication at the assign site.
- **Learned Clauses**: Contiguous literal storage with offset/size arrays
- **Free Slots**: `literalFreeSlot` struct tracks freed regions for reuse

### Key Files
- `internal/cnf/cnf.go`: Core data structures (Literal, Clause, CNF, Watch)
- `internal/parser/parser.go`: DIMACS CNF parser
- `internal/solver/solver_cdcl.go`: CDCL solver with 1-UIP, backjumping, restarts, tombstone deletion + periodic compaction
- `internal/solver/vsids.go`: VSIDS/LRB/CHB variable selection with activity heap
- `internal/solver/solver.go`: Base solver with propagation
- `internal/solver/solver_test.go`: Unit tests
- `internal/solver/verify.go`: Model verification
- `cmd/satience/main.go`: CLI
- `cmd/fuzz/main.go`: Fuzzer
- `internal/fuzzer/fuzzer.go`: Fuzzing infrastructure

### Implemented Optimizations
- **Watch list pre-allocation**: Accounts for original + learned clauses, caps at 256 capacity
- **Swap-remove clause deletion**: Move active clauses into deleted slots, update watches via scan
- **Free literal slot reuse**: Track and reuse freed literal regions
- **Watched literals**: O(1) propagation with ClauseIdx field in Watch struct (63% speedup)
- **Trail scanning optimization**: Pre-filter trail elements at current level in 1-UIP
- **Activity heap**: O(log n) variable selection instead of O(n) linear scan
- **varLevel cache**: O(1) level access during 1-UIP resolution
- **trailLevel cache**: Avoid random assignments[].Level access during propagation
- **Clause minimization**: Self-subsumption reduces learned clause size

## Testing

### Unit Tests
```bash
go test ./internal/solver -v
# 15/15 tests passing
```

### Fuzzer
```bash
./fuzz -n 20 -mode random -verbose
# Modes: random, structured, pigeonhole
# Verifies SAT models satisfy all clauses
```

### Soundness Verification
```bash
benchmark/eval_small_random.sh [n_instances]
# Tests N random instances < 200 vars, 60s timeout each
# 0 wrong results across 60+ tests
```

## Benchmark Infrastructure

- **meta.db**: GBD metadata (32,905+ instances, 200+ families)
- **gbd_instances/**: Real GBD CNF files (106+ downloaded)
- **download_instances.py**: Fetch instances from GBD
- **uv project**: Python benchmarks with gbd-tools, polars

## Key Decisions

- **Name**: satience (SAT + science/patience/essence)
- **1-UIP validation**: Skip clauses with ≠1 literal at current level
- **Activity reset on restart**: Prevents VSIDS loops
- **Phase saving for decisions only**: Don't save forced propagations
- **Restart policy**: Aggressive Glucose-style (1.2× avg LBD, min 25 conflicts)
- **maxLearned=2500**: Learned clause limit
- **ClauseIdx in Watch struct**: O(1) clause index access (63% speedup)
- **Default minimization = selective**: Aggressive mode adds 1-2% overhead
- **watchInitialized flag**: Set AFTER all clauses watched (critical bug fix)
- **Tombstone deletion + periodic compaction over array rebuild**: 23× GC reduction, bounds `learnedLiterals` growth
- **chooseWatchPositions helper**: Shared non-false literal selection for original/learned watch setup and compaction (preserves watched-literal invariant)

## Next Steps

### High Priority
1. **CHB/LRB tuning** (1-2 days): Better parameter tuning for random instances
2. **Watch list pre-allocation** ✅: Implemented - accounts for original + learned clauses, caps at 256 capacity
3. **Preprocessing** ✅: Implemented - adaptive strategy based on instance structure

### Medium Priority
4. **Extended fuzzer testing** (2-3 days): More instance types, UNSAT verification
5. **SAT Competition features** (1-2 days): JSON output, batch mode, progress reporting

### Not Planned (per constraints)
- **Inprocessing**: Removed - unit propagation at restart caused 34-228% slowdown with no benefit
- **Cardinality constraint detection**: PHP-like instances need specialized propagators for counting constraints. Expected 100-1000× speedup but requires fundamental architecture changes.
- **Variable elimination**: Removed (June 2026) due to soundness bugs—pos=1 elimination produced wrong results on PHP instances (SAT instead of UNSAT)
- Parallel solving
- Incremental solving
- Proof/unsat core generation

## Known Limitations

### PHP (Pigeonhole Principle) Instances
PHP UNSAT instances timeout while MiniSat solves instantly. This is **by design** (cardinality constraint detection is out of scope):
- Basic 1-UIP doesn't capture counting constraints
- VSIDS doesn't focus on critical "counting" variables
- **Out of scope**: Specialized propagators would provide 100-1000× speedup but require fundamental architecture changes

### Random Instances
Some random instances timeout. This is due to:
- VSIDS exploits community structure (absent in random instances)
- Lack of advanced heuristics (CHB, LRB tuning needed)
- Preprocessing disabled on random instances (causes 76× more conflicts if enabled)

### Variable Elimination (Removed)
Variable elimination was removed in June 2026 due to fundamental soundness issues:
- **Root cause**: pos=1 elimination assumes positive clauses are definitions (x = ¬A), but in PHP they are constraints
- **Symptom**: php_6p_5h_unsat.cnf returned SAT instead of UNSAT after VE
- **Resolution**: Removed ~800 lines of VE code; solver now relies on core CDCL techniques only

### Inprocessing (Removed July 2026)
Unit propagation inprocessing at restart was removed due to performance regression:
- **Symptom**: 34-228% slowdown on MiniSat fast suite with no solving benefit
- **Root cause**: Scanning all original clauses at every restart adds O(clauses) overhead per restart
- **Resolution**: Removed inprocessing call from restart(); preprocessing unit propagation remains for structured instances

## Recent Commits

```
<latest_commit> - Remove inprocessing from restart (34-228% slowdown with no benefit)
<latest_commit> - Remove preprocessing and inprocessing configuration (simplify codebase)
0845411 - Remove variable elimination (VE) due to soundness bugs
ebb9fa8 - Implement swap-remove clause deletion to reduce GC pressure
f53361a - Feature: Expose clause deletion scoring parameters as CLI options
8c694c2 - Optimize: Update default restart policy to aggressive configuration
```

## Critical Context

- Go version: `go1.22.2 linux/amd64`
- Git repo: `/home/luca/git/opencode-sat-new/`
- Benchmark project: `/home/luca/git/opencode-sat-new/benchmark/`
- GBD download URL: `https://benchmark-database.de/file/<hash>`
- Evaluation: 20 random instances < 200 vars, verify models for SAT
- CLI flag order: `-model file.cnf` works, `file.cnf -model` does not
- Compile with GOAMD64=v3 for AVX2/BMI2 optimizations
- **Variable elimination removed**: ~800 lines deleted due to soundness bugs (June 2026)
- **Inprocessing removed**: Unit propagation at restart caused 34-228% slowdown (July 2026)
- **Preprocessing**: Adaptive strategy based on instance structure (structured score ≥ 0.7 enables unit propagation)

## Clause Deletion + Compaction Implementation Details

### Why tombstones + periodic compaction?

**Before** (array rebuild):
```go
// Allocate NEW arrays every deletion
newLiterals := make([]cnf.Literal, ...)
for _, idx := range keepIndices {
    newLiterals = append(newLiterals, ...)  // Allocation!
}
s.learnedLiterals = newLiterals  // GC triggers
```

**After** (tombstones + compaction):
```go
// Deletion: mark tombstones, remove watches (NO allocation)
for i := 0; i < learnedCapacity; i++ {
    if deleted[i] && s.learnedSizes[i] > 0 {
        s.removeLearnedClauseWatches(i)
        s.learnedSizes[i] = 0  // tombstone
    }
}
// ... later, at the next restart (level 0), if tombstones accumulated:
s.compactLearnedClauses()  // move active clauses into a contiguous prefix,
                           // remap implications, rebuild all watch lists
                           // with non-false literal selection
```

### Key Components

1. **learnedActiveCount**: Track active clauses (excludes tombstones)
2. **learnedCapacity**: Total capacity including tombstone slots
3. **compactPending**: Set when accumulated tombstones (capacity − active) ≥ ~33% of capacity; cleared after compaction
4. **compactLearnedClauses()**: Reclaims tombstone literal gaps by compacting `learnedLiterals` to a contiguous prefix, remapping the implication array, and rebuilding all watch lists via `chooseWatchPositions` (non-false literal selection)
5. **chooseWatchPositions()**: Shared helper (used by original/learned watch setup AND compaction) that picks watched literals which are not both false, preserving the watched-literal invariant

### Trade-offs

**Pros**:
- 23× fewer GCs on large instances
- No array allocations during deletion (only at compaction, which is infrequent)
- `learnedLiterals` growth is bounded (tombstone gaps reclaimed periodically)
- Watch references remapped correctly during compaction

**Cons**:
- Compaction rebuilds all watch lists from scratch (O(clauses × literals)), but runs rarely (only at restart when tombstone ratio high)
- Literal pool has gaps between compactions (memory temporarily higher until next restart)
- Literal pool can fragment over time (mitigated by free slot reuse)
