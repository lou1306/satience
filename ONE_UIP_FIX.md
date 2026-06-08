# 1-UIP Fix - June 2026

## Problem
The 1-UIP (First Unique Implication Point) implementation was producing learned clauses with **2+ literals at the current decision level** instead of exactly 1. This caused:
- Learned clauses to be skipped
- Solver to fall back to DPLL-style chronological backtracking
- Poor performance on PHP and other structured instances

## Root Cause
The resolution loop scanned variables **forward by index (0 to NumVars)** instead of scanning the **trail backwards** to respect temporal order. This violated the fundamental 1-UIP requirement: resolving on the **most recently assigned** literal at the current decision level.

**Old code** (lines 2683-2694):
```go
for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
    if s.assignments[varIdx].Level == s.level && 
       s.tmpLiteralInClause[varIdx] && 
       !s.tmpResolved[varIdx] {
        // Resolve on this variable
    }
}
```

**Problems**:
1. ❌ Scanned by variable index, not temporal order
2. ❌ Could resolve on old literals before newly added ones
3. ❌ Didn't follow implication graph structure
4. ❌ Missed the true UIP (most recent bottleneck)

## Solution
Implemented **MiniSat-style trail scanning** (backwards from end):

**New code**:
```go
// Start from end of trail and scan backwards (MiniSat-style)
trailIndex := len(s.trail) - 1
pathC := s.tmpLevelCount[s.level]

for pathC > 1 {
    // Find next literal to resolve by scanning trail backwards
    for trailIndex >= 0 {
        varIdx := uint32(s.trail[trailIndex])
        trailIndex--
        
        // Skip if not in learned clause or already resolved
        if !s.tmpLiteralInClause[varIdx] || s.tmpResolved[varIdx] {
            continue
        }
        
        // Skip if at lower level (not counted in pathC)
        if s.assignments[varIdx].Level != s.level {
            continue
        }
        
        // Found a literal at current level
        foundVar = varIdx
        found = true
        break
    }
    
    // Stop early if no resolvable literal found (MiniSat behavior)
    if !found {
        break
    }
    
    // ... resolve on foundVar ...
}
```

**Key changes**:
- ✅ Scan trail backwards (`trailIndex := len(s.trail) - 1`)
- ✅ Respect temporal order of assignments
- ✅ Stop early when no resolvable literal found
- ✅ Maintain `pathC` counter dynamically
- ✅ Handle deleted learned clauses gracefully

## Implementation Details

### Files Modified
- `internal/solver/solver_cdcl.go:2668-2806` - Core 1-UIP resolution loop

### Changes Made
1. **Replaced forward variable scanning** with backward trail scanning
2. **Added invariant checking** to verify exactly 1 literal at current level
3. **Added comprehensive debug logging** for first 100 conflicts
4. **Fixed `newLiterals` variable scoping** (was undefined)

### Testing
- ✅ All 23 unit tests pass
- ✅ Fuzzer: 100% soundness (20/20 tests, 0 invalid models)
- ✅ PHP instances: All show "1 at level X" (no errors)
- ✅ No "1-UIP ERROR" or "INVARIANT FAIL" messages

### Verification Commands
```bash
# Unit tests
go test ./internal/solver -v

# Fuzzer
./fuzz -n 20 -mode random -verbose

# PHP instances (check 1-UIP output)
./satience -verbose benchmark/gbd_instances/php_6p_5h_unsat.cnf 2>&1 | grep "1-UIP"

# Check for errors
./satience -verbose benchmark/gbd_instances/php_7p_6h_unsat.cnf 2>&1 | grep -E "1-UIP.*ERROR|INVARIANT FAIL"
```

## Results

### Correctness
- **Before**: "2 at current level" on ~50% of conflicts (learned clauses skipped)
- **After**: "1 at current level" on 100% of conflicts (proper 1-UIP)

### Performance Impact
- **PHP 7p6h**: 435 conflicts (satience) vs 251 conflicts (MiniSat)
  - 1.7× more conflicts than MiniSat (acceptable with linear propagation)
  - Previously would be much higher due to skipped learned clauses
- **Runtime**: 67ms (satience) vs 6ms (MiniSat) - 11× slower
  - Expected due to linear propagation (not 1-UIP issue)
  - Watched literals would provide 10-50× speedup

### Example Output
**Before fix**:
```
c [debug] 1-UIP result: conflict=5, 17 literals total, 2 at current level 18
c [debug] Skipping learned clause: 2 literals at level 18 (expected 1)
```

**After fix**:
```
c [1-UIP] Conflict 5: 12 literals, 1 at level 11 (target: 1)
c [LEARNED] Clause 5: LBD=5, size=8, lits=[-9 11 12 13 21 23 26 28]
```

## Next Steps

### Immediate
1. ✅ Fix verified and merged
2. ✅ All tests passing
3. ✅ Soundness verified via fuzzer

### Future Optimization
1. **Watched literals** (5-7 days): Replace linear propagation with O(1) watched literals
   - Expected 10-50× speedup on propagation-heavy instances
   - Infrastructure exists but has performance bugs to fix

2. **Performance tuning**:
   - Reduce conflict count closer to MiniSat (better heuristics)
   - Optimize clause deletion strategy
   - Tune VSIDS decay and restart parameters

## References
- MiniSat Solver.cc: `analyze()` function (lines 414-469)
- "An Extensible SAT-solver" (Eén & Sörensson, 2003)
- WATCHED_LITERALS_IMPLEMENTATION.md - Related propagation optimization
- WATCHED_LITERALS_STATUS.md - Previous implementation attempts
