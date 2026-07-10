# Project: satience - CDCL SAT Solver in Go

## Goal & Constraints
Build a sound and complete CDCL SAT solver in Go with DIMACS CNF support, benchmarking on real GBD instances.
- DIMACS format only, single-threaded, no incremental/parallel/proof
- **No cardinality constraint detection** (PHP instances out of scope)
- 60s timeout per benchmark, GOAMD64=v3 build
- SAT Competition 2026 output format (exit 10=SAT, 20=UNSAT, 0=UNKNOWN)

## Status
Sound and complete. 15/15 unit tests, 100% soundness on 300+ fuzzer iterations.
MiniSat Fast Suite (30s timeout): **65/72 solved** (July 2026). Verified sound via minisat cross-check (`benchmark/cross_check_minisat.sh`): 0 mismatches.

### Recent Fixes (July 2026)
- **SCC-based equivalence detection**: Detects equivalent literals via Tarjan's SCC on the binary implication graph (built from binary clauses: (a∨b) gives edges ¬a→b, ¬b→a). Literals in the same SCC are equivalent; if l and ¬l are in the same SCC → UNSAT. Equivalent literals are merged into a representative, tautologies/duplicates from substitution are removed, and the mapping is stored for model reconstruction (`extendModel()` after SAT). Replaces the previously-banned pattern-matching variant that produced false equivalences. Significant merges on binary-heavy instances (e.g., bb34f22f: 2790 merged). -1 regression (`daf59d67` SAT@13s→TIMEOUT@31s, trajectory shift from 200 merged literals) — 66/72 → 65/72. Model verified on SAT instances with equivalences (262ba88b, 5a65b2816). Soundness verified: minisat cross-check 0 mismatches, fuzzer 400/400.
- **Usage-aware clause deletion + vivify LBD fix**: (1) Clause deletion was deleting clauses in arbitrary order within each LBD tier (LBD>5, then LBD>3). Now collects candidates per tier, sorts by PropCount ascending, and deletes least-propagated first — keeping high-traffic clauses that frequently cause propagations. The `PropCount` field was already maintained (incremented on each learned-clause propagation) but never read by the live deletion code. (2) Vivification shrinks clauses but never updated `learnedMetadata[].LBD`, leaving stale high-LBD values that caused valuable post-vivification glue clauses to be deleted. Fixed by setting `LBD = min(oldLBD, newSize)` (LBD ≤ clause size is a sound overestimate). No suite change (66/72) — the 6 remaining timeouts are throughput-bound (Go vs C) or search-quality-bound (deep search), not clause-database-quality-bound. Soundness verified: minisat cross-check 0 mismatches, fuzzer 400/400.
- **Size- and structure-adaptive LBD bonus scale**: The fixed `lbdBonusScale=10` (from the previous fix) caused bimodal behavior: small binary-heavy instances (e.g., `44092fcc`, 90v, 97.6% binary) need high LBD guidance (~2000) to escape bad trajectories, while long-clause instances (e.g., `274099073`, 80v, 99.4% long) need pure VSIDS (scale=10). A fixed scale solves one but not both. Replaced with adaptive formula: `scale = max(10, 200000 × binaryRatio / numVars)`. Binary-heavy small instances get ~2000, long-clause or large instances get 10. The formula uses binary clause ratio (not just size) because binary clauses produce LBD=2 glue clauses that benefit from guidance, while long clauses produce high-LBD clauses where the bonus dominates VSIDS. `44092fcc` TIMEOUT→UNSAT@4.5s, no regressions. 65/72 → 66/72 (+1). CLI: `-lbd-scale N` (non-zero overrides adaptive; default=0=adaptive). Soundness verified: minisat cross-check 0 mismatches, fuzzer 400/400.
- **LBD bonus scale reduction (2000→10)**: The LBD bonus (`lbdBonusScale=2000`) was 250x larger than VSIDS activity bumps (`baseBumpAmount=25`), making the heap score `activity + lbdBonus` dominated by LBD bonus. Variables in glue clauses (LBD=2: bonus=500) were permanently over-prioritized over variables with high VSIDS activity, causing the solver to repeatedly select the same variables → deep search (props/dec ~100, conflict level ~44-50) → wide 1-UIP clauses (62 vs minisat's 12 lits/clause pre-minimization). Reducing `lbdBonusScale` to 10 makes the glue-clause bonus (2.5) a tie-breaker (~10% of bump amount) instead of a dominator. 61/72 → 65/72 (+5 gained: `274099073` TIMEOUT→SAT, `8202af80` TIMEOUT→SAT, `8d58ca18` TIMEOUT→SAT, `93a5d44e` TIMEOUT→UNSAT, `b07759f4` TIMEOUT→UNSAT; -1 lost: `44092fcc` UNSAT@1s→TIMEOUT, small structured instance where LBD guidance mattered). Net +4. Soundness verified: minisat cross-check 0 mismatches, fuzzer 400/400.
- **Contiguous clause storage (single-storage)**: `Clauses[].Literals` previously pointed to per-clause allocations from the parser, while a separate `literalPool` held a copy — two copies causing cache misses on every clause-data access in propagation (the `clauseLits[0]` dereference was the single hottest line at 570ms). Fixed by making `literalPool` the single source of truth: `RebuildLiteralPool` now re-points `Clauses[i].Literals` into the pool after building it. Changed pool type from `[]uint32` to `[]Literal` for direct slicing. The previous two-copy attempt (reverted) caused 21 false-UNSATs from stale pool data after watch swaps; single-storage eliminates the root cause (swaps modify the pool directly, no sync needed). Profile: `clauseLits[0]` cache-miss cost 570ms→420ms (26%); 96e9379c 4.0s→3.8s. Soundness verified: minisat cross-check 0 mismatches, fuzzer 400/400. No suite regression (61/72).
- **Structure classifier: long-clause signal**: `analyzeInstanceStructure` scored structured instances primarily on binary-clause ratio (weight 0.6), misclassifying structured instances with many long (>3 literal) clauses but few binary clauses as random (StructuredScore 0.40 < 0.7). This sent them down the random path (`restartBase=5`, Glucose disabled, aggressive decay), causing catastrophic thrashing: 381 restarts in 6700 conflicts, 2-3 glue clauses learned, props/dec ~100, level 90-110. Fixed by adding `LongClauseRatio` and using `max(binaryScore, longScore)` as the size score — random k-SAT has 0% long clauses so only structured instances are reclassified (822378be/b8143c9d: 0.40→0.91). Search quality fixed (props/dec 100→5) but instances still timeout due to propagation throughput (Mode 2, separate issue). No suite regression (61/72, identical instance set). Soundness verified: fuzzer 400/400.
- **Phase saving fix**: `savedPhase` was initialized to `true` and never updated — the only write site (`solver_cdcl.go:2953`) was a no-op writing back the value just read, and neither `assignLiteral` (decisions) nor `assignLiteralByClause` (propagations) recorded assigned polarities. Result: every decision used `savedPhase[varIdx] = true` (negated literal → variable=false) forever; phase saving did nothing. Fixed by adding `s.savedPhase[varIdx] = lit.IsNegated()` in both assignment functions and removing the dead decision-site write. Now propagations record the polarity clauses force and decisions reuse them — standard CDCL phase saving. 57/72 → 61/72 (+4). Soundness verified: minisat cross-check 0 mismatches, fuzzer 200/200 sound.
- **Vivification throttling + deterministic budget**: Vivify was thrashing on small/fast-restart instances — `runVivification` fired every 50th Luby restart (line 2014), but on tiny instances Luby restarts trigger every few hundred conflicts, so vivify ran 51 times in 32s (47% of CPU) for negligible yield (6/585 clauses modified per round). Two fixes: (1) added a `vivifyMinConflictGap` gate (default 20000) so vivify only fires after ≥20K new conflicts, preventing thrashing on easy instances; (2) replaced the 500ms wall-clock time budget (nondeterministic — wall-clock truncation checked different clauses per run, perturbing the search trajectory) with a deterministic `maxCheckPerRound = 2000` clause-count cap. Result: `274099073` (80v) TIMEOUT→SAT@27s; 55/72 → 57/72 (+2 net). CLI: `-vivify-min-gap N` (0=restart-based only).
- **Tautology + duplicate-literal removal + pure literal elimination**: New preprocessing passes in `preprocessAggressive()` (before the structure gate). `removeTautologiesAndDuplicates()` scans all clauses, removes tautologies and dedups repeated literals in O(total literals) with a temporary seen-array. `pureLiteralElimination()` assigns single-polarity variables and removes satisfied clauses. Both run on ALL instances (trivially sound; don't change search trajectory the way forced unit propagation does). 54/72 → 55/72 (+2 gained: `66e6fea6` TIMEOUT→UNSAT, `b40a1e31` TIMEOUT→SAT; -1 lost: `8d58ca18` SAT@26.7s→TIMEOUT, boundary).
- **Assignment packing**: Packed `Assignment` to 8 bytes (`Level int32` + `Value bool`), eliminated separate `varLevel []int` cache. Every variable lookup now hits one 8-byte struct instead of two arrays on different cache lines. Memory for 290K vars: 5.7MB → 2.3MB.
- **LearnedClauseLoc packing**: Packed `learnedOffsets` + `learnedSizes` (two `[]int` = 16B across two cache lines) into single `[]LearnedClauseLoc` (`Offset int32` + `Size int32` = 8B). All 60 access sites use the same index for both fields (verified — no mismatched-index co-access). Also removed dead `ClauseMetadata.Offset`/`.Size` fields (never read; `Size` was set once but unused). `ClauseMetadata`: 72B → 56B (genuinely 1 cache line now; old comment claiming 64B was wrong).
- **CDCL backjump** (`backtrack()`): Was using `trailHead[bjLevel]` (start of level bjLevel) as decision point, which unassigned the decision at bjLevel and re-assigned it FLIPPED (DPLL chronological backtracking). Fixed to `trailHead[bjLevel+1]` (keep level bjLevel, let `propagateAssertingLiteral()` handle the UIP). Matches `cancelUntil()`.
- **LBD storage filter**: `learnClause()` was discarding clauses with LBD > 8, meaning the solver did conflict analysis and threw away the result. Removed filter — store ALL learned clauses, use LBD for deletion priority only.
- **decide() simplification**: Removed non-standard diversification logic (random decisions, decidedVarSet override, consecutiveFlips tracking) that was overriding VSIDS. Standard CDCL: VSIDS + phase saving only.

### Soundness Bugs to Never Re-introduce
- **Contiguous literal pool (two-copy)**: An earlier attempt kept `literalPool` as a separate `[]uint32` copy alongside `Clauses[].Literals` (which pointed to per-clause parser allocations). Watch position swaps modified `Clauses[].Literals` but not the pool copy, causing stale data. The single-storage fix (`Clauses[].Literals` pointing into the pool, pool type `[]Literal`) eliminates the root cause — swaps modify the pool directly. Always run `benchmark/cross_check_minisat.sh` after propagation changes.
- **VSIDS var_inc (MiniSat-style O(1) decay)**: Attempted to replace O(n) activity decay scan with `varInc /= decayFactor` (O(1)). Soundness verified (fuzzer 400/400, cross-check 0 mismatches) but caused -3 suite regression (61→58): the float-rounding difference shifts the search trajectory, gaining 3 throughput-bound instances but losing 6 large SAT instances. The O(n) scan (7% of CPU) is not worth the trajectory shift. Do not re-attempt without first addressing the trajectory sensitivity.

## Architecture

### Data Structures
- **Literal**: `uint32` (bit 31=sign, bits 0-30=variable index). Variables 0-based internally, 1-based in DIMACS.
- **Implication array encoding** (`s.implication`): `>=0` original clause; `<=-5` learned (`-learnedIdx-5`); `-1` decision; `-2` unit-prop preprocess; `-3` pure-literal; `-4` reserved. The 4-slot offset frees `-1..-4` as sentinels so learned-clause decode can't misread preprocessing sentinels (was a soundness bug). `Watch.ClauseIdx` uses a separate encoding (`-learnedIdx-1`); `propagateWatched` translates watch→implication at the assign site.
- **Learned clauses**: Contiguous literal pool with packed `LearnedClauseLoc` (`Offset int32` + `Size int32`) replacing separate offset/size arrays. Deletion uses tombstones (`learnedLoc[i].Size=0`); `compactLearnedClauses()` reclaims gaps at the next restart (level 0) when tombstones exceed ~33% of capacity.

### Key Files
- `internal/solver/solver_cdcl.go`: CDCL solver (1-UIP, backjumping, restarts, deletion+compaction, vivification)
- `internal/solver/vsids.go`: VSIDS/LRB/CHB variable selection with incremental activity heap
- `internal/cnf/cnf.go`: Core types (Literal, Clause, CNF, Watch — 8 bytes, no WatchPos)
- `cmd/satience/main.go`: CLI; `cmd/fuzz/main.go`: Fuzzer

## Key Design Decisions

These capture the *why* behind choices that aren't obvious from the code.

### Conflict Analysis & Propagation
- **UIP at position 0**: Learned clauses are reordered so the UIP is at position 0 and a backjump-level literal at position 1 (MiniSat asserting-clause invariant). Positions 0/1 are watched directly. After backjump, `qhead=decisionPoint` skips re-processing the earlier trail; the asserting literal is propagated explicitly by `propagateAssertingLiteral()` (O(clause-size) scan) since the watched literals sit at trail positions < decisionPoint.
- **1-UIP resolved-variable fix**: Skip re-adding already-resolved variables during 1-UIP. Without this, `currentCount` inflates and the loop never converges; the old fallback (dropping literals) was unsound (false UNSAT on 3d937949).
- **Level-0 literal skip**: Don't add level-0 literals to learned clauses (always true, no propagation benefit).
- **VSIDS activity NOT reset on restart**: Standard MiniSat behavior preserves activity across restarts. Resets (0.3× or 0.8×) destroy search memory and cause regressions.

### Restart Policy
- Glucose-style EMA LBD > ratio×avg (EMA α=0.1 smooths spikes; single-LBD triggers spurious restarts). Disabled for random instances (Luby base=5 suffices); Luby fallback base=100.
- CLI defaults: `restart-base=100`, `restart-glucose-ratio=1.5`, `restart-glucose-min=10`.

### Clause Database
- `calculateMaxLearned`: ~15% of clauses, min 300, max 100K. Deletion triggered at 150% of dynamic limit (`maxLearned + conflicts/50`).
- Tombstone deletion + periodic compaction over array rebuild: 23× GC reduction. Compaction runs at level-0 restart (no learned clause in use as reason) when tombstones > 33%.
- `chooseWatchPositions()`: Shared non-false literal selection for original/learned watch setup and compaction (preserves watched-literal invariant).

### Preprocessing (Adaptive)
- **Tautology + duplicate-literal removal** (`removeTautologiesAndDuplicates`): Runs on ALL instances before the structure gate. O(total literals) with a temporary seen-array; removes tautological clauses and dedups repeated literals. Trivially sound.
- **Pure literal elimination** (`pureLiteralElimination`): Runs on ALL instances before the structure gate. Assigns variables appearing with one polarity only, removes satisfied clauses. Trivially sound (no clause contains the opposite polarity).
- StructuredScore ≥ 0.7: unit propagation enabled. Score < 0.7: unit propagation disabled (causes 76× more conflicts on random instances). Tautology/duplicate removal and PLE still run on all instances.

### VSIDS Heap
- Incremental heap (`heapPos` + `increaseKey`/`decreaseKey`); `decay()` sets `heapValid=false` (NOT uniform scaling — `lbdBonus` is NOT scaled by decay). O(n/10) per conflict vs old O(n) per-conflict rebuild.
- `numUnassigned` field: O(1) allAssigned/hasUnassigned.

## Soundness Bugs to Never Re-introduce

- **pos=1 variable elimination**: Removed (~800 lines). The "pos=1 elimination" variant assumes positive clauses are *definitions* of a variable and reconstructs the eliminated variable's value from that assumption. In PHP instances, positive clauses are *constraints*, not definitions → returned SAT instead of UNSAT on php_6p_5h_unsat. Standard Davis-Putnam VE (resolve all (x∨A)×(¬x∨B) pairs, reconstruct by trying x=true then x=false) is sound — the ban is on the definitional variant, not standard VE.
- **Buggy subset-check subsumption elimination**: Removed (296 lines). The clause-subsumption check was implemented incorrectly — it determined clause A was subsumed by B when it wasn't, removing necessary clauses → false SAT. Forward subsumption (literal subset check) is sound — the ban is on the buggy implementation, not the algorithm.
- **Pattern-matching equivalence detection**: Disabled. The detection used ad-hoc pattern matching on binary clause pairs and produced false equivalences (claimed a↔b when variables were not equivalent). SCC-based equivalence detection (Tarjan's SCC on the binary implication graph) is sound — the ban is on the pattern-matching variant, not SCC-based detection.
- **Blocked clause elimination (BCE)**: Disabled — was removing ALL clauses from arg_chain instances. Safe per-clause in isolation but unsound when applied aggressively (removes interacting constraints).
- **Unit-prop inprocessing at restart**: Removed (34-228% slowdown, no benefit).
- **Vivification**: Re-enabled after 3 fixes (stale `tmpLearnedLits` length, unit-scan false UNSAT via `inVivification` flag, watch moves not re-checked). Does NOT remove TRUE literals at Level > 0 (unsound).

## Critical Context

- Go 1.22+, GOAMD64=v3 for AVX2/BMI2
- CLI flag order: `-model file.cnf` works, `file.cnf -model` does not
- GBD download: `https://benchmark-database.de/file/<hash>`
- Fuzzer: `./fuzz -n 100 -mode random` (modes: random, structured, pigeonhole; verifies SAT models)
- Soundness eval: `benchmark/eval_small_random.sh [n]`
