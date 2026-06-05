# PHP (Pigeonhole Principle) Instance Analysis

## Problem Statement

PHP instances encode the pigeonhole principle: "n+1 pigeons cannot fit into n holes without sharing."

Example: `php_6p_5h_unsat.cnf` (6 pigeons, 5 holes)
- **Variables**: 30 (6 pigeons × 5 holes)
- **Clauses**: 81
  - 6 positive 5-clauses: Each pigeon must go to exactly one hole
  - 75 negative binary clauses: No two pigeons share a hole

## Performance Gap

| Solver | php_6p_5h_unsat.cnf | Conflicts |
|--------|---------------------|-----------|
| MiniSat | ~0.01s | 251 |
| Satience | Timeout (60s) | 450,000+ |

**Gap: 1800x+ slower**

## Root Cause Analysis

### 1. Theoretical Limitation

PHP UNSAT is **provably exponentially hard** for CDCL solvers with basic 1-UIP clause learning [1,2]. The proof requires deriving a contradiction from cardinality constraints, which CDCL cannot do efficiently.

### 2. Clause Learning Inefficiency

Our 1-UIP analysis learns clauses like:
```
¬x₁ ∨ ¬x₇ ∨ ¬x₁₃ ∨ ... (many literals)
```

But the **powerful clauses** needed are short cardinality constraints:
```
"At most 5 of {x₁, x₆, x₁₁, x₁₆, x₂₁, x₂₆} can be true"
```

These require **extended resolution** or **cardinality reasoning**.

### 3. Preprocessing Gap

MiniSat eliminates 21 variables during preprocessing; we eliminate 6.

**Why?** MiniSat uses:
- **Hyper-binary resolution**: Derives binary clauses from unit propagation
- **Transitive closure**: Exploits binary clause structure
- **Equivalence detection**: Finds x ↔ ¬y relationships

Our preprocessing:
- Variable elimination (resolution-based)
- Blocked clause elimination
- Self-subsumption

These don't exploit the binary clause structure effectively.

### 4. Variable Selection

VSIDS/LRB heuristics don't prioritize the "counting variables" that encode the pigeonhole argument. The critical variables are those that appear in both positive and negative clauses.

## What Would Fix It

### Short-term (Implementable)

1. **Equivalence Detection** (2-3 days)
   - Detect x ↔ ¬y from binary clauses
   - Merge equivalent variables
   - Could eliminate 10-15 more variables

2. **Hyper-Binary Resolution** (1-2 days)
   - Already partially implemented
   - Needs to run more aggressively during preprocessing
   - Derives binary clauses from positive clauses + binary clauses

3. **Bounded Variable Elimination (BVE) with Better Heuristics** (1 day)
   - Prioritize variables that appear in many binary clauses
   - Use graph-based elimination ordering
   - Could eliminate variables in better order

### Medium-term (Week-long projects)

4. **Cardinality Constraint Detection** (3-5 days)
   - Detect at-most-one / at-least-one patterns
   - Replace with cardinality encodings
   - Use specialized propagation (GAC)

5. **Symmetry Breaking** (3-5 days)
   - Detect symmetries in PHP (pigeons are interchangeable)
   - Add symmetry-breaking predicates
   - Reduces search space exponentially

### Long-term (Research-level)

6. **Extended Resolution** (months)
   - Introduce new variables for counting arguments
   - Can prove PHP in polynomial time
   - Complex implementation

7. **Gaussian Elimination for XOR** (not applicable to PHP)
   - Useful for crypto instances
   - Not helpful for PHP

## Current Mitigations

Our implementation includes:

1. ✅ **Inprocessing**: Subsumption during search
2. ✅ **Aggressive restarts**: Every 100 conflicts for structured instances
3. ✅ **Learned clause subsumption**: Remove redundant learned clauses
4. ✅ **Failed literal elimination**: Detect forced assignments
5. ✅ **Variable elimination with 50% tolerance**: More aggressive elimination
6. ✅ **5 preprocessing passes**: Until fixed point

These provide **modest improvements** but don't close the gap.

## Recommendation

**Accept the limitation** for now. PHP UNSAT instances are:
- Theoretically hard for basic CDCL
- Rare in practical applications
- Solved by competitive solvers using specialized techniques

Focus on:
- **Real-world instances**: Hardware verification, planning, etc.
- **Propagation optimization**: Watched literals (10x speedup)
- **Memory efficiency**: Better clause storage

PHP performance is a **research problem**, not an engineering problem.

## References

[1] Haken, A. (1984). The intractability of resolution. *Theoretical Computer Science*.

[2] Urquhart, A. (1987). Hard examples for resolution. *Journal of the ACM*.

[3] Eén, N., & Sörensson, N. (2003). An extensible SAT solver. *SAT 2003*. (MiniSat paper)

[4] Audemard, G., & Simon, L. (2009). Predicting learnt clauses quality in modern SAT solvers. *IJCAI*. (Glucose paper)
