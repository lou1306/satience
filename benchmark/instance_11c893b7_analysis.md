# Analysis of Instance 11c893b7c37aeb53cdaf5f677dda0b7d.cnf

## Instance Characteristics
- **Variables**: 36
- **Clauses**: 144
- **Type**: Unknown (hash-based name)
- **Expected**: UNSAT (MiniSat result)

## Performance Comparison

| Solver | Result | Time | Conflicts | Decisions |
|--------|--------|------|-----------|-----------|
| MiniSat | UNSAT | 0.064s | 39,968 | 65,872 |
| Satience | TIMEOUT | >30s | >656,000 | >1,000,000 |

**Performance gap**: >468x slower, >16x more conflicts

## Root Cause Analysis

### 1. Restart Policy Too Aggressive
- **Satience**: Restarts every 100 conflicts (Luby sequence base=100)
- **Observed**: 1,445 restarts in 656,000 conflicts
- **MiniSat**: Likely uses dynamic restart policy (fewer restarts)

Effect: We restart before learning high-level structure, losing progress.

### 2. Poor Learned Clause Quality
- **Satience average LBD**: ~6.0
- **Expected good LBD**: 2-3
- **Implication**: Learned clauses involve too many decision levels

Effect: Clauses don't effectively prune search space.

### 3. Stuck at High Decision Level
- **Observed**: Conflicts occur at level 19 consistently
- **Variables**: Only 36 total, so level 19 means ~50% assigned
- **Pattern**: Deep conflicts suggest poor variable selection

Effect: VSIDS heuristic not focusing on critical variables.

### 4. Preprocessing Ineffective
- **Satience**: 0 variables eliminated, 0 clauses removed
- **MiniSat**: Also no simplification (0.00s simplification time)

This is not the issue - both solvers see the full problem.

## Hypothesis

The instance has a **specific structure** that requires:
1. **Longer search between restarts** to learn high-level clauses
2. **Better variable selection** to focus on critical "counting" variables
3. **More effective clause learning** to find shorter, more powerful clauses

Our current configuration learns many low-quality clauses that don't help escape local conflicts.

## Next Steps

### Immediate Investigation
1. **Check 1-UIP implementation** - Verify conflict analysis is correct
2. **Analyze learned clauses** - Check clause sizes and LBD distribution
3. **Compare variable selection** - See which variables MiniSat decides vs us

### Potential Fixes
1. **Increase restart base** - Try base=500 or 1000 instead of 100
2. **Improve VSIDS** - Better decay factor or initial activity
3. **Clause minimization** - Ensure we're minimizing learned clauses
4. **Luby sequence check** - Verify implementation matches standard

### Long-term Improvements
1. **Dynamic restarts** - Use Glucose-style LBD-based restarts
2. **Better heuristics** - LRB or CHB instead of basic VSIDS
3. **Structure detection** - Identify and exploit instance patterns

## Test Commands

```bash
# Run with verbose output
./satience -verbose benchmark/gbd_instances/11c893b7c37aeb53cdaf5f677dda0b7d.cnf

# Compare with MiniSat
time minisat benchmark/gbd_instances/11c893b7c37aeb53cdaf5f677dda0b7d.cnf
```

