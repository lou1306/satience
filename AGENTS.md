# Project: satience - CDCL SAT Solver in Go

## Goal & Constraints
Build a sound and complete CDCL SAT solver in Go with DIMACS CNF support, benchmarking on real GBD instances.
- DIMACS format only, single-threaded, no incremental/parallel/proof
- **No cardinality constraint detection** (PHP instances out of scope)
- 60s timeout per benchmark, GOAMD64=v3 build
- SAT Competition 2026 output format (exit 10=SAT, 20=UNSAT, 0=UNKNOWN)

## Status
Sound and complete. 51/51 unit tests, 100% soundness on 4000+ fuzzer iterations.
MiniSat Fast Suite (30s timeout): **71/72 solved (98.6%)**, PAR-2 4.48s (July 2026). Verified sound via minisat cross-check (`benchmark/cross_check_minisat.sh`): 0 mismatches.

### Potential Next Steps (July 2026)

Profiled on `BenchmarkRealInstanceLogAlloc` (post-A1+A2). CPU breakdown: `propagateWatched` 33.6%, `vsidsHeap.down` 13.2%, `learnClause` 11.9%, `backtrack` 7.1%, `onUnassign` 5.3%, `minimizeLearnedClause` 4.6%. `mallocgc` eliminated from top 20 (was 6.1% pre-A2).

All remaining timeout instances are solvable by minisat in <30s. Root causes: throughput (Go vs C, ~5-10x slower) and search quality (deep search → high-LBD clauses → no propagation guidance → deep search cycle).

#### A. Throughput Quick Wins (low risk, guaranteed speedup)

- **A3. Skip watch-list write-back for deletion-only path** (INFEASIBLE): Investigated and found infeasible. `watchLists[watchIdx] = watchList` (line 2963) is needed because `watchList = watchList[:lastIdx]` (line 2962) changes the slice length from `lastIdx+1` to `lastIdx`. Without the write-back, future calls to `propagateWatched` would load the stale (longer) slice and re-process deleted watch entries. The swap-with-last deletion DOES change the slice header — the original plan's claim "in-place, header unchanged" was incorrect.
- **C1. Precompute ClauseIdx decode + cache learnedMetadata + optimize heap down** (DONE, -7.45% PAR-2): `propagateWatched` decoded `watch.ClauseIdx` 5+ times per watch iteration (isLearned check + clauseID mask + myPos shift). Now decoded once into `isLearned`/`clauseID`/`myPos` at the top of the slow path. `learnedMetadata` cached as local (was `s.learnedMetadata` pointer chase per watch). `vsidsHeap.down` eliminated redundant `s[largest]` load by reusing already-fetched child struct. Result: PAR-2 6.31s → 5.84s avg (-7.45%), `44092fcc` went TIMEOUT→SOLVED (28.9s), 70/72 solved (was 69/72), no regressions. Skipped optimization #2 (use Blit for blitVarIdx/blitNegated) — UNSOUND: Blit goes stale when a sibling watch replacement swaps clause literals at position `myPos`, which is the other watch's `blitPos`. The stale literal is still in the clause (at a different position), so the fast-path `litValue[Blit]` check is sound (true → clause satisfied → skip correct). But using stale Blit for `blitVarIdx`/`blitNegated` in the slow path would propagate the wrong variable.

#### B. Search Quality (medium risk, high potential gains)

- **B4. Target/best phase saving** (REGRESSION — do not retry this variant): Tried Kissat-style best-phase saving. Recorded a decision's trial phase as `bestPhase[var]` when the resulting conflict was shallow (backjump level ≤ 10, conflict at the decision's own level). `decide()` preferred `bestPhase` over `savedPhase` when set. Result: PAR-2 regressed +36.4% (avg 6.32s → 8.63s; sum 455s → 621s over 72 instances), 3 instances went solved→timeout (`30eb4ef4`, `8d58ca18`, `de2b584e`), only 1 improved (`69d72f81`). Hypothesis was wrong: `savedPhase` (updated on every propagation) already captures useful phase info; overriding it with a stale "best" phase sends the search into worse branches. The "shallow conflict = good phase" signal is too noisy. Ban applies to the shallow-conflict-trigger variant; other best-phase strategies (e.g. most-propagations) are unexplored.

#### C. Throughput Deep Dives (higher effort)

- **C0. Learned-clause subsumption** (DONE, -3.5% PAR-2): Forward subsumption + self-subsumption (strengthening) on the learned clause DB, using binary learned clauses as the subsumers. Runs at level-0 restart boundaries (same safety conditions as `compactLearnedClauses`/`runVivification`), gated by `subsumptionPeriod=50` restarts + `subsumptionMinConflictGap=20000`. Self-gating: early-returns when no binary learned clauses exist (random instances produce none), so effectively free there. Forward subsumption deletes a non-binary learned clause C if some binary learned D ⊆ C. Strengthening removes literal l from C when a binary learned (¬l ∨ m) exists with m ∈ C (resolvent C∖{l} subsumes C); LBD updated to `min(oldLBD, newSize)`. Deterministic budget `maxCheckPerRound=2000` clauses. Result: PAR-2 4.66s → 4.48s avg (-3.5%), no solve-count change, no new timeouts, biggest wins on medium-hard instances (`4dd5ed7b` -1.04s, `de2b584e` -0.86s, `30eb4ef4` -0.72s). Sound variant of the previously-banned buggy subsumption: forward subsumption (literal subset check) was always sound — the ban was on the buggy implementation, not the algorithm.
- **C2. Propagation loop micro-optimization**: Investigate bounds check elimination via `unsafe` for the inner watch loop. Consider cache-line alignment of `Watch` structs. Potential 10-15% propagation speedup.
- **C3. Inprocessing: variable elimination for large instances**: Standard Davis-Putnam VE (sound per soundness rules — ban is on the "pos=1" definitional variant, not standard VE). Could reduce 29K vars / 708K clauses significantly on de2b584e. (Learned-clause subsumption is now re-implemented soundly — see C0; the earlier ban was on the buggy subset-check implementation, not the algorithm.)

#### Deferred (require test changes)

- **Legacy `solver.go` DPLL solver** (245 lines): Only used by tests in `solver_test.go` and the `-dpll` CLI escape hatch (`SolveDPLL()`). Removing requires migrating the ~10 DPLL test assertions to CDCL or deleting them. Low priority.

### Caveat
- Always run `benchmark/cross_check_minisat.sh` after propagation changes. O(1) VSIDS decay (`varInc /= decayFactor`) uses compensated decay factors (decayInterval=1, so factors are `^(1/5)` of MiniSat's). Changing decay arithmetic shifts the search trajectory.
- **Regression-check protocol for hot-path changes**: Any change to `decide()`, `propagateWatched()`, `handleConflict()`, `learnClause()`, or the VSIDS selection path MUST be validated with a PAR-2 before/after comparison on the MiniSat Fast Suite (use a git worktree for the "before" binary — never `git checkout` with unstaged files). Unit tests + fuzzer only prove soundness, NOT performance. B4 looked correct and passed all tests but regressed PAR-2 by +36%; without a before/after benchmark it would have shipped as an "improvement." PAR-2 = per-instance score summed (time if solved, 2×timeout if not), then averaged over the 72 instances; with a 30s timeout each instance scores at most 60s, so PAR-2 is in [0, 60]s. Report the average (sum/72) as the headline number; keep the raw sum for the detailed table.

## Architecture

### Data Structures
- **Literal**: `uint32` (bit 31=sign, bits 0-30=variable index). Variables 0-based internally, 1-based in DIMACS.
- **Watch**: 8 bytes (`ClauseIdx int32` + `Blit uint32`). `ClauseIdx` bit encoding: bit 31=learned flag, bit 30=myPos (which watch position 0/1), bits 0-29=clause index. `Blit` = litTrue index (`varIdx*2 + negated`) cached for fast-path skip via direct array index (no shift needed). Sentinel Blit=0 means "none found" but is also valid for var 0 positive — accepted as rare missed cache hit.
- **Implication array encoding** (`s.implication`): `>=0` original clause; `<=-5` learned (`-learnedIdx-5`); `-1` decision; `-2` unit-prop preprocess; `-3` pure-literal; `-4` reserved. The 4-slot offset frees `-1..-4` as sentinels so learned-clause decode can't misread preprocessing sentinels (was a soundness bug). `Watch.ClauseIdx` uses a separate encoding (`-learnedIdx-1`); `propagateWatched` translates watch→implication at the assign site.
- **Learned clauses**: Contiguous literal pool with packed `LearnedClauseLoc` (`Offset int32` + `Size int32`) replacing separate offset/size arrays. Deletion uses tombstones (`learnedLoc[i].Size=0`); `compactLearnedClauses()` reclaims gaps at the next restart (level 0) when tombstones exceed ~33% of capacity. `learnedAlive` `[]byte` bitmap alongside `learnedLoc` for 1-byte hot-path tombstone check (vs 8-byte struct access).
- **Hot-path slice caching**: `propagateWatched` and `vsidsHeap.down`/`up` cache outer slices (`watchLists`, `assignments`, `*h`) as locals to eliminate pointer-to-slice indirection. Valid because these slices never grow during the cached scope.

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
- **Recursive minimization: skip level-0 literals in reason clauses**: `exploreRemovable` uses `continue` (not `return false`) when a reason clause contains a level-0 literal. Level-0 literals are permanently false (preprocessing assignments never cleared by backjump), so they're automatically "covered." The old `return false` blocked ~99% of minimization on long-clause instances.
- **BIG-based clause minimization**: `bigReachableInClause(lit)` does forward BFS from `lit` through the binary implication graph (built from binary clauses). A literal is removable iff BFS reaches another literal genuinely in the clause. Budget-limited (`maxBigBfsVisited=16`). Uses epoch-stamped `bigBfsVisited` array. After BIG removes a literal, `tmpSeenVar[v]` is cleared to `false` — forcing recursive minimization to re-examine v independently (prevents circular dependency that caused false UNSAT on `028d0cc7`).
- **VSIDS activity NOT reset on restart**: Standard MiniSat behavior preserves activity across restarts. Resets (0.3× or 0.8×) destroy search memory and cause regressions.

### Restart Policy
- Glucose-style EMA LBD > ratio×avg (EMA α=0.1 smooths spikes; single-LBD triggers spurious restarts). Disabled for random instances (Luby base=5 suffices). Ratio 10.0 effectively disables it for most instances while keeping it as a safety net.
- Level-capped restart: on long-clause instances (`longClauseRatio > 0.8`), when `lastConflictLevel > restartLevelCap` (default 40), force a restart. Anti-thrashing: `lubyIndex >= 3`, `decisions > 10`, `levelRestartGap` (default 100) min-conflict gap.
- CLI defaults: `restart-base=200`, `restart-glucose-ratio=10.0`, `restart-glucose-min=10`.

### Clause Database
- `calculateMaxLearned`: ~15% of clauses, min `max(300, numVars*10)`, max 100K. Deletion triggered at 150% of dynamic limit (`maxLearned + conflicts/50`).
- **High-LBD shrink**: When cumulative average learned clause LBD is consistently high (>10), shrinks `maxLearned` floor from `numVars*10` to `numVars*3` (one-way, no grow-back). Fires once when `totalLbdCount > 1000` and `totalLbdSum/totalLbdCount > 10`.
- **Usage-aware deletion**: Clause deletion sorts candidates by `PropCount` ascending within each LBD tier, deleting least-propagated first — keeping high-traffic clauses.
- **BVE budget**: Default `veBudget=5M` resolvents; large instances (>50K vars/500K clauses) override to 2M. Prevents BVE from consuming the entire timeout on medium instances with many resolvents.
- Tombstone deletion + periodic compaction over array rebuild: 23× GC reduction. Compaction runs at level-0 restart (no learned clause in use as reason) when tombstones > 33%. Compaction rebuilds only learned-clause watches (original-clause watches are preserved — originals don't move during learned-clause compaction).
- `chooseWatchPositions()`: Shared non-false literal selection for original/learned watch setup and compaction (preserves watched-literal invariant).
- **Vivification**: Deterministic budget (`maxCheckPerRound=2000` clauses, not wall-clock) with `vivifyMinConflictGap=20000` gate (only fires after ≥20K new conflicts). Updates `learnedMetadata[].LBD = min(oldLBD, newSize)` after shrinking. Does NOT remove TRUE literals at Level > 0 (unsound).

### Preprocessing (Adaptive)
- **Tautology + duplicate-literal removal** (`removeTautologiesAndDuplicates`): Runs on ALL instances before the structure gate. O(total literals) with a temporary seen-array; removes tautological clauses and dedups repeated literals. Trivially sound.
- **Pure literal elimination** (`pureLiteralElimination`): Runs on ALL instances before the structure gate. Assigns variables appearing with one polarity only, removes satisfied clauses. Trivially sound (no clause contains the opposite polarity).
- **Structure classifier**: `analyzeInstanceStructure` uses `binaryRatio`, `ternaryRatio`, and `longClauseRatio`. Size score = `max(binaryScore, longScore)` — random k-SAT has 0% long clauses so only structured instances are reclassified. Structure classification runs BEFORE equivalence detection (equiv merges variables and removes tautologies, distorting ratios). StructuredScore ≥ 0.7: unit propagation enabled. Score < 0.7: unit propagation disabled (causes 76× more conflicts on random instances). Tautology/duplicate removal and PLE still run on all instances.
- **SCC-based equivalence detection**: Tarjan's SCC on the binary implication graph (built from binary clauses: (a∨b) gives edges ¬a→b, ¬b→a). Literals in the same SCC are equivalent; if l and ¬l are in the same SCC → UNSAT. Equivalent literals merged into a representative, mapping stored for model reconstruction (`extendModel()` after SAT). Merge significance gate: if `mergedCount*100 < numVars`, skip substitution (still detect l/¬l contradictions).
- **Dense-binary polarity gate**: Dense binary instances (binaryRatio>0.9 AND density>35) skip the occurrence-based initial phase override — their highly connected BIGs propagate to a solution in 0 conflicts with the default phase. Sparse binary instances (density<35) benefit from the override.

### VSIDS Heap
- Lazy heap with sink approach: `bumpLarge`/`bumpLBD` don't call `increaseKey` (O(1) bump). `selectVariableWithHeap` peeks at root; if assigned, sinks to -Inf (stays in heap, drops to bottom). `onUnassign` restores sunk entries' real scores and sifts up. `buildHeap` includes ALL variables (assigned at -Inf, unassigned at real score) — heap never empties, eliminating frequent O(n) rebuilds. Periodic refresh every 2000 conflicts fixes deeply stale entries. `restart()` forces a rebuild.
- O(1) decay via `varInc /= decayFactor` (MiniSat varInc trick). `decay()` does NOT touch the heap (relative ratios preserved). Rescale at `varInc > 1e100` sets `heapValid = false`.
- **Phase saving**: `savedPhase` initialized from occurrence counts (more frequent polarity wins). Updated in both `assignLiteral` (decisions) and `assignLiteralByClause` (propagations) via `s.savedPhase[varIdx] = lit.IsNegated()`.
- **LBD bonus scale**: Adaptive `max(10, 200000 × binaryRatio / numVars)`. Binary-heavy small instances get ~2000, long-clause or large instances get 10. CLI: `-lbd-scale N` (non-zero overrides adaptive; default=0=adaptive).
- `numUnassigned` field: O(1) allAssigned/hasUnassigned.

## Soundness Bugs to Never Re-introduce

- **pos=1 variable elimination**: Removed (~800 lines). The "pos=1 elimination" variant assumes positive clauses are *definitions* of a variable and reconstructs the eliminated variable's value from that assumption. In PHP instances, positive clauses are *constraints*, not definitions → returned SAT instead of UNSAT on php_6p_5h_unsat. Standard Davis-Putnam VE (resolve all (x∨A)×(¬x∨B) pairs, reconstruct by trying x=true then x=false) is sound — the ban is on the definitional variant, not standard VE.
- **Buggy subset-check subsumption elimination**: Removed (296 lines). The clause-subsumption check was implemented incorrectly — it determined clause A was subsumed by B when it wasn't, removing necessary clauses → false SAT. Forward subsumption (literal subset check) is sound — the ban is on the buggy implementation, not the algorithm. Re-implemented soundly as `subsumptionPass()` (original clauses, preprocessing) and `runLearnedSubsumption()` (learned clauses, restart boundaries — see C0).
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
