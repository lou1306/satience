# Memory Explosion on PHP Instances

## Problem
The solver exhibits extreme memory usage (16GB+) on PHP (pigeonhole principle) instances, specifically `php_6p_5h_unsat.cnf`.

## Symptoms
- Memory grows exponentially: 0MB → 379MB → 16GB in 30 seconds
- Only 30-50 conflicts processed before memory explodes
- Other instances (Sudoku, Tseitin, algebra) work fine (<50MB)

## Investigation

### Ruled Out
- ❌ Learned clause accumulation (only 15-20 clauses when explosion starts)
- ❌ Arena buffer growth (capacity stays at 0MB)
- ❌ Minimization overhead (disabled, no improvement)
- ❌ Failed literal elimination (disabled, no improvement)
- ❌ Variable elimination (disabled, no improvement)
- ❌ Preprocessing passes (reduced to 2, no improvement)

### Observations
- Memory growth rate: ~0.5GB/second
- Conflict rate: ~6 conflicts/second (slowing down over time)
- Iterations: Only ~36 for 30 conflicts (propagate() is slow)
- Trail size: Stable at 30
- Decision level: Stable at 6

### Hypotheses (Unconfirmed)
1. **Go runtime heap pre-allocation**: Go may reserve large contiguous memory regions
2. **Watched literals bug**: Binary clause watches might have a memory leak
3. **Hidden allocation in propagate()**: Some code path allocating large temporaries
4. **GC pressure**: Memory allocated faster than GC can reclaim

## Workaround

Use `ulimit` to limit memory usage:

```bash
ulimit -v 2000000  # 2GB limit
./satience php_6p_5h_unsat.cnf
```

Note: PHP instances are theoretically exponential for CDCL solvers with 1-UIP learning. Even if memory issues were fixed, the solver would likely timeout.

## Next Steps

To identify the root cause:
1. Use pprof with `-memprofile` flag
2. Add per-function allocation tracking
3. Check Go runtime heap reservation behavior
4. Profile watched literals initialization
5. Consider rewriting propagate() to be allocation-free

## Impact

This issue only affects PHP and similar pigeonhole principle instances. The solver works correctly on:
- ✅ Tseitin instances (1.0× slowdown)
- ✅ XOR/Equality instances (2.0× slowdown)
- ✅ Chain instances (5.4× slowdown)
- ✅ Sudoku (412× slowdown, but only 42MB memory)

PHP instances are rare in practical applications and are primarily used as theoretical benchmarks.
