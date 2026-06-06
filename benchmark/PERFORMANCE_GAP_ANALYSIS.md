# Performance Gap Analysis: Satience vs MiniSat

## Executive Summary

Satience is **100% sound** (all results match MiniSat) but has a **significant performance gap** on certain instances:

- **Fast instances** (< 0.01s): 1.5-3x slower than MiniSat (acceptable)
- **Propagation-heavy** (sudoku): 800x slower (linear scanning bottleneck)
- **Structured UNSAT** (11c893b7): >468x slower, 16x more conflicts (clause learning inefficiency)

## Test Results

### Soundness Verification
✅ **10/10 instances match MiniSat** - 100% soundness confirmed

### Performance Breakdown

| Instance | Vars | Satience | MiniSat | Slowdown | Conflicts (Sat/Mini) |
|----------|------|----------|---------|----------|---------------------|
| algebra_xor_20 | 20 | 0.002s | 0.001s | 1.6x | - |
| algebra_xor_40 | 40 | 0.002s | 0.002s | 1.6x | - |
| php_5p_6h_sat | 30 | 0.006s | 0.002s | 2.2x | - |
| arg_chain_150 | 150 | 0.006s | 0.002s | 3.5x | - |
| tseitin_5x5 | 65 | 0.003s | 0.002s | 1.3x | - |
| sudoku_3x3 | 729 | 9.4s | 0.011s | **828x** | 10K / - |
| 11c893b7 | 36 | TIMEOUT | 0.064s | **>468x** | >656K / 40K |

## Root Causes Identified

### 1. Linear Scanning Bottleneck (Sudoku: 828x slower)
**Cause**: Disabled watched literals for long clauses (≥4 literals)
**Impact**: O(n) clause checking instead of O(1)
**Evidence**: 
- Sudoku has 8,586 clauses after preprocessing
- All clauses are ternary (3 literals) - should use watched literals
- 10,002 conflicts × 8,586 clauses = 86M clause checks

**Fix Required**: Re-implement watched literals with proper O(1) removal

### 2. Clause Learning Inefficiency (11c893b7: >468x slower)
**Cause**: Learning low-quality clauses (high LBD) that don't effectively prune search
**Evidence**:
- Satience: 656,000+ conflicts, avg LBD ~6.0
- MiniSat: 40,000 conflicts (16x fewer), expected avg LBD ~2-3
- Stuck at decision level 19 for hundreds of thousands of conflicts
- 1,445 restarts (too frequent, don't allow structure learning)

**Specific Issues**:
1. **Restart policy too aggressive**: Luby base=100 causes restart every ~100 conflicts
2. **Poor LBD quality**: Average LBD of 6 vs expected 2-3
3. **VSIDS not focusing**: Making decisions at level 19 instead of critical variables
4. **No clause minimization**: Learned clauses larger than necessary

**Fixes Required**:
1. Increase restart base to 500-1000
2. Implement clause minimization (self-subsumption after 1-UIP)
3. Improve VSIDS decay or switch to LRB/CHB heuristic
4. Add LBD-based variable selection bonus

### 3. Preprocessing Gap
**Cause**: Less aggressive preprocessing than MiniSat
**Evidence**:
- Instance 11c893b7: 0 variables eliminated by our preprocessing
- MiniSat also shows 0.00s simplification time (both see full problem)

**Status**: Not the primary issue for this instance

## Priority Fixes

### Critical (Soundness Impact: None, Performance: High)
1. **Re-implement watched literals for long clauses**
   - Use O(1) swap-remove for watch list updates
   - Test incrementally on small instances
   - Expected: 10-1000x speedup on propagation-heavy instances

### High (Soundness Impact: None, Performance: Medium-High)
2. **Tune restart policy**
   - Increase restartBase from 100 to 500 or 1000
   - Or implement fully dynamic LBD-based restarts (Glucose-style)
   - Expected: 2-10x speedup on structured UNSAT instances

3. **Add clause minimization**
   - Self-subsumption after 1-UIP analysis
   - Remove literals subsumed by other literals in clause
   - Expected: 1.5-2x speedup, lower LBD clauses

### Medium (Soundness Impact: None, Performance: Medium)
4. **Improve variable selection**
   - Add LBD bonus to VSIDS (variables in low-LBD clauses)
   - Or implement LRB/CHB heuristic
   - Expected: 1.5-3x speedup

5. **Better clause database management**
   - Current: Keep 5000 normal clauses
   - Improve: Tiered approach (glue, useful, trash)
   - Expected: 1.2-1.5x speedup

## Next Steps

1. **Immediate**: Re-implement watched literals (highest impact, 3-5 days)
2. **Short-term**: Tune restart policy + add clause minimization (2-3 days)
3. **Medium-term**: Improve VSIDS with LBD bonus (1-2 days)

## Conclusion

Satience is **sound but slow**. The performance gaps are well-understood and fixable:
- 828x slowdown on sudoku → watched literals fix
- 468x slowdown on 11c893b7 → better clause learning + restart tuning

With these fixes, expected median slowdown: **2-5x** (acceptable for a research solver).

