# Critical Performance Bug: Learned Clauses Not Using Watched Literals

## Issue Discovered

**Date**: June 5, 2026  
**Severity**: CRITICAL - Causes 100-1000× slowdown on propagation-heavy instances  
**Root Cause**: Learned clauses are checked via linear scanning instead of watched literals

## Evidence

In `internal/solver/solver_cdcl.go`, the `propagate()` function:

1. Uses watched literals for binary clauses (`propagateBinary()`)
2. Uses watched literals for ternary clauses (`propagateTernary()`)  
3. Uses watched literals for long clauses >4 literals (`propagateLong()`)
4. **Linearly scans ALL learned clauses** (line 2148: `for learnedIdx := range s.learnedClauses`)

This means:
- Every learned clause is checked on EVERY propagation step
- O(n) per propagation where n = number of learned clauses
- With 10,000 learned clauses, this is 10,000× slower than necessary

## Impact

### PHP Instances
- MiniSat: 251 conflicts, solves instantly
- Satience: 224,000+ conflicts, times out
- **900× more conflicts** is partly due to O(n) learned clause checking

### Sudoku (729 vars, 11,745 clauses)
- Even with ternary watched literals, learned clauses are scanned linearly
- Expected 10-50× speedup from ternary watches is lost to learned clause scanning

### General Performance
- Median 2.3× slower than MiniSat (excluding PHP)
- This bug likely contributes 2-10× overhead on most instances

## Why This Happened

The watched literals implementation was done incrementally:
1. Binary clauses (commit 8482570) ✅
2. Long clauses (commit f81231d) ✅
3. Ternary clauses (commit b5ea2a6) ✅
4. **Learned clauses: NEVER IMPLEMENTED** ❌

The code structure treats "learned clauses" as a separate category from "binary/ternary/long", but learned clauses can be ANY size (2, 3, 4, 10+ literals).

## Fix Required

### Option 1: Add Learned Clauses to Existing Watch Lists (Recommended)
- When learning a clause, add it to appropriate watch list based on size
- Binary learned clauses → `WatchList[]`
- Ternary learned clauses → `TernaryWatchList[]`
- Long learned clauses → `WatchListLong[]`
- Modify `propagate()` to skip the linear learned clause scan

**Effort**: 2-3 days  
**Risk**: Medium (soundness testing required)  
**Expected Impact**: 5-50× speedup on most instances

### Option 2: Separate Learned Clause Watch Lists
- Create `LearnedWatchList[]`, `LearnedTernaryWatchList[]`, etc.
- Keeps learned and original clauses separate (easier debugging)
- Slightly more memory overhead

**Effort**: 2-3 days  
**Risk**: Medium  
**Expected Impact**: Same as Option 1

### Option 3: Hybrid Approach
- Use watched literals for short learned clauses (≤4 literals)
- Linear scan only for long learned clauses (>4 literals)
- Simpler to implement, captures most of the benefit

**Effort**: 1-2 days  
**Risk**: Low  
**Expected Impact**: 3-10× speedup

## Verification Plan

After implementing the fix:

1. **Unit tests**: All 15 tests must pass
2. **Soundness**: Fuzzer with 50+ random instances, verify all SAT models
3. **Performance**: 
   - PHP: Expect 50-100× reduction in conflicts (224K → 2-4K)
   - Sudoku: Expect 10-50× speedup
   - Chain: Should remain fast (already optimized by VE)
4. **Memory**: Monitor for memory explosion (watched literals use more memory)

## Related Issues

This bug compounds with other issues:
- **Variable elimination disabled** (now re-enabled with limits) - helped preprocessing
- **Clause learning quality** - 900× conflicts suggests weak learned clauses
- **Go GC overhead** - 3-5× overhead on high-churn workloads

Fixing this bug should be the **highest priority** as it affects all instances.

## References

- MiniSat watched literals: [Moskewicz et al., 2001](https://www.eecis.udel.edu/~saunders/courses/889-06a/reading/minisat.pdf)
- SAT solver architecture: [Biere et al., Handbook of Satisfiability](https://www.frontiersinai.com/)
- Current propagate implementation: `internal/solver/solver_cdcl.go:2148-2193`
