# Preprocessing Analysis

## Date: 2026-06-06

## Goal
Improve preprocessing to solve "easy" instances before search, closing the performance gap with MiniSat on instances it solves in <0.1s.

## Key Finding: MiniSat Solves 50v/159c Instance in 0.0009s

**Instance**: `18f54820956791d3028868b56a09c6cd.cnf` (50 vars, 159 clauses, UNSAT)

**MiniSat**:
- Time: 0.0009s
- Conflicts: 0
- Method: "Solved by simplification"
- Preprocessing: variable elimination + garbage collection

**Satience**:
- Time: ⏱️ Timeout (>10s)
- Preprocessing: eliminates 29 variables, reduces to 84 clauses
- Search: required (times out)

**Gap**: MiniSat detects UNSAT during preprocessing; we do not.

## What We Implemented

### 1. Added Subsumption Elimination to Preprocessing Pipeline
- `subsumptionElimination()` was implemented but not called during preprocessing
- Now called before `selfSubsumption()` in each preprocessing pass
- Removes clauses subsumed by shorter clauses

### 2. Improved Preprocessing Order
New order in each pass:
1. **Unit propagation** - catch existing units
2. **Variable elimination** - can create unit clauses
3. **Unit propagation** - propagate newly created units
4. **Pure literal elimination** - assign pure literals
5. **Subsumption elimination** - remove subsumed clauses
6. **Self-subsumption** - strengthen clauses
7. **Hyper-binary resolution** - derive binary clauses
8. **Unit propagation** - propagate from binary clauses
9. **Failed literal elimination** - detect forced assignments

Rationale: Variable elimination can create unit clauses, so we run unit propagation immediately after.

## Results

### Unit Tests
✅ All 9 unit tests pass - no regressions

### Instance: 18f54820956791d3028868b56a09c6cd.cnf (50v, 159c, UNSAT)
- **Before**: Timeout
- **After**: Timeout (no improvement)
- **Preprocessing**: Eliminates 29 variables, reduces to 84 clauses
- **Issue**: Variable elimination doesn't create conflict; search still required

### Instance: 11c893b7c37aeb53cdaf5f677dda0b7d.cnf (36v, 144c, UNSAT)
- **MiniSat**: 0.067s, 40K conflicts
- **Satience**: Timeout, >53K conflicts (incomplete)
- **Issue**: Propagation speed (177x slower per conflict), not preprocessing

## Analysis: Why MiniSat Solves 50v Instance Instantly

MiniSat's preprocessing output:
```
subsumption left:        158 (0 subsumed, 0 deleted literals)
elimination left:         49
Garbage collection: 4176 bytes → 2020 bytes (8 iterations)
Solved by simplification
conflicts: 0, propagations: 1
```

**Key observations**:
1. Subsumption finds 0 clauses to remove (same as us)
2. "elimination left: 49" - likely 49 clauses after elimination
3. Multiple garbage collections suggest clause database cleanup
4. "propagations: 1" - finds UNSAT with single propagation

**What MiniSat does differently**:
- MiniSat's variable elimination likely uses **substitution** rather than resolution
- When eliminating variable `x`, it substitutes `x = expression` throughout formula
- This can create unit clauses that propagate to UNSAT
- Our variable elimination uses resolution (creates resolvents), which preserves satisfiability but doesn't simplify as aggressively

**Alternative hypothesis**:
- MiniSat might detect **equivalence chains** (a↔b↔c↔d) and substitute representatives
- The 50v instance has many equivalence-like patterns: `-7 4 0, -7 1 0` means `7→4` and `7→1`
- If there are reverse implications, this creates equivalences
- Substituting one representative for all equivalent variables could collapse the formula

## Next Steps

### Option 1: Improve Variable Elimination (2-3 days)
- Add equivalence detection during variable elimination
- When eliminating `x`, check if resolvents are equivalences (a↔b)
- Substitute representatives instead of creating resolvents
- More aggressive simplification

### Option 2: Add Equality Reasoning (3-5 days)
- Detect equivalence classes from binary clauses
- Use union-find to maintain equivalence relations
- Substitute representatives throughout formula
- Could solve equivalence-rich instances instantly

### Option 3: Accept Limitation ( documentation only)
- Document that Satience doesn't match MiniSat on "preprocessing-only" instances
- Focus on search performance (watched literals) for general cases
- Compare with historical solvers (pre-2000) on search benchmarks

## Recommendation

**Pursue Option 2 (Equality Reasoning)**:
- The 50v instance is clearly equivalence-rich
- MiniSat solves it via preprocessing, not search
- Adding equivalence detection would close the gap on this class of instances
- Implementation: enhance `equivalenceDetection()` and re-enable it in preprocessing pipeline
- Test on equivalence-rich instances from GBD

**Also pursue watched literals** (separate track):
- Even with perfect preprocessing, search will be needed for most instances
- Watched literals would provide 100-1000x speedup on search-heavy instances
- This is the primary bottleneck for general performance

## Current Preprocessing Pipeline

```
for pass in 1..5:
  1. unitPropagationPreprocess()
  2. variableElimination()
  3. unitPropagationPreprocess()  # After VE
  4. pureLiteralElimination()
  5. subsumptionElimination()     # NEW
  6. selfSubsumption()
  7. hyperBinaryResolution()
  8. unitPropagationPreprocess()  # After hyper-binary
  9. failedLiteralElimination()
  
blockedClauseElimination()  # After all passes
```

This is comprehensive and follows modern solver practices. The missing piece is **equivalence reasoning**.
