# Watched Literals Implementation Status

## Current Status (June 2026 - Updated)

**Watched literals is DISABLED** due to severe performance bugs. The solver uses linear propagation which is correct but O(n) per propagation.

### Latest Attempt (June 8, 2026)

**Changes Made**:
1. ✅ Fixed trail processing to start from index 0 (not `trailHead[s.level]`)
2. ✅ Removed trail restart logic (process sequentially like MiniSat)
3. ✅ Enabled `watchInitialized = true`
4. ✅ Added pre-allocation for watch lists

**Results**:
- ❌ **100× slower** than linear propagation on PHP instances
- ❌ PHP 6p5h: 0.4s with watched literals vs 0.004s with linear propagation
- ❌ Root cause: Go slice operations (`append`) during watch movement cause massive allocation overhead

**Conclusion**: The trail processing correctness bug is FIXED, but performance is unacceptable. Go's slice-based data structures are not suitable for high-performance watched literals without significant optimization (pools, swap-remove, pre-allocation).

## Implementation Attempts

### Attempt 1: Full Watched Literals (Commit ef53ebd)
- Implemented complete watched literals following MiniSat design
- Added watch data structures, initWatches(), propagateWatched()
- Added verification framework (VerifyWatchInvariants)
- **Result**: Missed conflicts, 706K decisions vs 96 expected on PHP 6p5h
- **Root cause**: Watch movement logic incorrect - watches not properly removed from old watch lists

### Attempt 2: Debugging and Fixes
- Added comprehensive sanity checking (compare with linear propagation)
- Fixed watch list update logic (update once at end of loop, not during)
- Added learned clauses to watches when created
- **Result**: Still missed conflicts - only 4 trail elements processed instead of 30
- **Root cause discovered**: Trail processing skips propagations from lower levels

## Critical Bug Analysis

### Symptom
```
Propagation mismatch: watched=false, linear=true at conflict 0
Falsified clauses 71-80 (all binary) not detected by watched literals
```

### Root Cause
The watched literals implementation processes trail elements starting from `trailHead[s.level]`. This means:
- At level 4, only trail[18:19] is processed (1 element)
- Trail elements [0:17] (18 elements) are SKIPPED
- These skipped elements include propagations whose watches should be processed
- Conflicts in clauses watched by these literals are MISSED

### Why This Happens
When propagateWatched() processes a trail element and propagates new variables:
1. New variables are added to the trail
2. But the loop continues from the current trailIndex
3. Before all propagations are processed, decide() is called
4. New decision level created, trailHead updated
5. Next propagateWatched() call starts from NEW trailHead
6. OLD propagations are NEVER processed!

## Correct Design (for Future Implementation)

### Key Requirements
1. **Process ALL trail elements**: Don't skip propagations from lower levels
2. **Restart trail after propagation**: When a propagation occurs, restart from trailHead[level]
3. **Proper watch removal**: When moving watch from A to B, remove from A's list
4. **Watch ALL clauses**: Original and learned clauses must be watched
5. **Invariant checking**: Verify after every operation

### Correct Algorithm
```go
func propagateWatched() (bool, int) {
    trailIndex := trailHead[level]
    for trailIndex < len(trail) {
        lit := trail[trailIndex]
        trailIndex++
        
        falseLit := getFalseLiteral(lit)
        watchIdx := LitToIndex(falseLit)
        watches := watchLists[watchIdx]
        
        newWatchCount := 0
        for i := 0; i < len(watches); i++ {
            watch := watches[i]
            blit := getBlockingLiteral(watch)
            
            if literalIsTrue(blit) {
                watches[newWatchCount] = watch
                newWatchCount++
                continue
            }
            
            if isBinary(watch) {
                // Binary clause: propagate or conflict
                if assignment[blit.Var()].Level == 0 {
                    assignLiteral(blit, level, watch.ClauseID)
                    // CRITICAL: Restart trail processing!
                    trailIndex = trailHead[level]
                    break
                }
                if !literalIsTrue(blit) {
                    return CONFLICT, watch.ClauseID
                }
                watches[newWatchCount] = watch
                newWatchCount++
                continue
            }
            
            // Long clause: find replacement watch
            foundReplacement := false
            for _, clauseLit := range getClause(watch.ClauseID).Literals {
                if clauseLit != falseLit && clauseLit != blit {
                    if literalIsTrue(clauseLit) || isUnassigned(clauseLit) {
                        newWatchIdx := LitToIndex(clauseLit)
                        watchLists[newWatchIdx].append(Watch{watch.ClauseID, watchIdx})
                        foundReplacement = true
                        break
                    }
                }
            }
            
            if foundReplacement {
                // Don't add to newWatchCount (removes from this watch list)
                continue
            }
            
            // No replacement: check blocking literal
            if assignment[blit.Var()].Level == 0 {
                assignLiteral(blit, level, watch.ClauseID)
                watches[newWatchCount] = watch
                newWatchCount++
                watchLists[watchIdx] = watches[:newWatchCount]
                trailIndex = trailHead[level]  // CRITICAL: Restart!
                break
            }
            if !literalIsTrue(blit) {
                return CONFLICT, watch.ClauseID
            }
            watches[newWatchCount] = watch
            newWatchCount++
        }
        watchLists[watchIdx] = watches[:newWatchCount]
    }
    return NO_CONFLICT, -1
}
```

## Future Work Plan

### Phase 1: Minimal Correct Implementation (3-4 days)
1. Start with tiny instance (3 vars, 8 clauses)
2. Implement watched literals with extensive logging
3. Add invariant checking after EVERY operation
4. Compare with linear propagation after EVERY call
5. Debug until no mismatches

### Phase 2: Full Integration (2-3 days)
1. Extend to all benchmark instances
2. Ensure learned clauses are watched correctly
3. Test on PHP, Tseitin, Sudoku instances
4. Verify no performance regressions

### Phase 3: Optimization (2-3 days)
1. Pre-allocate watch lists
2. Optimize binary clause handling
3. Reduce allocation overhead
4. Profile and optimize hot paths

### Total Estimated Effort: 7-10 days

## Performance Expectations

With correct watched literals:
- **Propagation-heavy** (Sudoku, Tseitin): 10-50× speedup
- **Decision-heavy** (PHP, Algebra): 2-5× speedup
- **Overall median**: 5-10× speedup

Current linear propagation is the primary bottleneck for large instances.

## References
- MiniSat paper: "An Extensible SAT-solver" (Eén & Sörensson, 2003)
- MiniSat code: https://github.com/niklasso/minisat
- CaDiCaL: https://github.com/arminbiere/cadical
- Git history: commits ef53ebd, 1e964c6, 6a608b7, bcd1bba

## Conclusion

Watched literals is a critical optimization for SAT solvers, but implementing it correctly requires careful attention to:
- Watch movement and removal
- Trail processing order
- Learned clause integration
- Invariant maintenance

The current implementation has fundamental bugs that would take 7-10 days to fix properly. Given that linear propagation is correct (if slower), the pragmatic decision is to keep it enabled and revisit watched literals when time permits.
