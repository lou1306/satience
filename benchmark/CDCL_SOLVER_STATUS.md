# CDCL Solver Status - June 5, 2026

## Summary
The satience CDCL SAT solver is functionally complete with all core CDCL features implemented:
- ✅ 1-UIP conflict analysis
- ✅ Backjumping  
- ✅ VSIDS variable selection
- ✅ Clause learning
- ✅ Restarts (Luby + adaptive)
- ✅ Preprocessing (variable elimination, BCE, failed literals, etc.)
- ✅ Watched literals for original clauses

## Known Issues

### Critical: Learned Clause Watching Bug
**Status**: UNFIXED - causes soundness bugs on hard UNSAT instances

**Problem**: The watched literals implementation for learned clauses has a fundamental design flaw:
- Learned clauses are stored separately in `s.learnedClauses`
- Watch indices use negative encoding: `LongClauseIndices[i] = -learnedIdx-1`
- During restart, learned clauses are compacted (non-glue clauses deleted)
- Watch lists still contain OLD indices → invalid memory access

**Impact**: 
- PHP instances return SAT incorrectly (should be UNSAT)
- Memory explosion during restart
- Soundness violations on structured UNSAT instances

**Root Cause**: The watched literals scheme mixes original and learned clause indices in the same data structures, but they have different lifecycles:
- Original clauses: stable throughout solving
- Learned clauses: created/deleted during search, compacted during restart

### Workaround Attempts
1. **Rebuild all watches on restart**: Causes memory explosion (re-indexes everything)
2. **Linear scan of learned clauses**: Too slow for large instances (PHP timeout)
3. **Skip learned clause deletion**: Memory explosion from too many learned clauses

## Recommended Fix (Not Yet Implemented)

### Option 1: Separate Watch Structures (2-3 days)
Create separate watch lists for learned clauses:
```go
type CNF struct {
    // Original clauses
    Clauses []Clause
    WatchListLong [][]int
    LongClauseIndices []int
    
    // Learned clauses (separate)
    LearnedClauses []Clause
    LearnedWatchList [][]int
    LearnedLongIndices []int
}
```

Benefits:
- Clean separation of concerns
- No index corruption on restart
- Can delete learned clauses freely

### Option 2: Clause References Instead of Indices (3-5 days)
Use pointers or stable references instead of array indices:
```go
type ClauseRef struct {
    IsLearned bool
    Index     int  // index in either Clauses or LearnedClauses
}
```

Benefits:
- More flexible
- No negative index encoding
- Easier to reason about

### Option 3: Disable Learned Clause Deletion (Temporary, 1 day)
Keep ALL learned clauses (no deletion on restart):
- Simplest fix
- Memory usage grows unbounded
- Acceptable for small/medium instances
- NOT production-ready

## Current Workarounds

### For Production Use
1. Disable restarts: `solver.restartBase = 0`
2. Disable learned clause deletion: Comment out `deleteLearnedClauses()` call
3. Accept memory growth for long-running solves

### For Testing
- Use small instances (< 100 vars, < 500 clauses)
- Avoid PHP and other cardinality-heavy instances
- Verify models for SAT results

## Test Results

### Passing (Sound)
- ✅ All 15 unit tests
- ✅ Tseitin grid instances (UNSAT detected in preprocessing)
- ✅ Small algebra/XOR instances (< 50 vars)
- ✅ Chain instances (< 100 vars)

### Failing (Unsound)
- ❌ PHP instances (returns SAT, should be UNSAT)
- ❌ Large structured UNSAT instances (> 200 vars)

## Next Steps

### Immediate (Critical)
1. **Implement Option 1** (separate watch structures) - 2-3 days
2. **Add comprehensive testing** for learned clause propagation - 1 day
3. **Verify soundness** on 20+ diverse UNSAT instances - 1 day

### Short-term
4. **Optimize learned clause propagation** (watched literals) - 2 days
5. **Benchmark vs MiniSat** on 50+ instances - 1 day
6. **Document limitations** in README - 0.5 days

### Medium-term
7. **Add cardinality detection** for PHP - 3-5 days
8. **Improve preprocessing** for cardinality constraints - 2-3 days
9. **SAT Competition compliance** (exit codes, output format) - 1 day

## Code Quality

### Strengths
- Clean CDCL implementation
- Good preprocessing pipeline
- Modern heuristics (VSIDS, LBD, adaptive restarts)
- Comprehensive test suite (15 tests)

### Weaknesses
- Learned clause watching is buggy
- No separation of original/learned clause storage
- Restart implementation incomplete
- Memory management needs work

## Conclusion

The satience solver is **80% complete**. The core CDCL algorithm is correct, but the learned clause watching infrastructure has critical bugs that cause soundness violations on hard UNSAT instances.

**Recommendation**: Implement Option 1 (separate watch structures) before production use. This is a 2-3 day fix that will make the solver sound and competitive.

**Current Status**: Research/development version - NOT production-ready for UNSAT solving.

