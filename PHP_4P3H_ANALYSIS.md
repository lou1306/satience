# PHP 4 Pigeons 3 Holes Analysis

## Instance
- 12 variables, 22 clauses
- 4 pigeon clauses (size 3): each pigeon must go to at least one hole
- 18 hole clauses (size 2): no two pigeons can share a hole
- Expected: UNSAT (pigeonhole principle)

## Results
- **MiniSat**: UNSAT in 0.001s, **0 conflicts** (solved in preprocessing)
- **satience**: TIMEOUT after 146,000+ conflicts

## Root Cause Analysis

### 1-UIP Analysis Failure

The 1-UIP conflict analysis produces clauses with **2+ literals at the current decision level** instead of exactly 1. This violates the 1-UIP invariant and causes learned clauses to be non-unit after backjumping.

**Example from conflict #1:**
```
c [debug] 1-UIP: 2 candidates to resolve on, tmpLevelCount[2]=2
c [debug] 1-UIP: resolved var 5 (level 2), reason has 3 lits, 1 at current level, tmpLevelCount[2]=2
c [debug] 1-UIP: resolved var 8 (level 2), reason has 3 lits, 1 at current level, tmpLevelCount[2]=2
c [debug] 1-UIP result: conflict=1, 4 literals total, 2 at current level 2
```

**Resolution cycle:**
1. Start: 2 literals at level 2
2. Resolve var 5: reason adds 1 literal at level 2 → count stays 2
3. Resolve var 8: reason adds 1 literal at level 2 → count stays 2
4. Exit with 2 literals (should be 1!)

**Learned clause:** `[4 5 7 8]` with LBD=2, but **2 literals at level 2**

After backjumping, this clause has 2 unassigned literals, so it doesn't propagate anything. The same conflict recurs infinitely.

### Why Reason Clauses Have 3 Literals

The reason clauses are the **pigeon constraints** (e.g., `4 ∨ 5 ∨ 6` = pigeon 2 must go to hole 1, 2, or 3). When we resolve on one literal from this clause, the other two literals may be at the same decision level, causing the tmpLevelCount to stay constant or even increase.

**Example:**
```
c [debug] 1-UIP: resolved var 11 (level 3), reason has 3 lits, 2 at current level, tmpLevelCount[3]=3
```

Resolving var 11 added 2 literals at level 3, increasing the count from 2 to 3!

### Comparison with MiniSat

MiniSat solves this in **preprocessing** through techniques we don't implement:
- **Hyper-binary resolution**: Derives binary clauses from ternary + binary combinations
- **Equivalence detection**: Finds equivalent literals and merges them
- **Cardinality reasoning**: Recognizes pigeonhole structure directly

Our preprocessing includes:
- Unit propagation (no unit clauses initially)
- Pure literal elimination (no pure literals)
- Subsumption (no subsumed clauses)
- Hyper-binary resolution (implemented but not effective on PHP)
- Failed literal detection (requires trying many literals)
- Variable elimination (disabled due to soundness bugs)
- Blocked clause elimination (runs but doesn't help)

None of these detect the pigeonhole unsatisfiability.

## Solutions

### Short-term (not implemented)
1. **Enable variable elimination** - Might simplify the formula enough
2. **Improve hyper-binary resolution** - Derive more binary clauses
3. **Add cardinality constraint detection** - Recognize "at least k of n" patterns

### Long-term (research-level)
1. **Extended resolution** - More powerful than 1-UIP
2. **Symmetry breaking** - PHP has symmetries that could be exploited
3. **Gaussian elimination** - For XOR/equality structures

## Status

This is a **known limitation** of basic CDCL with 1-UIP learning. PHP UNSAT instances are provably exponentially hard for this solving approach.

**Recommendation**: Accept the limitation for now. PHP UNSAT is rare in practical applications and is primarily a benchmark instance. Focus on instances where our solver performs well (algebra, XOR, arg chain, tseitin).

## Test Command

```bash
# Create PHP 4p3h instance
cat > /tmp/php_4p_3h_unsat.cnf << 'CNF'
p cnf 12 22
1 2 3 0
4 5 6 0
7 8 9 0
10 11 12 0
-1 -4 0
-1 -7 0
-1 -10 0
-4 -7 0
-4 -10 0
-7 -10 0
-2 -5 0
-2 -8 0
-2 -11 0
-5 -8 0
-5 -11 0
-8 -11 0
-3 -6 0
-3 -9 0
-3 -12 0
-6 -9 0
-6 -12 0
-9 -12 0
CNF

# Solve with MiniSat (0.001s, 0 conflicts)
minisat /tmp/php_4p_3h_unsat.cnf /tmp/out.txt

# Solve with satience (timeout after 146K+ conflicts)
./satience -verbose /tmp/php_4p_3h_unsat.cnf
```
