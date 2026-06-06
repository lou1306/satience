# Comprehensive Benchmark Results

**Date**: 2026-06-06 23:12:57
**Instances**: 2 (from GBD database)
**Timeout**: 60 seconds

## Summary

- **Total instances**: 2
- **Satience correct**: 0/2 (0.0%)
- **Matches MiniSat**: 1/2 (50.0%)
- **Satience faster**: 1/2
- **MiniSat faster**: 0/2
- **Median speedup**: 0.00x (MiniSat/Satience)


## Results by Family

### random (1 instances)

| Instance | Vars | Clauses | Expected | Satience | MiniSat | Time (Sat/Min) | Speedup |
|----------|------|---------|----------|----------|---------|----------------|---------|
| 961811bc85fb3ad1... | 120 | 1872 | sat | TIMEOUT | SAT | 60.0s (TO)/34.881s | 0.58x |

### uniform-random (1 instances)

| Instance | Vars | Clauses | Expected | Satience | MiniSat | Time (Sat/Min) | Speedup |
|----------|------|---------|----------|----------|---------|----------------|---------|
| e0daae7b94d54c77... | 70 | 6230 | unsat | TIMEOUT | TIMEOUT | 60.0s (TO)/N/A | 1.00x |

## Detailed Results

| Hash | Family | Vars | Clauses | Satience | MiniSat | Speedup | Match |
|------|--------|------|---------|----------|---------|---------|-------|
| e0daae7b94d54c77... | uniform-random | 70 | 6230 | 60.000s | 60.000s | 1.00x | ✓ |
| 961811bc85fb3ad1... | random | 120 | 1872 | 60.000s | 34.881s | 0.58x | ✗ |
