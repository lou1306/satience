# Watched Literals Implementation Status

## Summary

**Status**: DISABLED (June 2026)

Watched literals propagation is currently disabled due to severe performance bugs. The solver uses linear clause scanning which is correct but O(n) per propagation.

## Implementation Attempts

### Attempt 1: Enable Existing Implementation (June 2026)

**Approach**: Enable the watched literals implementation from commit `ef53ebd` with trail processing fix.

**Changes Made**:
1. Enabled `watchInitialized = true` in `initWatches()`
2. Fixed trail processing to start from index 0 instead of `trailHead[s.level]`
3. Added watch list rebuilding after restart
4. Added learned clauses to watches immediately after creation

**Results**:
- ✅ **Sound**: All learned clauses were correct
- ✅ **22/23 tests passing**: All tests except PHP 6p5h
- ❌ **PHP 6p5h timeout**: Solver returns UNKNOWN instead of UNSAT
- ❌ **Severe performance degradation**: 1000× slower than linear propagation on PHP instances

**Root Cause Analysis**:

The watched literals implementation has two critical bugs:

1. **Trail Processing Bug** (FIXED): Starting from `trailHead[s.level]` skips propagations from lower levels, causing missed conflicts. Fixed by starting from trail index 0.

2. **Performance Bug** (UNFIXED): After fixing trail processing, the solver becomes extremely slow. Preliminary analysis suggests:
   - Excessive watch list scanning during propagation
   - Too many watch movements causing cache misses
   - Possible issue with watch list data structure (slice appends causing reallocations)

## Current Status

### Disabled Code Location

- **Initialization**: `solver_cdcl.go:334-355` (`initWatches()`)
- **Propagation**: `solver_cdcl.go:2168-2310` (`propagateWatched()`)
- **Restart handling**: `solver_cdcl.go:1010-1030` (watch list rebuilding)
- **Learned clause integration**: `solver_cdcl.go:2970-2975` (adding to watches)

### How to Re-enable

To experiment with watched literals, change line 351:
```go
s.watchInitialized = true  // Change from false to true
```

**Warning**: This will cause severe performance degradation on propagation-heavy instances.

## Performance Comparison

| Instance Type | Linear Propagation | Watched Literals | Expected |
|--------------|-------------------|------------------|----------|
| PHP 6p5h UNSAT | 0.3s | Timeout (UNKNOWN) | <0.1s |
| Tseitin 5×5 SAT | 0.01s | ~10s (estimated) | <0.01s |
| Algebra 20 SAT | 0.01s | Similar | Similar |

## Recommendations for Future Implementation

### Phase 1: Correctness (3-4 days)

1. **Start with minimal instance**: Test on 3-5 variable instances with step-by-step verification
2. **Add invariant checking**: Verify after every propagation that:
   - Every clause with ≥2 literals has exactly 2 watches
   - All watches point to literals IN their clauses
   - Blocking literals are in clauses
3. **Compare with linear propagation**: After every propagate() call, verify same result as linear scan
4. **Fix trail processing**: Ensure ALL trail elements are processed

### Phase 2: Performance Optimization (4-5 days)

1. **Profile hot paths**: Use `go test -cpuprofile` to identify bottlenecks
2. **Pre-allocate watch lists**: Avoid slice appends during propagation
   ```go
   // Instead of: s.watchLists[idx] = append(...)
   // Pre-allocate with capacity hints
   ```
3. **Optimize binary clause handling**: Special-case binary clauses to avoid scanning
4. **Reduce watch movements**: Only move watches when absolutely necessary
5. **Cache-friendly data structures**: Consider contiguous memory layout for watches

### Phase 3: Integration Testing (2-3 days)

1. **Benchmark suite**: Test on diverse instances (PHP, Tseitin, Sudoku, Algebra)
2. **Compare with MiniSat**: Validate performance is within 2-5× of MiniSat
3. **Stress testing**: Run on large instances (>10K clauses)

### Total Estimated Effort: 9-12 days

## Reference Implementation

The MiniSat watched literals algorithm:

```cpp
// From MiniSat paper (Eén & Sörensson, 2003)
lbool Solver::propagate() {
    int p_index = 0;
    Clause c;
    while (p_index < trail.size()) {
        Lit p = trail[p_index++];
        vec<Watch>& ws = watches[toInt(p)];
        Watch *i, *j, *end;
        for (i = j = ws.begin(), end = i + ws.size(); i != end;) {
            // Check if watching another literal
            // Move watch if needed
            // Propagate or detect conflict
        }
        ws.shrink(i - j);
    }
    return l_Undef;
}
```

Key insights:
- Uses pointer arithmetic for efficient watch list iteration
- Shrinks watch list in-place to remove moved watches
- Processes trail sequentially with single pass

## Known Issues

1. **Watch list reallocation**: Slice appends during watch movement cause memory allocations
2. **Excessive scanning**: Long clauses require O(n) scanning to find replacement watches
3. **Cache inefficiency**: Watch lists scattered in memory cause cache misses
4. **Trail restart overhead**: Restarting from 0 processes same literals multiple times

## Alternative Approaches

### Hybrid Approach (Recommended)

Use linear propagation for long clauses (>10 literals) and watched literals for short clauses:
- Short clauses benefit most from watched literals
- Long clauses rarely propagate, so linear scan is acceptable
- Reduces watch list overhead significantly

### Lazy Watch Movement

Only move watches when both watched literals become false:
- Reduces watch list updates
- Keeps watches stable during search
- Requires careful invariant maintenance

## Conclusion

Watched literals is a critical optimization for SAT solvers, but implementing it correctly requires careful attention to:
- Trail processing order
- Watch movement and removal
- Learned clause integration
- Performance optimization

The current implementation has the correct structure but suffers from performance bugs that make it slower than linear propagation. Future work should focus on profiling and optimizing the hot paths before re-enabling.

**Current Recommendation**: Keep watched literals disabled and use linear propagation until dedicated time is available for proper optimization.

---

*Last Updated: June 2026*
*Author: Implementation attempted during satience project*
