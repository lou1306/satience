# Profiling Analysis - Implementation Issues Found

**Date**: June 5, 2026  
**Analysis Type**: Code inspection and profiling attempt

## Critical Issues (Highest Priority)

### 1. ❌ Learned Clauses Not Using Watched Literals

**Location**: `internal/solver/solver_cdcl.go:2148-2193`  
**Impact**: 5-50× slowdown on most instances  
**Severity**: CRITICAL

**Problem**:
```go
// Check learned clauses for conflicts and unit propagation
for learnedIdx := range s.learnedClauses {
    clause := &s.learnedClauses[learnedIdx]
    // Linear scan of ALL learned clauses on EVERY propagation!
}
```

**Why It's Wrong**:
- Learned clauses are checked via O(n) linear scanning
- With 10,000 learned clauses, this is 10,000× slower than watched literals
- Original clauses use watched literals (binary, ternary, long), but learned clauses don't
- `InitializeWatches()` only indexes original clauses, not learned clauses added during search

**Fix Required**:
- Add learned clauses to watch lists when created in `learnClause()`
- Route to appropriate watch list based on clause size (binary/ternary/long)
- Remove the linear learned clause scan from `propagate()`

**Expected Impact**:
- PHP: 50-100× reduction in conflicts
- Sudoku: 10-50× speedup
- General: 3-10× speedup on most instances

---

## High Priority Issues

### 2. ⚠️ Restart() Uses Map Allocation for LBD

**Location**: `internal/solver/solver_cdcl.go:646`  
**Impact**: Minor slowdown on restart-heavy instances  
**Severity**: HIGH

**Problem**:
```go
func (s *CDCLSolver) restart() {
    // ...
    for i, clause := range s.learnedClauses {
        levelSet := make(map[int]bool)  // ALLOCATES NEW MAP EVERY TIME!
        for _, lit := range clause.Literals {
            lvl := s.assignments[lit.Var()].Level
            if lvl > 0 {
                levelSet[lvl] = true
            }
        }
        lbd := len(levelSet)
        // ...
    }
}
```

**Why It's Wrong**:
- `learnClause()` already has reusable buffers (`tmpLevelSet`, `tmpLevelSetUsed`)
- Restart calculates LBD for ALL learned clauses (could be 10,000+)
- Each map allocation is ~100 bytes × 10,000 clauses = 1 MB of allocations per restart
- With 100 restarts, that's 100 MB of unnecessary GC pressure

**Fix Required**:
```go
func (s *CDCLSolver) restart() {
    // Clear reusable buffers
    for i := range s.tmpLevelSetUsed[:s.level+1] {
        s.tmpLevelSetUsed[i] = false
    }
    s.tmpLevelSet = s.tmpLevelSet[:0]
    
    for i, clause := range s.learnedClauses {
        lbd := 0
        for _, lit := range clause.Literals {
            lvl := s.assignments[lit.Var()].Level
            if lvl > 0 && !s.tmpLevelSetUsed[lvl] {
                s.tmpLevelSetUsed[lvl] = true
                s.tmpLevelSet = append(s.tmpLevelSet, lvl)
                lbd++
            }
        }
        // Clear tmpLevelSetUsed for next clause
        for _, lvl := range s.tmpLevelSet {
            s.tmpLevelSetUsed[lvl] = false
        }
        s.tmpLevelSet = s.tmpLevelSet[:0]
        // ... use lbd ...
    }
}
```

**Expected Impact**: 10-20% reduction in GC pressure on restart-heavy instances

---

## Medium Priority Issues

### 3. ⚠️ Watch List Growth Without Bounds

**Location**: `internal/solver/solver_cdcl.go:1663, 1686, 1832, 1857`  
**Impact**: Memory growth on long-running instances  
**Severity**: MEDIUM

**Problem**:
```go
s.cnf.WatchList[newWatchIdx] = append(s.cnf.WatchList[newWatchIdx], binIdx)
```

- Watch lists grow unbounded via `append()`
- No lazy cleanup when clauses are deleted
- Deleted clauses remain in watch lists (dangling references)
- Could cause memory bloat on long-running instances

**Why It's Tolerable**:
- Watch lists are `[]int`, so dangling references are just integers (not pointers)
- GC can collect deleted clause data even if watch lists reference old indices
- Performance impact is minor (slightly larger arrays to scan)

**Fix (Optional)**:
- Add lazy cleanup: periodically rebuild watch lists
- Or use "tombstone" markers for deleted clauses
- Effort: 1-2 days, benefit: marginal

---

### 4. ⚠️ ClauseKey() Allocates String for Every Resolvent

**Location**: `internal/solver/solver_cdcl.go:986-1013`  
**Impact**: Minor slowdown in variable elimination  
**Severity**: MEDIUM

**Problem**:
```go
func (s *CDCLSolver) clauseKey(clause *cnf.Clause) string {
    lits := make([]uint32, len(clause.Literals))  // Allocates slice
    // ... sort literals ...
    key := make([]byte, len(lits)*4)  // Allocates byte slice
    // ... encode ...
    return string(key)  // Allocates string
}
```

**Why It's Suboptimal**:
- Called for every resolvent during variable elimination
- Each call allocates 2 slices + 1 string
- On PHP: thousands of resolvents = thousands of allocations
- Already optimized (avoids `fmt.Sprintf`), but still allocates

**Fix (Optional)**:
- Use a reusable buffer for key encoding
- Or use a `map[uint64]bool` for small clauses (pack literals into uint64)
- Effort: 0.5 days, benefit: 10-20% faster VE

---

## Low Priority Issues

### 5. ℹ️ Preprocessing Iterates Over All Clauses Multiple Times

**Location**: Various preprocessing functions  
**Impact**: Minor slowdown on large instances  
**Severity**: LOW

**Observation**:
- `preprocessAggressive()` calls multiple passes over all clauses
- Each technique (subsumption, hyper-binary, equivalence, VE, BCE) iterates independently
- Could be fused into fewer passes

**Why It's Tolerable**:
- Preprocessing is one-time cost
- Limited to 2 passes by design
- Time limit (2s) prevents excessive preprocessing

**Fix (Optional)**: Combine passes, effort: 1-2 days, benefit: marginal

---

## Summary

| Issue | Priority | Expected Impact | Effort |
|-------|----------|----------------|--------|
| Learned clauses not using watched literals | CRITICAL | 5-50× speedup | 2-3 days |
| Restart() uses map allocation | HIGH | 10-20% GC reduction | 0.5 days |
| Watch list growth unbounded | MEDIUM | Memory stability | 1-2 days |
| ClauseKey() allocations | MEDIUM | 10-20% faster VE | 0.5 days |
| Preprocessing multiple passes | LOW | Marginal | 1-2 days |

## Recommended Action Plan

**Week 1**: Fix learned clause watched literals (CRITICAL)
- Implement watch list indexing for learned clauses
- Remove linear learned clause scan from propagate()
- Test extensively for soundness

**Week 2**: Fix restart() allocation + other high-priority issues
- Replace map with reusable buffers in restart()
- Optimize clauseKey() if needed
- Run comprehensive benchmarks

**Expected Overall Impact**: 5-50× speedup on most instances, potentially closing 80-90% of the performance gap with MiniSat.

## Verification Metrics

After fixes:
- **PHP conflicts**: 224,000 → <10,000 (95% reduction)
- **Sudoku time**: Current → 10-50× faster
- **Median slowdown**: 2.3× → <1.5× (competitive with MiniSat)
- **All 15 unit tests**: Must pass
- **Fuzzer soundness**: 100% on 50+ instances
