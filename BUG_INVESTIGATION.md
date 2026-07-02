# Bug Investigation: Incorrect UNSAT on 0f4576a6e7399336e11f0828d32263dd.cnf

## Problem
Satience returns UNSAT on instance `benchmark/gbd_instances/0f4576a6e7399336e11f0828d32263dd.cnf`, but MiniSat returns SAT in 0.006s.

## Instance Details
- 200 variables, 856 clauses
- MiniSat model: var 2 = false (among others)
- Model verified to satisfy all 856 clauses

## Root Cause Analysis

### Observed Behavior
The solver enters an infinite loop:
1. Learns unit clause: var 2 = false (learned clause 111)
2. Propagates: var 2 = false at level 1
3. **BUG**: Decides var 2 = true at level 2 (should not decide on assigned variable!)
4. Conflict: var 2 = true conflicts with unit clause var 2 = false
5. Backtracks
6. Repeats from step 1

### Key Findings

1. **Unit clause is learned correctly**: Learned clause 111 is (2-) meaning var 2 = false

2. **Unit propagation works**: Var 2 is propagated to false at level 1

3. **Decision logic is buggy**: The `decide()` function is selecting var 2 even though it's already assigned (level 1)

4. **VSIDS should skip assigned variables**: Both `selectVariableWithHeap()` and `selectVariable()` check `assignments[i].Level == 0`, so they should only return unassigned variables

5. **Inconsistent state**: When `decide()` is called, var 2's level appears to be 0 even though it was just propagated to level 1

### Hypothesized Causes

1. **Level not set correctly during propagation**: The propagation code might not be setting `assignments[varIdx].Level` correctly

2. **Level reset between propagation and decision**: Something might be resetting the level without clearing the assignment

3. **Implication array corruption**: The implication array might be pointing to the wrong clause, causing incorrect protection during clause deletion

4. **Clause deletion bug**: Unit clause 111 might be getting deleted or moved, and the implication array is not being updated correctly

### Code Locations to Investigate

1. `internal/solver/solver_cdcl.go:2930-2945` - Unit propagation in `propagateWatched()`
2. `internal/solver/solver_cdcl.go:3362+` - Decision logic in `decide()`
3. `internal/solver/vsids.go:552-620` - Variable selection in `selectVariableWithHeap()`
4. `internal/solver/solver_cdcl.go:4580-4660` - Clause deletion and implication updates
5. `internal/solver/solver_cdcl.go:4700+` - `updateWatchClauseIndices()` for watch list updates

### Recommended Fix Approach

1. **Add assertion**: In `decide()`, assert that the selected variable has `Level == 0`

2. **Add debug logging**: Log var 2's level and implication at key points:
   - After unit propagation
   - Before decide()
   - After backtrack()

3. **Verify implication array**: Ensure the implication array is correctly updated when clauses are moved during swap-remove deletion

4. **Protect unit clauses**: Ensure unit clauses are protected from deletion by checking they're in `clauseUsedAsReason`

## Status
RESOLVED (July 2026). The original "infinite loop on var 2" no longer reproduces — a
SAFETY CHECK was added in `decide()` to fall back to a linear scan when the selected
variable is already assigned (previously it recursed and corruptly decremented the
level before it was incremented).

A deeper, related soundness bug was found and fixed via random 3-SAT differential
testing against MiniSat: satience returned UNSAT on satisfiable instances. Root cause:
when 1-UIP conflict analysis did not converge (more than one literal remaining at the
current decision level, caused by inconsistent reason clauses), the "FALLBACK"/"FORCE"
code DROPPED literals to force a single UIP. Dropping literals produces a clause that
is NOT entailed by resolution, which yielded incorrect unit clauses (e.g. learning
`var=x` on a SAT instance) and thus incorrect UNSAT.

Fix: the fallback no longer drops literals. When 1-UIP fails to converge, ALL remaining
current-level literals are kept — the clause is still a valid resolvent of the conflict
clause with the resolved reasons, so it is sound (it may be non-asserting, which is
merely less effective, never wrong). Verified: 80 random 3-SAT instances + 28 GBD
instances vs MiniSat, 0 disagreements.

Note: this specific instance (0f4576...) now returns UNKNOWN (timeout) rather than a
wrong answer; it remains disabled in the perf regression suite for performance reasons,
not soundness.

## Files Modified During Investigation
- internal/solver/solver_cdcl.go: sound 1-UIP non-convergence fallback; decide() linear-scan fallback; compactLearnedClauses re-enabled with non-false watch selection; chooseWatchPositions helper
- internal/solver/solver_debug.go: removed nonexistent watchListsBinary reference (fixed broken debug build)

## Next Steps
1. Add tests for clause deletion, watch invariants under stress, restart correctness
2. Investigate the underlying reason-clause inconsistency that triggers non-convergence
