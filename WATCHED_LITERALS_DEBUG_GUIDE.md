# Watched Literals Debugging Guide

## Current Status (June 2026)

### ✅ What Works
- Watch data structures (Watch struct, watch lists)
- Watch initialization after preprocessing
- Soundness framework (correct UNSAT detection when working)

### ❌ Known Bugs

#### Bug #1: Infinite Loop at Low Decision Levels
**Symptoms:**
- Solver oscillates between level 2-3
- Trail size stuck at 2-3
- Continuous conflicts without progress
- Example: tiny_debug.cnf (5 vars, 10 clauses)

**Suspected Root Cause:**
Watch maintenance logic in `propagateWatched()` is incorrect. Specifically:
1. Replacement watch finding may not scan all literals correctly
2. Watch list updates may corrupt the watch structure
3. Trail restart logic may not properly reset state

#### Bug #2: Performance Regression (when not looping)
**Symptoms:**
- 40x more conflicts than linear scanning (57K vs 1.3K on Sudoku)
- props/dec ratio = 1.1 (terrible)
- Search space exploration corrupted

## Debugging Strategy

### Step 1: Verify Watch Invariants

Add assertions to check these invariants after every propagation step:

```go
// Invariant 1: Every non-satisfied clause has exactly 2 watches
for clauseID := 0; clauseID < numClauses; clauseID++ {
    watchCount := 0
    for lit := 0; lit < numLits; lit++ {
        for _, w := range watchLists[lit] {
            if w.ClauseID == clauseID {
                watchCount++
            }
        }
    }
    if !clauseIsSatisfied(clauseID) && watchCount != 2 {
        panic(fmt.Sprintf("Clause %d has %d watches, expected 2", clauseID, watchCount))
    }
}

// Invariant 2: Watch literals are not false
for lit := 0; lit < numLits; lit++ {
    if literalIsFalse(lit) {
        for _, w := range watchLists[lit] {
            // This should never happen!
            panic(fmt.Sprintf("False literal %d is watching clause %d", lit, w.ClauseID))
        }
    }
}
```

### Step 2: Trace Watch Operations

Add detailed tracing to `propagateWatched()`:

```go
fmt.Printf("c [WATCH] Checking literal %d (false)\n", falseLit)
for i, watch := range watches {
    fmt.Printf("c   Watch %d: clause=%d, blit=%d, binary=%v\n", 
        i, watch.ClauseID, watch.Blit, watch.IsBinary)
    
    // After processing
    fmt.Printf("c   -> Action: %s\n", action)
}
```

### Step 3: Compare with Reference Implementation

**Reference: MiniSat/CaDiCaL propagate()**

Key differences to check:
1. **Watch selection**: CaDiCaL watches first TWO unassigned/false literals, not necessarily literals at positions 0,1
2. **Watch update**: When finding replacement, CaDiCaL swaps literals in the clause to keep watched literals at positions 0,1
3. **Binary clauses**: CaDiCaL never updates binary clause watches (they're fixed)

### Step 4: Test on Minimal Instances

Create minimal test cases:

```cnf
# Test 1: Simple propagation
p cnf 3 3
1 2 0
-1 2 0
-2 3 0
# Expected: propagate 2=true, then 3=true

# Test 2: Simple conflict
p cnf 2 3
1 2 0
1 -2 0
-1 0
# Expected: conflict after deciding 1=true

# Test 3: Watch replacement
p cnf 4 4
1 2 3 0
-1 2 3 0
1 -2 3 0
-3 4 0
# Expected: watch replacement when 3 becomes false
```

### Step 5: Check Specific Code Paths

#### Watch Initialization
```go
// Verify: watches are added to BOTH watched literals
// Verify: blit is set to the OTHER watched literal
// Verify: binary flag is set correctly
```

#### Watch Replacement
```go
// Current code APPENDS to new watch list
// Should it UPDATE in place?
// Are we skipping the old watch correctly?
```

#### Trail Restart
```go
// After propagation, trailIndex = s.trailHead[s.level]
// Does this correctly restart from beginning of current level?
// Are we missing propagations?
```

## Common Pitfalls

### Pitfall 1: Modifying Slice While Iterating
```go
watches := s.watchLists[watchIdx]
for i := 0; i < len(watches); i++ {
    // If we append to s.watchLists[...], does it affect watches?
    // Answer: Yes, if they share the same underlying array!
}
```

**Fix**: Use separate counter for new watches:
```go
newWatchCount := 0
for i := 0; i < len(watches); i++ {
    // ... process ...
    if keep {
        watches[newWatchCount] = watches[i]
        newWatchCount++
    }
}
watches = watches[:newWatchCount]
```

### Pitfall 2: Incorrect Literal Comparison
```go
if clauseLit == falseLit || clauseLit == blit {
    continue
}
// Are we comparing Literal to Literal?
// Or Literal to uint32 (watch index)?
```

**Fix**: Ensure consistent types:
```go
blit := cnf.IndexToLit(int(blitIdx))  // Convert to Literal
if clauseLit == falseLit || clauseLit == blit {
    continue
}
```

### Pitfall 3: Missing Watch Removal
When adding a replacement watch, the old watch must be removed:
```go
// WRONG: Just append new watch
s.watchLists[newWatchIdx] = append(...)
// Old watch still in watchLists[watchIdx]!

// RIGHT: Don't increment newWatchCount, so old watch is removed
if foundReplacement {
    s.watchLists[newWatchIdx] = append(...)
    // Don't do: watches[newWatchCount] = watch
    // Don't do: newWatchCount++
}
```

## Re-implementation Plan

### Phase 1: CaDiCaL-Style Watch Maintenance (2-3 days)

1. **Store watches differently**: Instead of separate Watch struct, store watch indices directly in Clause
2. **Swap literals**: When updating watch, swap literals in clause to keep watched literals at positions 0,1
3. **Simplify propagate()**: Follow CaDiCaL's propagate() exactly

### Phase 2: Incremental Testing (1-2 days)

1. Test on 3-variable instances
2. Add invariant assertions
3. Compare propagation traces with MiniSat

### Phase 3: Performance Validation (1 day)

1. Benchmark on Sudoku
2. Verify props/dec ratio improves to 5-10
3. Verify conflict count matches MiniSat

## References

- CaDiCaL: https://github.com/arminbiere/cadical
  - See `src/watch.hpp` and `src/propagate.cpp`
- MiniSat: http://minisat.se/
  - See `Core/Solver.C`
- SAT Solvers Book (Biere et al.): Chapter on CDCL

## Current Recommendation

**Use linear scanning for production**. Watched literals infrastructure is committed but needs 3-5 days of focused debugging. The solver is sound and complete with linear scanning - watched literals is a performance optimization, not a correctness requirement.
