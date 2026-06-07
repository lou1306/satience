# Watched Literals Implementation Status

## Current State (June 2026)

**Watched literals: DISABLED** due to soundness bugs
**Linear propagation: ENABLED** and working correctly

## Performance Impact

With linear propagation (O(n) per clause):
- Sudoku: 100-1000x slower than MiniSat
- Tseitin: 10-50x slower
- Random instances: 5-20x slower
- PHP: Similar (both timeout)

With working watched literals (O(1) for binary/ternary):
- Expected: Within 2-5x of MiniSat on most instances

## Bug Description

Watched literals propagation incorrectly moves watches without detecting unit clauses, leading to:
- Variables decided instead of propagated
- Clauses like `[1 2 3]` having all literals FALSE without conflict
- Invalid models returned as SAT

### Minimal Failing Instance

```cnf
p cnf 6 5
1 2 3 0
4 5 6 0
-1 -4 0
-2 -5 0
-3 -6 0
```

Expected: SATISFIABLE (e.g., 1=T, 2=F, 3=F, 4=F, 5=T, 6=T)
Actual with watched literals: UNSATISFIABLE or invalid model

### Root Cause Analysis

The bug occurs in the watch replacement logic:

1. Clause `[1 2 3]` has watches on literals 1 and 2
2. Variable 1 is decided FALSE at level 1
3. Watch triggers, scans for replacement among literals 2 and 3
4. Finds literal 3 with `level=0` (unassigned) but `value=false` (stale from previous assignment)
5. Moves watch to literal 3, NO propagation occurs
6. Variable 2 is decided FALSE at level 2
7. Variable 3 is decided FALSE at level 3
8. All three literals FALSE, no conflict detected!

The issue is that after backtracking:
- `assignments[var].Level` is correctly reset to 0
- `assignments[var].Value` retains stale value (false by default)
- Watch logic checks `level == 0` to detect unassigned
- But doesn't verify the value is meaningful

## Attempted Fixes

### Attempt 1: Count unassigned literals
Counted unassigned literals before moving watches. Failed because:
- Counting included both watched literals
- Didn't account for watches moving during processing
- Still missed unit propagation opportunities

### Attempt 2: Check all literals for unit detection
Scanned all literals to detect unit clauses. Failed because:
- Watches processed one at a time
- By time unit detected, other decisions already made
- Race condition between watch processing and decisions

## Recommended Fix Strategy

### Phase 1: Simplify and Verify (2-3 days)
1. Implement watched literals ONLY for binary clauses
2. Keep linear propagation for ternary and longer clauses
3. Add extensive invariant checking:
   - Every clause must have at least one non-false watched literal
   - Unit clauses must trigger propagation immediately
   - Conflicts must be detected when both watches false
4. Test on minimal instance until 100% correct

### Phase 2: Extend to Ternary (2-3 days)
1. Add ternary clause support
2. Maintain invariants from Phase 1
3. Test on tseitin instances (ternary-heavy)

### Phase 3: Full Implementation (3-5 days)
1. Extend to all clause lengths
2. Optimize watch movement
3. Benchmark against MiniSat

## Alternative: Use Existing Implementation

Consider using a proven watched literals implementation:
- MiniSat (C++): Reference implementation
- CaDiCaL (C++): Modern, well-tested
- Go-SAT: Go implementation (if available)

Extract watched literals code and integrate with our CDCL engine.

## Testing Strategy

For each phase:
1. Run on minimal failing instance
2. Verify with model checking
3. Compare propagation count with linear version
4. Test on 100+ random instances
5. Benchmark on sudoku, tseitin, PHP

## Current Priority

**LOW** - Solver is sound and correct with linear propagation.
Watched literals is an optimization, not a correctness feature.

Focus on:
1. Ensuring model verification catches any bugs
2. Optimizing other areas (clause database, heuristics)
3. Adding features (inprocessing, better preprocessing)

Revisit watched literals when:
- Have dedicated debugging time (1-2 weeks)
- Can compare with reference implementation
- Performance becomes critical bottleneck
