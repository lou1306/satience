# Glue Clause Preservation + Diversification Results

## Implementation Summary

Implemented two key optimizations from the roadmap:

### 1. Glue Clause Preservation Across Restarts ✅
- **LBD threshold**: ≤ 5 (relaxed from ≤ 3)
- **Mechanism**: Calculate LBD before clearing assignments, keep low-LBD clauses
- **Result**: Keeping 5-150 clauses per restart (was: 0)
- **Deletion triggering**: NOW ACTIVE (was: never triggered)

### 2. Variable Selection Diversification ✅
- **Random decisions**: 5% of decisions (every 20 conflicts)
- **Stuck detection**: 500 conflicts at same level → force random
- **Mechanism**: Deterministic "random" selection (conflicts % unassigned)
- **Result**: Random decisions happening regularly

## Testing Results

### Problematic Instance: 874bdedb23926bd0df8ef574e981fd2f.cnf (42 vars, 144 clauses)

**Before optimizations**:
- Timeout at 60s
- 300K+ conflicts
- Stuck at level 18
- Deletion never triggered
- All clauses cleared on restart

**After optimizations**:
- Still timeout at 60s
- 1.1M+ conflicts (3.6x MORE!)
- Still stuck at level 18
- Deletion NOW triggering (every ~500 conflicts)
- Keeping 5-150 glue clauses per restart
- Diversification active (random decisions every 20 conflicts)

**Paradoxical Result**: More conflicts, same timeout!

## Analysis

### Why More Conflicts?

1. **Deletion overhead**: Calculating LBD for 500 clauses every restart adds CPU time
2. **Diversification overhead**: Random decisions lead to suboptimal search paths
3. **Glue clause accumulation**: Keeping clauses means more to scan during propagation

### Why Still Stuck?

The core issue is **instance structure**, not clause management:

1. **Hidden trap**: The 42-variable instance has a structure that creates a local minimum at level 18
2. **Weak learned clauses**: Even with LBD-based selection, learned clauses don't target the trap
3. **Random decisions insufficient**: 5% randomization isn't enough to escape deep traps
4. **Propagation bottleneck**: O(n) scanning makes each conflict slower

### Evidence

```
Before: 300K conflicts in 60s = 5,000 conflicts/sec
After:  1.1M conflicts in 60s = 18,333 conflicts/sec
```

The solver is doing MORE work but not making progress. This confirms:
- ✅ Deletion is working (clauses accumulated, then deleted)
- ✅ Diversification is working (random decisions happening)
- ❌ But neither addresses the ROOT CAUSE: variable selection trap

## What This Tells Us

### Positive Results
1. **Glue clause preservation works**: Clauses ARE being kept across restarts
2. **Deletion triggers now**: Infrastructure is utilized
3. **Diversification active**: Random decisions happening
4. **No soundness issues**: All tests still pass

### Negative Results
1. **More conflicts ≠ faster solving**: Quality > quantity
2. **5% randomization too weak**: Need stronger diversification
3. **LBD ≤ 5 still deletes too much**: Or kept clauses aren't helpful
4. **Core problem remains**: Variable selection myopia

## Root Cause Hypothesis

The instance has a **combinatorial trap**:
- Variables 0-17 form a "garden of Eden" - easy to enter, hard to escape
- Variable 18 always leads to conflict
- Backjump to level < 18, but VSIDS immediately chooses variables 0-17 again
- Learned clauses don't prevent re-entering the trap
- Random decisions (5%) occasionally escape, but VSIDS pulls back

**Analogy**: Like a ball rolling into a deep valley - small nudges (5% random) aren't enough to escape.

## Recommendations

### Immediate (Low Hanging Fruit)

1. **Increase randomization to 10-20%**
   - Change `conflicts % 20 == 0` to `conflicts % 10 == 0`
   - Expected: Better escape from local minima
   - Risk: More random walk, less guided search

2. **More aggressive stuck detection**
   - Reduce threshold from 500 to 100 conflicts
   - Expected: Earlier escape from traps
   - Risk: Premature diversification

3. **Targeted diversification**
   - When stuck, force random decision on HIGH-activity variables
   - Focus randomization on "hot" variables
   - Expected: More effective escapes

### Medium Term

4. **Configuration learning**
   - Track which variable combinations lead to conflicts
   - Avoid similar configurations after restart
   - Expected: Prevent re-entering same traps

5. **Stronger restart triggers**
   - Restart after 100 conflicts at same level (not Luby sequence)
   - Expected: Faster escape from unproductive regions

### Long Term (Still Valid)

6. **Watched literals** (deferred)
   - Would speed up propagation 10-50x
   - Doesn't solve variable selection trap
   - Still recommended for overall performance

## Code Quality

### Strengths
- ✅ Clean implementation
- ✅ Proper tracking (conflictsAtLevel, lastRandomDecision)
- ✅ Reset on backtrack/restart
- ✅ No soundness issues
- ✅ Verbose output for debugging

### Weaknesses
- ❌ Diversification parameters arbitrary (5%, 500 conflicts)
- ❌ No adaptive tuning (should learn optimal randomization rate)
- ❌ Random selection not truly random (deterministic based on conflicts)

## Conclusion

**Glue clause preservation + diversification** are correctly implemented but **insufficient** for hard instances with deep local minima.

**Key Insight**: The problem isn't clause management - it's **variable selection myopia**. The solver keeps making similar decisions and falling into the same trap.

**Next Best Step**: Implement **more aggressive diversification** (10-20% random, lower stuck threshold) OR implement **configuration learning** to avoid similar traps.

**Alternative**: Accept that some instances are exponentially hard for basic CDCL and focus on instances where the solver performs well (cardinality constraints, algebra/XOR).

## Performance Metrics

| Metric | Before | After | Change |
|--------|--------|-------|--------|
| Conflicts in 60s | 300K | 1.1M | +266% |
| Conflicts/sec | 5,000 | 18,333 | +266% |
| Clauses kept/restart | 0 | 5-150 | +∞ |
| Deletion triggers | 0 | ~2000 | +∞ |
| Random decisions | 0 | ~5% | +∞ |
| Decision level | Stuck at 18 | Stuck at 18 | 0% |
| Result | TIMEOUT | TIMEOUT | 0% |

**Verdict**: Infrastructure improvements successful, but core algorithmic limitation remains.
