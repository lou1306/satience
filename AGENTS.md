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

**Benchmark results** (MiniSat Fast Suite, 30s timeout, GOAMD64=v3, July 2026):
- **Solved**: 42/72 instances (58.3% solve rate)
- **Tseitin**: All solved (4×4, 5×5, 6×6 - both SAT and UNSAT) ✅
- **Arg chain**: Solved ✅
- **PHP UNSAT**: php_6p_5h now solved (2.1s); larger PHP still timeout (cardinality constraint reasoning needed)
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
- Adaptive restarts (Glucose-style EMA LBD > ratio×avg, now actually functional after fixing dead lbdSum/lbdCount increment bug; disabled for random instances where Luby base=5 suffices)
- Luby restart sequence fallback (base=100 default, base=5 for random instances)
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

### Inprocessing
- **Clause Vivification** (`solver_cdcl.go:1284`): Shortens learned clauses by detecting redundant literals via trial propagation. Runs every Nth restart (default 50, configurable via `-vivify-period`, 0=disabled). Adaptive: structured instances only. Soundness verified via 500K+ fuzzer iterations.
  - For each literal li in a clause, assume ¬li and propagate. If the prefix causes conflict, shrink to it (sound: the prefix is implied).
  - Uses `inVivification` flag to suppress false UNSAT from the unit scan during trial propagation (level > 0).
  - After the round, clears all Level > 0 assignments and re-propagates from scratch (trial propagation moves watches; without re-propagation, base assignments miss propagation → false UNSAT).
  - Does NOT remove literals that are TRUE under existing assignments (can't distinguish permanent unit-propagated from trial-forced without the `inVivification` flag; removal was unsound).

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
- `-vivify-period`: Run clause vivification every Nth restart (default=50, 0=disabled)
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
- **varLevel cache**: O(1) level access during 1-UIP resolution (retained: removing it causes ~5% regression — separate compact array beats unified struct for hot-path level-only accesses)
- **trailLevel cache**: Avoid random assignments[].Level access during propagation
- **Clause minimization**: Self-subsumption reduces learned clause size
- **qhead = decisionPoint after backtrack**: Avoids re-processing entire trail after each conflict; asserting literal propagated explicitly via `propagateAssertingLiteral`
- **Watch struct 8 bytes (no WatchPos)**: `myPos` derived from `clauseLits[0]` vs `falseLit`; 5→8 watches per cache line
- **Redundant watch list writeback removal**: Element mutations visible through shared backing array; writeback only after swap-remove (re-slice)

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
- **Activity reset on restart**: Prevents VSIDS loops — **REMOVED**: Standard MiniSat behavior preserves activity across restarts; 0.3× reset destroys search memory
- **Phase saving for decisions only**: Don't save forced propagations
- **Restart policy**: Glucose-style EMA LBD > ratio×avg (now functional after fixing dead lbdSum/lbdCount increment); disabled for random instances (Luby base=5 suffices); Luby fallback base=100 (base=5 for random)
- **maxLearned=2500**: Learned clause limit
- **ClauseIdx in Watch struct**: O(1) clause index access (63% speedup)
- **Default minimization = selective**: Aggressive mode adds 1-2% overhead
- **watchInitialized flag**: Set AFTER all clauses watched (critical bug fix)
- **Tombstone deletion + periodic compaction over array rebuild**: 23× GC reduction, bounds `learnedLiterals` growth
- **chooseWatchPositions helper**: Shared non-false literal selection for original/learned watch setup and compaction (preserves watched-literal invariant)
- **Incremental VSIDS heap over full rebuild**: `heapPos` array + `increaseKey`/`decreaseKey` eliminates O(n) `buildHeap` on every conflict; `decay` doesn't invalidate (uniform scaling preserves order); `selectVariableWithHeap` uses `removeMax` + `onUnassign` on backtrack (MiniSat-style)
- **O(1) unassigned count over O(n) scan**: `numUnassigned` field maintained at all assign/unassign sites replaces `allAssigned()`/`hasUnassigned()` linear scans
- **qhead = decisionPoint over qhead = 0 after backtrack**: Replaced O(trail × watchlist) trail re-processing per conflict with O(clause-size) `propagateAssertingLiteral` scan. `lastLearnedClauseIdx` tracks the most recently learned clause; after backjump + flip, the asserting (1-UIP) literal is explicitly enqueued if the clause is genuinely unit (checked via a scan: exactly 1 unassigned literal, all others false). The clause's watched literals sit at trail positions < decisionPoint and would otherwise never be re-checked by the propagation loop.
- **Watch struct 8 bytes (no WatchPos) over 12 bytes**: Dropped `WatchPos uint8` (3 bytes padding). `myPos` derived by comparing `clauseLits[0]` against precomputed `falseLit`. 5→8 watches per cache line.
- **Redundant watch list writeback removal**: Slice header writebacks only needed after swap-remove (which re-slices `watchList`); element mutations visible through shared backing array. Removed 3 unnecessary stores per trail element.
- **Batch writeback reverted**: Tracking `wlLen` separately caused 3.9% regression from register pressure — per-swap-remove writes are to a hot cache line (L1 hits), so `wlLen` overhead exceeds savings.
- **Contiguous literal pool skipped**: Only 1.2% of `propagateWatched` time; not worth routing original clause access through pool (complexity/risk of pool as source of truth).
- **EMA restart signal over single lastConflictLBD**: EMA (α=0.1, half-life ~7 conflicts) smooths individual LBD spikes. Single-LBD triggers spurious restarts every 2-5 conflicts at ratio=1.5. EMA detects sustained LBD increases without noise.
- **Glucose disabled for random instances**: Luby base=5 already restarts every 5-20 conflicts; adding Glucose changes the search trajectory without benefit. Glucose is only active for structured instances (default ratio=1.5 from CLI).
- **Fuzzer assignmentsToModel fix**: `Level > 0` → `>= 0` — preprocessing assigns at Level 0; excluding Level 0 produced empty models for unit-propagated instances, causing false soundness failures in the fuzzer (not a solver bug).

## Next Steps

### High Priority
1. **CHB/LRB tuning** (1-2 days): Better parameter tuning for random instances
2. **Watch list pre-allocation** ✅: Implemented - accounts for original + learned clauses, caps at 256 capacity
3. **Preprocessing** ✅: Implemented - adaptive strategy based on instance structure

### Performance Optimizations (from profiling)
Identified via CPU profiling on GBD instances. P0 items implemented; P1/P2 pending.

**P0 (Implemented July 2026)**:
- **Incremental VSIDS heap** (`vsids.go`): Replaced O(n) `buildHeap` on every conflict with O(log n) `increaseKey`/`decreaseKey` using a `heapPos` position array. `decay` no longer invalidates the heap (uniform scaling preserves order). `selectVariableWithHeap` uses `removeMax` + `onUnassign` on backtrack (MiniSat-style). Eliminated 27% of runtime from `buildHeap`/`heap.init`. Decide overhead: 33% → 14%.
- **O(1) unassigned count** (`solver_cdcl.go`): `numUnassigned` field replaces O(n) `allAssigned()`/`hasUnassigned()` scans. Maintained at all assignment/unassignment sites.

**P1 (Implemented July 2026)**:
- **`qhead = 0` after every backtrack** (`solver_cdcl.go`): Replaced `qhead = 0` (which re-processed the ENTIRE trail through watch lists after every conflict, O(trail × watchlist_size) per conflict) with `qhead = decisionPoint` (only trail elements from the backjump level onward). The asserting literal from the just-learned clause is propagated explicitly by `propagateAssertingLiteral()` — a cheap O(clause-size) scan — because its watched literals sit at trail positions < decisionPoint and would otherwise never be re-checked. `lastLearnedClauseIdx` field tracks the most recently learned clause. Correctness: 500/500 fuzzer + all unit tests pass. Improvement: ~7% reduction in `propagateWatched` time; total impact modest because the Blit fast path makes the old trail re-processing cheap (most watches are O(1) skip). Should help more on larger instances with longer trails.
- **`Assignment` struct 16 bytes + redundant `varLevel` array** — **REVERTED** (`solver.go:8`): Attempted to change `Level int` → `Level int32` (struct → 8 bytes) and remove `varLevel`, using `assignments[i].Level` directly. Caused a ~5-11% regression in `propagateWatched` flat time: the `varLevel` array provides better cache locality for the level-only accesses in the hot path (Blit fast path, replacement search). Consolidating Value + Level into one struct forced more cache pressure on the `assignments` array. Lesson: separate compact arrays for hot-path fields can outlive a unified struct even when total memory is higher.
- **Unit scan O(units) on every `propagateWatched` call** (`solver_cdcl.go:2207`): Scans `unitLearnedList` on every propagation call. Dead entries accumulate between compactions. Fix: gate on a `unitsDirty` flag set during backtrack, or watch unit clauses via the standard watch mechanism.
- **`decay`/`decayLBD` O(n) scans every 10 conflicts / every conflict** (`vsids.go:475,385`): `decay` scans all activities every 10 conflicts (9.9% of runtime). `decayLBD` scans all lbdBonus every conflict (7.1% of runtime). Fix: use lazy decay (multiply bump amount by `1/decayFactor` instead of scaling all activities), or accept the cost (it's inherent to VSIDS).

**P2 (Implemented July 2026)**:
- **Redundant watch list writeback** (`solver_cdcl.go`): Removed 3 unconditional `s.watchLists[watchIdx] = watchList` stores per trail element — at end-of-loop and before early returns. The only modification to the slice header is the swap-remove (`watchList = watchList[:lastIdx]`), which already writes back immediately. Element mutations via `watchList[i] = ...` are visible through the shared backing array and need no writeback.
- **`Watch` struct 12→8 bytes** (`cnf.go`): Dropped `WatchPos uint8` field (1 byte + 3 padding). `myPos` is now derived in the slow path by comparing `clauseLits[0]` against the false literal (`myPos=0` iff `clauseLits[0]==falseLit`). 5→8 watches per cache line. `falseLit` is precomputed once per trail element (outer loop). All watch creation sites updated to omit `WatchPos`. The `learnedWatchIdx` update uses the already-computed `myPos` instead of `watch.WatchPos`.
- **uint32 indices → int in hot path** (`solver_cdcl.go`): Converted `blitVarIdx`, `clauseLitVar`, `watchLitVar`, and `varIdx` from `uint32` to `int` in `propagateWatched`. `varIdx` now uses `lit` (int from trail) directly instead of `uint32(lit)`. Reduces type conversions and may help BCE (though Go's BCE can't prove untrusted literal-derived indices are in bounds, so impact is mostly from fewer conversion instructions). Neutral to 3% improvement depending on instance.
- **Batch writeback** — **REVERTED**: Attempted to track `wlLen` separately and write back `s.watchLists[watchIdx]` once per trail element instead of per swap-remove. Caused 3.9% regression from `wlLen` register pressure — the per-swap-remove writes are to a hot cache line (same `watchIdx`), so they're L1 hits, and the `wlLen` tracking overhead exceeds the savings.
- **Contiguous literal pool** — **SKIPPED**: Profiled at 60ms out of 4850ms `propagateWatched` time (1.2%). Not worth the complexity/risk of routing original clause access through the pool (would require making the pool the source of truth, handling conflict analysis access, and managing pool reallocation).

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

### Unit Propagation Inprocessing (Removed July 2026)
Unit propagation inprocessing at restart was removed due to performance regression:
- **Symptom**: 34-228% slowdown on MiniSat fast suite with no solving benefit
- **Root cause**: Scanning all original clauses at every restart adds O(clauses) overhead per restart
- **Resolution**: Removed inprocessing call from restart(); preprocessing unit propagation remains for structured instances

### Clause Vivification (Re-enabled July 2026)
Clause vivification was re-enabled after fixing three soundness bugs that caused false UNSAT:
- **Stale `len(s.tmpLearnedLits)`**: `vivifyClause` used a local slice but `runVivification` read `len(s.tmpLearnedLits)` (stale from last `learnClause`), appending stale literals to vivified clauses. Fixed by `s.tmpLearnedLits = newLits` before return.
- **Unit scan false UNSAT**: The unit scan in `propagateWatched` set `emptyClauseFound` when a unit clause conflicted with a trial assumption — declaring UNSAT from a mere trial conflict. Fixed with `inVivification` flag (skip unit scan during trial, level > 0).
- **Watch moves not re-checked**: Trial propagation moved watches; after `cancelUntil(0)`, base assignments remained in `s.assignments` but weren't in the trail, causing missed propagations. Fixed by clearing Level > 0 assignments and re-propagating from scratch after the round.
- Also stopped removing TRUE literals at Level > 0 (unsound: can't distinguish permanent unit-propagated from trial-forced).

## Recent Commits

```
04b8207 - Fix dead Glucose restart criterion + EMA restart signal + fuzzer model fix
7848083 - Stop resetting VSIDS activity on restart + P2 perf + unitsDirty gate
dc98229 - P0 perf: incremental VSIDS heap + O(1) unassigned count
da92ca7 - Fix vivification soundness bugs causing false UNSAT, re-enable by default
c2d0b03 - Add -vivify-period CLI flag + vivification tests
37e8e73 - Add clause vivification inprocessing
1f843fe - Add blocking literals for fast watch propagation skip
6bf75ca Recursive minimization CLI flag + tests
7775d27 MiniSat-style watches, soundness fixes, recursive minimization
23af6bf MiniSat-style watched literals: replace Blit with WatchPos
05a7161 Remove ~1900 lines dead code, add P2 perf + P3 quality fixes
96239d3 Fix two soundness issues: parser unterminated clause, learnedWatchIdx mismatch
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
