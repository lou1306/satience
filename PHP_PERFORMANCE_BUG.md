# Critical Performance Bug: CDCL Slower Than DPLL on PHP Instances

## Summary

**CDCL with clause learning is 7500× SLOWER than plain DPLL** on PHP 5 pigeons 4 holes:

- **Plain DPLL**: 0.004s ✅
- **CDCL with 1-UIP**: TIMEOUT (30s+) ❌

This is backwards - CDCL should be **faster** than DPLL due to clause learning, not slower.

## Root Cause Analysis

### 1. VSIDS Variable Selection

For PHP instances, VSIDS chooses variables in a suboptimal order. DPLL with sequential ordering (pigeon 1, then pigeon 2, etc.) finds conflicts quickly. VSIDS's activity-based heuristic makes poor choices for this instance structure.

### 2. 1-UIP Produces Weak Clauses

When conflicts involve literals from multiple decision levels (including decisions), our 1-UIP implementation cannot resolve to a single literal at the current level. It exits early, producing clauses with:

- **5-9 literals** (too large)
- **LBD=3-4** (weak constraints)
- **Multiple literals at current level** (don't propagate after backjump)

Example learned clauses:
```
Clause 0: [-3 5 6 9 10]        LBD=3, size=5
Clause 1: [9 10 -11 12 13 14 17 18]  LBD=3, size=8
Clause 3: [5 6 -8 9 10 -11 12 13 17] LBD=4, size=9
```

These clauses:
- **Slow down propagation** (more literals to check per clause)
- **Don't constrain search** (high LBD means they span many decision levels)
- **Cause exponential search** (same conflicts recur with different variable assignments)

### 3. Clause Learning Hurts More Than Helps

The learned clauses are so weak that they're net negative:
- Overhead of checking them during propagation > benefit from constraint
- Search becomes DPLL with bad variable ordering + overhead

## Current Mitigation

Added clause size filter in `learnClause()`:
```go
if len(learnedLits) > 4 {
    // Skip this clause - too large, likely from failed 1-UIP
    return backjumpLevel
}
```

This prevents learning the worst clauses, but **doesn't fix the exponential search**. VSIDS still makes poor choices, and we're essentially running DPLL with overhead.

## Solutions

### Short-term (not implemented)

1. **Detect PHP-like instances** and switch to sequential variable ordering
2. **Disable clause learning** when it's consistently producing weak clauses
3. **Improve 1-UIP** to handle multi-decision conflicts better

### Long-term (recommended)

1. **Implement watched literals** - reduces propagation overhead by 10-50×
2. **Add cardinality constraint detection** - recognizes PHP structure and reasons about it directly
3. **Improve VSIDS** with phase saving and better decay for structured instances
4. **Implement clause minimization** after 1-UIP to remove redundant literals

## Test Case

```bash
# Plain DPLL solves instantly
go test ./internal/solver -run TestDPLLPhp5p4hUnsat -v
# PASS (0.004s)

# CDCL times out
go test ./internal/solver -run TestCDCLPhp5p4hUnsat -v
# TIMEOUT (30s+)
```

## Impact

- **Small PHP instances (≤4 pigeons)**: Solve correctly with 1-UIP fix
- **Larger PHP instances (≥5 pigeons)**: Time out due to this bug
- **Other structured instances**: May be affected if they have similar characteristics

This is a **critical correctness-adjacent bug** - the solver is complete (will eventually find UNSAT), but the performance is so poor it's effectively broken for these instances.

## References

- PHP instances are known to be hard for basic CDCL
- MiniSat solves PHP 5p4h in 32 conflicts (0.0002s) using watched literals + better heuristics
- State-of-the-art solvers use cardinality reasoning for PHP-like structures
