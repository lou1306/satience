# Satience Benchmark Suite

## Quick Start

```bash
# Build solver
go build -o satience ./cmd/satience

# Run quick benchmark (12 instances)
cd benchmark
python3 quick_bench.py

# View results
cat quick_bench_results.txt
```

## Benchmark Categories

### Small Instances (< 100 vars)
- algebra_xor: XOR/equality structures
- php: Pigeonhole principle (known hard for CDCL)
- arg_chain: Argumentation chain structures
- tseitin_grid: Tseitin graph formulas

### Medium Instances (100-500 vars)
- Dense random formulas
- BMC (Bounded Model Checking)
- Circuit equivalence

### Large Instances (> 500 vars)
- Cardinality constraints
- Sudoku
- Planning problems

## Performance Summary

**Median slowdown vs MiniSat: 5.39x**

| Category | Performance | Notes |
|----------|-------------|-------|
| Cardinality constraints | **18x FASTER** | Our preprocessing excels |
| Algebra/XOR | 1.5-2x slower | Competitive |
| Arg chain | 1.7-2x slower | Good |
| Tseitin grid | 10-35x slower | Needs watched literals |
| Dense random | TIMEOUT | Propagation bottleneck |
| Sudoku | 1200x slower | Propagation bottleneck |
| PHP UNSAT | TIMEOUT | Theoretically hard |

## Key Findings

1. **Cardinality constraints**: Satience outperforms MiniSat on large cardinality instances
2. **Propagation bottleneck**: Linear clause scanning causes 10-1000x slowdown on dense/propagation-heavy instances
3. **Soundness**: 100% - all results verified against MiniSat
4. **Preprocessing**: Highly effective on structured instances

## Files

- `quick_bench.py`: Quick benchmark script (12 instances)
- `comprehensive_bench.py`: Full benchmark (all instances)
- `download_diverse.py`: Download diverse instances from GBD
- `gbd_instances/`: Benchmark instances
- `performance_analysis_2026_june.md`: Detailed analysis

## Downloading More Instances

```bash
# Download diverse instances from GBD
python3 download_diverse.py

# This will download ~60 instances from 12 different families
```

## Known Limitations

- **Watched literals**: Not implemented (linear scanning instead)
- **PHP UNSAT**: Exponentially hard for basic CDCL
- **Dense instances**: Propagation bottleneck
