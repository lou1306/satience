# Project: satience - CDCL SAT Solver in Go

## Goal & Constraints
Sound and complete CDCL SAT solver in Go with DIMACS CNF support.
- DIMACS only, single-threaded, no incremental/parallel/proof
- No cardinality constraint detection (PHP out of scope)
- 60s/benchmark, GOAMD64=v3, SAT Competition 2026 output (10=SAT, 20=UNSAT, 0=UNKNOWN)

## Status
Sound and complete. 44/44 unit tests, 110/110 CNFgen soundness (6 families: PHP, Tseitin, ordering, counting, parity, pebbling), 72/72 minisat cross-check (0 mismatches).
MiniSat Fast Suite (30s timeout): **72/72 solved (100%)**, PAR-2 2.11s (July 2026, best-of-3).

### Potential Next Steps (July 2026)
- **C2. Propagation `unsafe` bounds-check elimination**: First `unsafe` usage. Expected ~1-3% PAR-2 (not 10-15% — `litValue[watch.Blit]` saves ~1 cycle/watch). Higher maintainability cost than gain warrants; defer.

### Completed (July 2026)

- **L1. Struct field reorganization (hot/warm/cold blocks)** (PAR-2 neutral, layout stability): Reordered `CDCLSolver` struct fields into three blocks — HOT (12 fields touched every propagation/decision, ~3.5 cache lines) at the top, WARM (per-conflict/per-restart) in the middle, COLD CONFIG + DEBUG/STATS at the bottom. Adding cold fields (vivify counters, debug stats) no longer shifts the cache lines of hot fields, preventing struct-layout sensitivity that caused V1/V5 regressions (30eb4ef4 SAT ~12s ↔ TIMEOUT 30s). Unblocks future cold-field experiments. PAR-2 neutral (2.11s best-of-3, within noise of 2.11s baseline).

- **C6. Activity-based clause deletion (gated, decay 0.99)** (-9.4% PAR-2): VSIDS-style decayed activity for within-LBD-tier deletion (was FIFO). `Activity float64` on `cnf.ClauseMetadata`; `claInc`/`claDecayFactor`/`claActivityEnabled` on `CDCLSolver`; `clause_activity.go` with O(1) decay (`claInc /= claDecayFactor`, rescale at 1e100); bump in `learnClause` 1-UIP loop; `deleteLearnedClauses` sorts by Activity ascending within LBD tier (FIFO tiebreak). CLI: `-cla-decay` (0.99), `-no-cla-activity`. Two key decisions: (1) **0.95→0.99** (MiniSat's 0.95 too aggressive — 0.99 preserves activity, helps 4/6 hurt instances). (2) **Gate** (solver_cdcl.go:1147): disable when `StructuredScore < 0.7 OR (binaryRatio > 0.5 AND density < 4.0)`. Ungated: 2 timeouts (30eb4ef4, bb34f22f) +51% PAR-2; gated: 18 helps (-19.25s), 2 hurts (+1.90s). PAR-2 2.34s→2.12s.

- **S1. 1-UIP candidate scan narrowed to current-level trail** (PAR-2 neutral): `learnClause` scanned entire trail; current-level literals start at `trailHead[s.level]`, so scan `trail[len-1] down to trailHead[s.level]`. One-line: `i >= 0` → `i >= s.trailHead[s.level]`. Real gain washed out by noise; larger on deeper-search instances.

- **S2. trail/preprocessTrail int → uint32** (PAR-2 neutral): 4B/element (was 8B), doubles cache density on hot propagation trail scan. Within noise.

- **S3. implication int → int32** (PAR-2 neutral): 4B/element (was 8B), 800KB→400KB on 100K-var instances. Halves memory on hot path. Within noise.

- **S4. Eliminate `propagateAssertingLiteral` O(clause-size) scan** (PAR-2 neutral): Replaced O(clause-size) scan with O(1) `assignLiteralByClause(literals[0], propLevel, -learnedIdx-5)` trusting asserting-clause invariant (UIP at pos 0 is only literal at `s.level > bjLevel`; others at ≤ `bjLevel` retain false values). Debug/release file pair (`propagate_asserting_debug.go`/`_nodebug.go`) panics on invariant violation. Within noise (short learned clauses → small scan cost).

- **C5. Dense-instance subsumption gate** (PAR-2 neutral, fixes 5 cnfgen families): `skipSubsumption` for density > 60. Found via `benchmark/cnfgen_divergence.sh`: `ramlb_6_6` (density 149) 5s→0.03s, `ramlb_7_7` 16.5s→0.143s, `kcliquebin_6` 1.98s→0.023s. Threshold 60 > highest suite instance (32baec6a, 49).

- **D2. Pure k-SAT adaptive split** (-3.6% PAR-2): Split `StructuredScore < 0.7` branch by `binaryRatio`: pure k-SAT (`binaryRatio == 0 AND density > 4.5`) uses default decay 0.95 + `restartBase=5` + Glucose active; mixed-binary keeps aggressive decay (0.30→0.60) + `restartBase=5` + Glucose disabled. `566f366c` 11.94s→0.37s (32x, was 346x slower than minisat). `density > 4.5` excludes `30eb4ef4` (4.20, regresses to TIMEOUT under default decay). `restartBase=5` critical (100→7.7s).

- **DPLL removal** (-397 lines): Removed legacy `solver.go` DPLL, `SolveDPLL()` bridge, `-dpll` flag, 7 tests. Preserved `Assignment`/`LearnedClauseLoc` structs. 51→44 tests.

- **B11. Adaptive phase flip 0.1 → 0.5** (-17.4% PAR-2, solves last timeout): `bb34f22f` (7807v, 67% binary, score 0.73) timed out — props/dec restart fired every ~100 conflicts preempting Luby after lubyIndex~14; ~2000 restarts into same basin; phase saving pulled back each time. Adaptive flip (0.1) too weak. 0.5 breaks the fixed point; bb34f22f solves ~20s. Gate (props/dec > 120%) ensures low-props/dec instances unaffected. PAR-2 3.05s→2.50s.

- **C3. Dense-binary BVE gating** (-30.9% PAR-2): `skipBVE` for binaryRatio>0.95 AND density>10 — BVE hit resolvent budget eliminating 7-14% of vars while spending 4-15s. Skip `restartBase=20` override for same. bb34f22f (density 3.12) NOT gated.

- **C1. Precompute ClauseIdx decode + cache learnedMetadata** (-7.45% PAR-2): Decoded `watch.ClauseIdx` once into `isLearned`/`clauseID`/`myPos` (was 5+ decodes/watch). Skipped Blit-based optimization — UNSOUND (Blit goes stale on sibling watch replacement).

- **B10. Luby exhaustion cap** (resolves `274099073`): When `threshold > lubyThresholdCap` (10000), reset `lubyIndex=0`. Gated on `longClauseRatio > 0.95` — `> 0.8` regresses 80-95% long cluster.

- **C0. Learned-clause subsumption** (-3.5% PAR-2): Forward + self-subsumption using binary learned clauses as subsumers, at level-0 restart boundaries. Self-gating: early-returns when no binary learned clauses.

- **B5. emaLBD reset + Glucose ratio clamp** (correctness fix): `emaLBD` was reset to 0 on every restart, making Glucose criterion dead. Removed reset; raised `glucoseRatio` clamp 5.0→100.0; added `glucoseGap` (100 conflicts) gate.

- **Batch 1: Dead code + redundant checks** (no behavior change): Removed unused VSIDS/CDCL fields/funcs; gated `glueCount` behind `verbose`; fixed redundant branch in `propagateWatched`; simplified `decide()` phase handling.

- **Batch 2: Deprecate CHB entirely** (-564 lines): Removed opt-in CHB branching heuristic. Never enabled by default.

- **Batch 3a: Remove dead hasUnassigned guard** (no behavior change): O(reasonLen) guard in 1-UIP never fired (0/72) — reason clauses never have unassigned literals.

- **Batch 3b: Parallel-array VSIDS heap** (PAR-2 neutral): `vsidsHeap` from `[]vsidsHeapItem` (16B) to parallel arrays `[]float64` scores + `[]uint32` varIdxs. Comparison path touches only 8B scores.

#### Regressions (do not retry)

- **B4. Best-phase saving** (+36.4% PAR-2, 3 timeouts): Kissat-style best-phase on shallow conflicts. `savedPhase` already captures useful info; overriding with stale "best" sends search into worse branches.

- **B7. Glucose ratio < 10.0** (all worse): 1.5→4.04s, 5.0→3.12s vs 10.0→3.16s. Lower ratios fire too aggressively. 10.0 = safety net only.

- **B8. DB cap K≤5** (+54.3% PAR-2, 3 timeouts): DB bloat is symptom of high conflict count, not cause. `conflicts/50` growth serves a purpose.

- **B9. Raw PropCount deletion** (+33% PAR-2, 2 timeouts): Cumulative counter with no recency — stale "busy" clauses protected forever. FIFO (current) is better. Decayed activity explored in C6 — net-positive when gated (StructuredScore ≥ 0.7 AND NOT (binaryRatio > 0.5 AND density < 4.0)) with decay 0.99.

- **C4. `-rnd-init` by default** (net-negative): Solves bb34f22f but regresses 30eb4ef4 at every noise level. Kept opt-in (default 0). Tie-breaking NOT root cause of 274099073 (resolved by B10).

- **D1. Remove `SetAggressiveDecay()` for random-like instances** (+30.0% PAR-2, 1 timeout): Hypothesis: aggressive VSIDS decay (0.30→0.60) on random-like (StructuredScore<0.7) loses conflict memory vs minisat (566f366c: 290K conflicts vs minisat's 2.5K). Fix: remove so random-like uses default 0.95. Targets improved massively (566f366c -10.9s) BUT 15+ sub-0.7 instances regressed (30eb4ef4 SAT→TIMEOUT +48.4s, 262ba88b +7.0s, 5a65b281 +5.6s). Aggressive decay helps broad class of sub-0.7 instances, not just easy random k-SAT. Ban on removing SetAggressiveDecay; 346x ratio on 566f366c accepted as known weak spot. Partially addressed by D2.

- **D3. avgLBD-based adaptive decay** (NO SIGNAL): After aggressive decay ramps up (~5000 conflicts), switch to default if avgLBD < threshold. Of 10 instances differing >0.1s between decay modes, 2 prefer default (-2.32s), 8 prefer aggressive (+38.3s). avgLBD of default-preferring (5.6, 17.5) spans ENTIRE range of aggressive-preferring (0.0-19.8) — signal predicts OPPOSITE. No other runtime signal (conflicts, decisions, props/dec, level) separates either. Ban on runtime-signal-based adaptive decay; classifier's structural gate is right approach.

- **C7. DB size retune under activity deletion** (NO RETUNING): Swept 2×2 grid (`-max-learned-mult`, `-conflict-growth-div`): mult=5/7 all → 71/72, 1 timeout. mult=12 → 72/72, 2.16s (neutral); mult=15 → +0.20s. Smaller DB loses mid-activity clauses; larger bloats propagation. `numVars*10` floor + `conflicts/50` growth well-tuned independent of deletion ordering. Confirms B8 under activity-ordered deletion.

- **A3. Watch-list write-back skip** (INFEASIBLE): `watchList = watchList[:lastIdx]` changes slice header — write-back required.

- **V1. Vivify "tried" bit** (+34.6% PAR-2, 1 timeout): Per-clause "tried" bitmap skips clauses that failed vivify. Bit set on failure, cleared on success or activity bump. 30eb4ef4 timed out 3/3 runs (2.08s→2.80s). Root cause: vivifiability is DYNAMIC — clause unvivifiable now may become vivifiable as new learned clauses add implications. Activity-bump clear only fires when `claActivityEnabled=true` (score ≥ 0.7) — exactly where waste is lowest. On random-like (score < 0.7, waste highest), bit never cleared, vivify becomes no-op after round 1. Ban on permanent "tried" bit; vivifiability is dynamic.

- **V2. Vivify budget/frequency/order sweep** (NO REDUCTION VIABLE): Swept 5 configs (best-of-3): c1 (20K/1000/FIFO) 3.95s 3 timeouts; c2 (20K/1000/LIFO) 3.12s 1 timeout (30eb4ef4); c4 (40K/2000/FIFO) 3.17s 2 timeouts; c5 (40K/1000/LIFO) 2.88s 1 timeout; c6 (20K/2000/LIFO) 2.55s 1 timeout (bb34f22f). Baseline (20K/2000/FIFO) = 2.07s, 72/72. EVERY reduction causes timeouts. 30eb4ef4 depends on full ~1753 clauses every 20K conflicts; bb34f22f needs frequent rounds AND FIFO. Recency hypothesis WRONG — FIFO beats LIFO (oldest clauses accumulated more implications, MORE vivifiable). 98% waste irreducible: only way to know if clause vivifies is full propagation. CLI flags (`-vivify-max-check`, `-vivify-recent`) KEPT as A/B infra (defaults match baseline, no hot-path overhead). Ban on reducing budget/frequency or switching to LIFO.

- **V3. Vivify LBD/size ratio filter** (NO SIGNAL): Hypothesis: low LBD/size ratio (clustered levels) → stronger implication chains → more vivifiable. Instrumented `vivifyClause`, bucketed 96K checks on 5 instances. Signal is instance-dependent and contradictory: non-binary (worst waste: 30eb4ef4 0.025%, 274099073 0.61%) uniformly ~0% across ALL ratios — no skippable bucket. Binary-heavy bb34f22f: OPPOSITE — ratio<0.1 has 44% success (vs 9% overall). Instance-specific gating (BIG=nil → skip ratio=1.0) saves 24% on 30eb4ef4 but changes WHICH clauses checked → same failure mode as V2 (30eb4ef4 timeout). No universal threshold. Confirms V1/V2 from third angle: vivifiability not predictable from clause metadata (LBD, size, ratio); only predictor is full trial propagation. Instrumentation reverted.

- **V4. BIG re-minimization replacing vivify** (+17.4% PAR-2, 1 timeout): Replace vivify with periodic `rebuildBIG` (incl. learned binary) + `runBIGReMinimization` (transitive BIG BFS, budget 64, no trial propagation). Vivify as fallback for binaryRatio < 0.3. PAR-2 2.558s→3.003s, 71/72 (bb34f22f TIMEOUT). Root cause: BIG re-min finds STRICT SUBSET of vivify's removals (binary implications only vs all clause lengths via trial propagation). On bb34f22f: big-remin 9 rounds/2461 mods vs baseline vivify 6 rounds + subsumption str=2136; V4's subsumption str dropped to 29 (BIG stole literals but found fewer total). Result: longer learned clauses (>50 bucket: 32816 vs 17994), 52% more conflicts, 28.6s vs 17.7s. Premise wrong: vivify's value comes from NON-BINARY implication chains. Running both = pure overhead. Ban on replacing vivify with BIG re-min. Reverted entirely.

- **V5. Vivify zero-round-disable** (+34.85% PAR-2, 1 timeout): Disable vivify after N=5 consecutive zero-modification rounds. `vivifyZeroRounds`/`vivifyZeroDisableThreshold` fields on CDCLSolver. Mechanism correct (verified with threshold=1). Adding struct fields shifted 30eb4ef4 from SAT ~11.5s to TIMEOUT consistently (struct layout change → non-deterministic perf regression — same sensitivity as V1-V4). Feature itself provided zero measurable benefit: no instance improved beyond noise, all non-timeout deltas <0.5s. Ban on struct layout changes to CDCLSolver for vivify-gating experiments. Reverted. Confirms V1-V4: vivify's 98% waste is irreducible price of finding 2% gems via trial propagation.

### Caveat
- Always run `benchmark/cross_check_minisat.sh` after propagation changes. O(1) VSIDS decay uses compensated factors (decayInterval=1, so `^(1/5)` of MiniSat's). Changing decay arithmetic shifts trajectory.
- **Regression-check protocol for hot-path changes**: Any change to `decide()`, `propagateWatched()`, `handleConflict()`, `learnClause()`, or VSIDS selection MUST be validated with PAR-2 before/after on MiniSat Fast Suite (use git worktree for "before" — never `git checkout` with unstaged files). Tests + CNFgen prove soundness, NOT performance. B4 passed all tests but regressed +36%. PAR-2 = sum(time if solved, 2×timeout if not) / 72; report average as headline, keep raw sum for detail table.

## Architecture

### Data Structures
- **Literal**: `uint32` (bit 31=sign, bits 0-30=var index). 0-based internal, 1-based DIMACS.
- **Watch**: 8B (`ClauseIdx int32` + `Blit uint32`). `ClauseIdx`: bit 31=learned, bit 30=myPos, bits 0-29=clause index. `Blit` = litTrue index (`varIdx*2 + negated`) for fast-path skip. Blit=0 sentinel (also valid for var 0 positive — rare missed cache hit).
- **Implication array** (`s.implication`): `>=0` original; `<=-5` learned (`-learnedIdx-5`); `-1` decision; `-2` unit-prop preprocess; `-3` pure-literal; `-4` reserved. 4-slot offset frees `-1..-4` as sentinels (was soundness bug). `Watch.ClauseIdx` uses separate encoding (`-learnedIdx-1`); `propagateWatched` translates watch→implication at assign site.
- **Learned clauses**: Contiguous literal pool with packed `LearnedClauseLoc` (`Offset int32` + `Size int32`). Tombstone deletion (`learnedLoc[i].Size=0`); `compactLearnedClauses()` at level-0 restart when tombstones > 33%. `learnedAlive` `[]byte` bitmap for 1-byte hot-path check.
- **Hot-path slice caching**: `propagateWatched` caches `watchLists`/`assignments` as locals; `vsidsHeap.down`/`up` cache `h.scores`/`h.varIdxs`. Valid because slices never grow during cached scope.

### Key Files
- `internal/solver/solver_cdcl.go`: CDCL solver (1-UIP, backjumping, restarts, deletion+compaction, vivification)
- `internal/solver/vsids.go`: VSIDS variable selection with incremental activity heap
- `internal/cnf/cnf.go`: Core types (Literal, Clause, CNF, Watch — 8 bytes, no WatchPos)
- `cmd/satience/main.go`: CLI; `benchmark/cnfgen_fuzz.sh`: soundness harness; `benchmark/cnfgen_divergence.sh`: divergence probe (15 families)

## Key Design Decisions

### Conflict Analysis & Propagation
- **UIP at position 0**: Learned clauses reordered so UIP at pos 0, backjump-level literal at pos 1 (MiniSat invariant). Positions 0/1 watched. After backjump, `qhead=decisionPoint` skips earlier trail; asserting literal propagated by `propagateAssertingLiteral()`.
- **1-UIP resolved-variable fix**: Skip re-adding resolved variables during 1-UIP. Without this, `currentCount` inflates and loop never converges; old fallback (dropping literals) was unsound (false UNSAT on 3d937949).
- **Level-0 literal skip**: Don't add level-0 literals to learned clauses (always true).
- **Recursive minimization: skip level-0 in reason clauses**: `exploreRemovable` uses `continue` (not `return false`) when reason contains level-0 literal. Level-0 permanently false, automatically "covered." Old `return false` blocked ~99% of minimization on long-clause instances.
- **BIG-based clause minimization**: `bigReachableInClause(lit)` does forward BFS through binary implication graph. Removable iff BFS reaches another literal genuinely in clause. Budget `maxBigBfsVisited=16`. Epoch-stamped `bigBfsVisited`. After BIG removes literal, `tmpSeenVar[v]` cleared to force recursive re-examination (prevents circular dependency, false UNSAT on 028d0cc7).
- **VSIDS activity NOT reset on restart**: Standard MiniSat. Resets (0.3× or 0.8×) destroy memory, cause regressions.

### Restart Policy
- Glucose-style EMA LBD > ratio×avg (α=0.1 smooths spikes). Disabled for random (Luby base=5 suffices). Ratio 10.0 = safety net. EMA persists across restarts (was reset to 0 — B5 fix); `lbdSum`/`lbdCount` reset on restart. `glucoseGap` (100 conflicts) prevents double-restarts.
- Props/dec-bounded: `props/dec > restartPropsDecLimit` (100) AND `lubyIndex >= 3` → restart. Gap `propsDecRestartGap` (100). On bb34f22f fires every ~100 conflicts, preempts Luby after lubyIndex~14.
- Level-capped: `longClauseRatio > 0.8` AND `lastConflictLevel > restartLevelCap` (40) → restart. Anti-thrashing: `lubyIndex >= 3`, `decisions > 10`, `levelRestartGap` (100).
- Luby exhaustion cap (B10): `longClauseRatio > 0.95` AND `threshold > lubyThresholdCap` (10000) → reset `lubyIndex=0`. `> 0.95` gate critical (`> 0.8` regresses 80-95% long cluster).
- **Adaptive phase flip** (B11): `props/dec > 120%` of limit → flip each saved phase with prob `adaptivePhaseFlipRate` (0.5). Hysteresis: disable at `< 80%`. Breaks phase-saving + VSIDS-preservation fixed point (bb34f22f). 0.5 (was 0.1) strong enough to escape.
- CLI defaults: `restart-base=200`, `restart-glucose-ratio=10.0`, `restart-glucose-min=10`, `restart-props-dec=100`, `adaptive-phase-flip=0.5`.

### Clause Database
- `calculateMaxLearned`: ~15% of clauses, min `max(300, numVars*10)`, max 100K. Deletion at 150% of dynamic limit (`maxLearned + conflicts/50`).
- **High-LBD shrink**: When `totalLbdCount > 1000` AND `totalLbdSum/totalLbdCount > 10`, shrink floor `numVars*10`→`numVars*3` (one-way).
- **FIFO clause deletion**: Two-pass LBD-tiered (LBD>5 then LBD>2, glue LBD≤2 never), reason clauses protected. Within tier, ascending clause-index (oldest first = FIFO). Decayed-activity sort (B9) regressed +33% — "keep high-use longer" suspect.
- **BVE budget**: `veBudget=5M` resolvents; large (>50K vars/500K clauses) → 2M.
- Tombstone deletion + periodic compaction: 23× GC reduction. Compaction at level-0 restart (no learned clause in use as reason) when tombstones > 33%. Rebuilds only learned-clause watches.
- `chooseWatchPositions()`: Shared non-false literal selection for original/learned watch setup and compaction.
- **Vivification**: Deterministic budget (`maxCheckPerRound=2000`), `vivifyMinConflictGap=20000` gate. Updates `learnedMetadata[].LBD = min(oldLBD, newSize)`. Does NOT remove TRUE literals at Level > 0 (unsound).

### Preprocessing (Adaptive)
- **Tautology + duplicate removal** (`removeTautologiesAndDuplicates`): All instances, before structure gate. O(total literals) with temp seen-array. Trivially sound.
- **Pure literal elimination** (`pureLiteralElimination`): All instances, before structure gate. Trivially sound.
- **Structure classifier**: `analyzeInstanceStructure` uses `binaryRatio`/`ternaryRatio`/`longClauseRatio`. Size score = `max(binaryScore, longScore)`. Runs BEFORE equivalence detection (equiv distorts ratios). StructuredScore ≥ 0.7: unit prop enabled. < 0.7: disabled (76× more conflicts on random). Tautology/duplicate/PLE run on all.
- **SCC-based equivalence detection**: Tarjan's SCC on BIG (binary clauses: (a∨b) gives ¬a→b, ¬b→a). Same SCC = equivalent; l and ¬l same SCC → UNSAT. Merged into representative, mapping for `extendModel()` after SAT. Merge gate: `mergedCount*100 < numVars` → skip substitution (still detect contradictions).
- **Dense-binary polarity gate**: binaryRatio>0.9 AND density>35 → skip occurrence-based initial phase override (BIG propagates to solution in 0 conflicts). Sparse (density<35) benefits from override.

### VSIDS Heap
- Lazy heap with sink: `bumpLarge`/`bumpLBD` don't `increaseKey` (O(1) bump). `selectVariableWithHeap` peeks root; if assigned, sinks to -Inf. `onUnassign` restores, sifts up. `buildHeap` includes ALL variables (heap never empties). Refresh every 2000 conflicts. `restart()` forces rebuild.
- **Parallel-array storage** (Batch 3b): `[]float64` scores + `[]uint32` varIdxs (was 16B struct). Comparison path touches only 8B scores.
- O(1) decay via `varInc /= decayFactor` (MiniSat trick). `decay()` doesn't touch heap. Rescale at `varInc > 1e100` sets `heapValid = false`.
- **Phase saving**: `savedPhase` from occurrence counts (more frequent polarity wins). Updated in `assignLiteral` (decisions) and `assignLiteralByClause` (propagations).
- **LBD bonus scale**: Adaptive `max(10, 200000 × binaryRatio / numVars)`. CLI: `-lbd-scale N` (non-zero overrides adaptive).
- `numUnassigned` field: O(1) allAssigned.

## Soundness Bugs to Never Re-introduce

- **pos=1 variable elimination** (~800 lines removed): Definitional variant assumes positive clauses are *definitions*; in PHP they're *constraints* → false SAT on php_6p_5h_unsat. Standard Davis-Putnam VE (resolve (x∨A)×(¬x∨B) pairs) is sound — ban is on definitional variant only.
- **Buggy subset-check subsumption** (296 lines removed): Incorrect subset check removed necessary clauses → false SAT. Forward subsumption (literal subset) sound — ban is on buggy implementation. Re-implemented as `subsumptionPass()` (originals) and `runLearnedSubsumption()` (learned, C0).
- **Pattern-matching equivalence detection**: Ad-hoc pattern matching produced false equivalences. SCC-based detection sound — ban is on pattern-matching variant.
- **Blocked clause elimination (BCE)**: Removed ALL clauses from arg_chain instances. Safe per-clause but unsound aggressive.
- **Unit-prop inprocessing at restart**: 34-228% slowdown, no benefit.
- **Vivification**: Re-enabled after 3 fixes (stale `tmpLearnedLits`, unit-scan false UNSAT via `inVivification` flag, watch moves not re-checked). Does NOT remove TRUE literals at Level > 0 (unsound).

## Critical Context

- Go 1.22+, GOAMD64=v3 for AVX2/BMI2
- CLI flag order: `-model file.cnf` works, `file.cnf -model` does not
- GBD download: `https://benchmark-database.de/file/<hash>`
- Soundness eval: `bash benchmark/cnfgen_fuzz.sh` (110 instances, 6 families, 6 workers, verifies SAT models); `benchmark/eval_small_random.sh [n]`
