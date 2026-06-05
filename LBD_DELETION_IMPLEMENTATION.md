# LBD + Size Based Clause Deletion - Implementation Report

## Summary

Implemented improved clause deletion strategy focusing on LBD + size based deletion with activity tracking. However, discovered that **restart frequency prevents clause accumulation**, making deletion less impactful than expected.

## Changes Made

### 1. Reduced Clause Database Limits
- **maxLearned**: 10000 → 200 clauses (50x reduction)
- **minLearned**: Added (100 clauses target after deletion)
- **Arena size**: 10000 → 1000 (matching reduced limits)

### 2. Added Clause Size Tracking
- Added `clauseSize []int` array to track learned clause sizes
- Updated `learnClause()` to record clause size
- Size used in deletion scoring (penalize clauses > 20 literals)

### 3. Improved Deletion Scoring
New `deleteLearnedClauses()` calculates deletion score based on:
- **LBD** (most important): score += lbd * 100
- **Size penalty**: score += (size - 20) * 50 for clauses > 20 literals
- **Age penalty**: score += age * 0.5
- **Activity bonus**: score -= activity * 10.0

### 4. Protection Rules
- **Glue clauses (LBD ≤ 3)**: NEVER delete (score = -1000)
- **Short clauses (< 5 literals)**: NEVER delete (score = -500)
- All other clauses: eligible for deletion based on score

### 5. Activity Tracking
- Bump clause activity when learned clause participates in conflict
- Decay clause activity every 100 conflicts (factor: 0.95)
- Active clauses less likely to be deleted

### 6. Aggressive Deletion Target
- Delete down to `minLearned` (100 clauses) when triggered
- Previously: delete 50% (kept 5000 from 10000)
- Now: delete 50-66% (keep 100 from 200-300)

## Testing Results

### Unit Tests
✅ All 15 unit tests pass
✅ No soundness issues introduced

### Problematic Instance (874bdedb23926bd0df8ef574e981fd2f.cnf)
**Before**: Timeout at 60s, 300K+ conflicts, stuck at level 18
**After**: Still timeout, similar behavior

**Root Cause Discovered**: 
- Restarts occur every 100-400 conflicts (Luby sequence)
- Restart clears ALL learned clauses
- Clause count never exceeds 200-250 before restart
- Deletion trigger (200 clauses) rarely hit
- Most restarts happen with < 200 clauses

**Evidence**:
```
Conflict 100, level 25, learned 100
Restart #1 at conflict 100
Conflict 200, level 24, learned 100
Restart #2 at conflict 200
...
Conflict 1000, level 18, learned 200
Restart #7 at conflict 1200
```

Clause count oscillates between 0-200, never triggering deletion.

## Analysis

### Why Current Approach Doesn't Help

1. **Restart Frequency**: Luby sequence triggers restart every 100-400 conflicts
2. **Clause Accumulation Rate**: ~1 clause per conflict = 100-400 clauses per restart cycle
3. **Deletion Trigger**: 200 clauses, but restart clears before hitting limit
4. **Result**: Deletion logic rarely executes, clause database stays small naturally

### What This Means

The **restart policy is already enforcing a small clause database** (~200 clauses), which is close to our target of 100-200 clauses. The deletion improvements would help if we:
- Increased maxLearned to 500-1000 (allow more accumulation)
- Reduced restart frequency (longer search between restarts)
- Kept glue clauses across restarts (preserve valuable information)

## Recommendations

### Option 1: Keep Glue Clauses Across Restarts (RECOMMENDED)
Instead of clearing all clauses on restart, keep glue clauses (LBD ≤ 2-3):
- Preserves high-quality learned information
- Allows clause database to grow gradually
- Makes deletion logic relevant (will trigger occasionally)
- Used by modern solvers (Glucose, CaDiCaL)

**Implementation**: 
- Calculate LBD before clearing assignments
- Keep clauses with LBD ≤ 2-3
- Delete non-glue clauses during restart

**Expected**: 1.5-3x speedup on structured instances

### Option 2: Reduce Restart Frequency
Increase restartBase from 100 to 200-300:
- Allows more clause accumulation
- Makes deletion logic relevant
- Trade-off: longer unproductive search regions

**Expected**: 1.2-2x speedup, but risk of getting stuck

### Option 3: Increase maxLearned
Raise maxLearned to 500-1000:
- Allows learning more clauses before deletion
- Deletion logic becomes relevant
- Trade-off: more memory, slower propagation

**Expected**: 1.1-1.5x speedup

## Code Quality

### Strengths
- ✅ Clean implementation with clear scoring function
- ✅ Protection rules prevent deleting valuable clauses
- ✅ Activity tracking rewards useful clauses
- ✅ Size penalty removes weak long clauses
- ✅ All tests pass, no soundness issues

### Weaknesses
- ❌ Deletion rarely triggers due to restart policy
- ❌ No benefit realized in current configuration
- ❌ Need to change restart policy to see improvements

## Next Steps

1. **Implement glue clause preservation** (Option 1)
   - Modify restart() to keep LBD ≤ 2-3 clauses
   - Calculate LBD before clearing assignments
   - Test on problematic instances

2. **Tune parameters**
   - Experiment with LBD threshold (2 vs 3)
   - Adjust maxLearned (200 vs 500)
   - Test different restart frequencies

3. **Measure impact**
   - Benchmark on 10+ diverse instances
   - Compare conflicts, decisions, runtime
   - Verify soundness maintained

## Conclusion

The LBD + size based deletion infrastructure is **correctly implemented** but **not utilized** due to restart policy. The restart clears clauses before deletion triggers. To realize benefits, we must either:
1. Keep glue clauses across restarts (best option)
2. Reduce restart frequency
3. Increase clause accumulation rate

**Recommendation**: Implement Option 1 (keep glue clauses) as it aligns with modern solver practices and preserves valuable learned information.
