# Closing the Performance Gap: Alternative Strategies

## Current Situation

**Problem**: Binary/ternary optimization and watched literals both have soundness bugs that are subtle and hard to debug.

**Constraint**: Need to close the 6.76x Sudoku gap and 1.2x general slowdown WITHOUT introducing bugs.

## Alternative Optimization Strategies

### 1. **Clause Ordering Heuristics** (LOW RISK, 1.2-1.5x improvement)

**Idea**: Order clauses by likelihood of propagation to improve cache locality and early termination.

**Implementation**:
```go
// Sort clauses after preprocessing:
// 1. Binary clauses first (most likely to propagate)
// 2. Ternary clauses second
// 3. Short clauses (< 10 literals)
// 4. Long clauses last

// In propagate(), check in this order
for _, clause := range s.orderedClauses {
    // Early exit when unit found
    // Better cache locality for similar-sized clauses
}
```

**Why it helps**:
- Better L1/L2 cache utilization
- More propagations found early in loop
- No correctness risk (just reordering)

**Expected**: 1.1-1.3x on Sudoku, 1.05-1.15x overall

---

### 2. **Literal Count Caching** (LOW RISK, 1.3-1.8x improvement)

**Idea**: Cache the number of false literals per clause to avoid full scans.

**Data structure**:
```go
type ClauseState struct {
    falseCount uint16  // Number of false literals
    firstUnassigned uint32  // Index of first unassigned literal
}
clauseState []ClauseState
```

**Update on assignment**:
```go
func (s *CDCLSolver) assignLiteral(lit, level, reason) {
    // Update falseCount for all clauses containing this literal
    // If falseCount increases, check if clause becomes unit/conflict
}
```

**Why it helps**:
- Unit detection becomes O(1) instead of O(clause_length)
- Only update affected clauses (not all clauses)
- Standard technique in solvers like CaDiCaL

**Risk**: Moderate - need to maintain counts correctly on backtrack

**Expected**: 1.5-2x on propagation-heavy instances

---

### 3. **Incremental Pure Literal Detection** (LOW RISK, 1.1-1.3x improvement)

**Idea**: Track literal polarities incrementally instead of full scans.

**Implementation**:
```go
literalPolarity []int  // +1 for positive, -1 for negative, 0 for both

// Update when assigning:
// When x=true, decrement polarity of ¬x in all clauses
// When polarity reaches threshold, literal is pure

// Check periodically (every 100 decisions)
```

**Why it helps**:
- Pure literals can be assigned without backtracking
- Reduces search space
- Cheap to maintain incrementally

**Expected**: 1.1-1.2x on structured instances

---

### 4. **Adaptive Clause Selection** (MEDIUM RISK, 1.2-1.5x improvement)

**Idea**: Don't check all clauses every propagate() - use activity-based selection.

**Implementation**:
```go
clauseActivity []float64  // How often does this clause propagate?

// In propagate(), only check top N% most active clauses
// Every K propagations, check all clauses (to catch new units)

threshold := 0.7  // Check 70% of clauses normally
for i, clause := range s.clauses {
    if i % 10 < 7 || clauseActivity[i] > threshold {
        // Check this clause
    }
}
```

**Why it helps**:
- Most propagations come from "active" clauses
- Skip checking "dead" clauses most of the time
- Similar to VSIDS but for clauses

**Risk**: Medium - might miss propagations, need to verify completeness

**Expected**: 1.3-1.8x on large instances

---

### 5. **Bit-Parallel Propagation for Binary Clauses** (MEDIUM RISK, 2-5x improvement)

**Idea**: Use bit vectors to represent binary clause relationships and propagate with bitwise operations.

**Implementation**:
```go
// For each variable x, store:
// - posImplications[x]: bitvector of variables y where (x → y) exists
// - negImplications[x]: bitvector of variables y where (¬x → y) exists

// When x=true:
// propagate := posImplications[x]
// for each bit set in propagate:
//     assign corresponding variable

// Bitwise operations are O(n/64) instead of O(n)
```

**Why it helps**:
- Binary clauses are 50-80% of clauses in many instances
- Bitwise operations are extremely fast
- No complex watched literal invariants

**Risk**: Medium - need to handle bitvector updates on backtrack

**Expected**: 2-3x on binary-heavy instances (Sudoku, Tseitin)

---

### 6. **Two-Phase Propagation** (LOW RISK, 1.2-1.5x improvement)

**Idea**: Separate "check if unit" from "propagate" to reduce redundant work.

**Implementation**:
```go
func (s *CDCLSolver) propagate() (bool, int) {
    // Phase 1: Quick scan to find all units
    units := make([]uint32, 0)
    for _, clause := range s.clauses {
        if clause.falseCount == len(clause)-1 {
            units = append(units, clause.unassignedLit)
        }
    }
    
    // Phase 2: Propagate all units
    for _, unit := range units {
        s.assignLiteral(unit, ...)
    }
    
    // If any units found, restart propagation
    return len(units) > 0, ...
}
```

**Why it helps**:
- Avoids restarting propagation after EVERY unit
- Batch processing is more cache-friendly
- Simpler control flow

**Expected**: 1.2-1.4x on instances with many simultaneous units

---

### 7. **Variable Activity-Based Ordering** (LOW RISK, 1.1-1.2x improvement)

**Idea**: Order variables in clauses by VSIDS activity to find conflicts faster.

**Implementation**:
```go
// After preprocessing, reorder literals in each clause:
// Put highest-activity literals first
// Most likely to be assigned = find conflicts/units faster

for _, clause := range s.clauses {
    sort.Slice(clause.Literals, func(i, j int) bool {
        return s.vsids[clause.Literals[i].Var()] > 
               s.vsids[clause.Literals[j].Var()]
    })
}
```

**Why it helps**:
- High-activity variables are assigned earlier in search
- Checking them first finds conflicts/units sooner
- Early termination in clause scanning

**Expected**: 1.05-1.15x (small but free improvement)

---

### 8. **Lazy Clause Reconstruction** (MEDIUM RISK, 1.3-1.8x improvement)

**Idea**: Don't store full clauses - store literals separately and reference by indices.

**Implementation**:
```go
// Instead of []Clause { []Literal {...}, ... }
// Use:
allLiterals []uint32  // All literals in one array
clauseStart []uint32  // Start index of each clause

// Clause i: allLiterals[clauseStart[i]:clauseStart[i+1]]
```

**Why it helps**:
- Better memory locality (contiguous storage)
- No slice overhead per clause
- Easier to SIMD-optimize later

**Risk**: Medium - need to update indices when clauses change

**Expected**: 1.2-1.5x from better cache utilization

---

## Recommended Strategy: Progressive Optimization

### Phase 1: Safe Optimizations (1-2 days)
**Goal**: 1.3-1.5x improvement with minimal risk

1. **Clause ordering** (2 hours) - reorder by size
2. **Variable activity ordering** (1 hour) - sort literals by VSIDS
3. **Two-phase propagation** (4 hours) - batch unit processing
4. **Literal count caching** (8 hours) - cache falseCount per clause

**Total expected**: 1.4-1.8x on Sudoku, 1.15-1.25x overall

### Phase 2: Moderate Optimizations (2-3 days)
**Goal**: Additional 1.5-2x improvement

5. **Bit-parallel binary propagation** (1 day) - bitvectors for binary clauses
6. **Lazy clause reconstruction** (1 day) - contiguous literal storage
7. **Adaptive clause selection** (4 hours) - skip inactive clauses

**Total expected**: 2-3x on Sudoku, 1.3-1.5x overall

### Phase 3: Aggressive Optimizations (3-5 days)
**Goal**: Match MiniSat performance

8. **Re-implement watched literals** - but this time:
   - Start with binary clauses ONLY
   - Extensive property-based testing
   - Compare with reference implementation (MiniSat)
   - Use fuzzer to find edge cases

**Total expected**: 5-10x on Sudoku, 1.0-1.1x overall (competitive with MiniSat)

---

## Immediate Next Step

**Start with Phase 1, optimization #4: Literal Count Caching**

**Why**:
- Highest impact (1.5-2x) with low risk
- Well-understood technique (used in CaDiCaL)
- Doesn't require complex data structure changes
- Can be tested incrementally
- If buggy, easy to revert

**Implementation plan**:
1. Add `falseCount []uint16` and `firstUnassigned []uint32` to CDCLSolver
2. Initialize during preprocessing
3. Update in assignLiteral() when variables are assigned
4. Restore in backtrack()
5. Use in propagate() for O(1) unit detection
6. Test on all 15 unit tests + fuzzer

**Time estimate**: 8-12 hours including testing

---

## Why This Approach is Better Than Watched Literals

| Aspect | Watched Literals | Literal Count Caching |
|--------|-----------------|----------------------|
| **Complexity** | High (complex invariants) | Medium (simple counters) |
| **Bug risk** | High (stale indices) | Low (counters hard to get wrong) |
| **Debugging** | Hard (state is implicit) | Easy (counters are explicit) |
| **Backtracking** | Complex (update watches) | Simple (restore counts) |
| **Preprocessing** | Breaks watches (need rebuild) | Works naturally |
| **Performance** | 10-100x | 1.5-2x |
| **Time to implement** | 3-5 days + debugging | 1 day |

**Strategy**: Get the easy 1.5-2x first, then decide if watched literals is worth the complexity.

---

## Conclusion

The performance gap can be closed **incrementally** with lower-risk optimizations:

- **Short term (1 week)**: 1.5-2x with literal count caching + clause ordering
- **Medium term (2 weeks)**: 3-5x with bit-parallel propagation
- **Long term (1 month)**: 10x+ with carefully-tested watched literals

**Key insight**: Don't aim for the perfect optimization (watched literals) when good optimizations (literal caching) are available NOW with much lower risk.
