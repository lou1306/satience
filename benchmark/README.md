# Benchmark Infrastructure

## Quick Start

```bash
# Quick benchmark (recommended)
python3 benchmark/quick_benchmark.py

# Compare with MiniSat
python3 benchmark/compare_minisat.py

# Full comparison
python3 benchmark/compare_minisat_full.py

# Regression testing
python3 benchmark/benchmark_regression.py
```

## Scripts

### Active Scripts

1. **quick_benchmark.py** - Fast benchmark on small instances
2. **compare_minisat.py** - Head-to-head comparison with MiniSat
3. **compare_minisat_full.py** - Comprehensive MiniSat comparison
4. **benchmark_regression.py** - CI/CD regression testing
5. **auto_restart_benchmark.py** - Restart policy testing

### Download Scripts

- **download_instances.py** - Fetch instances from GBD
- **download_diverse.py** - Download diverse instance families
- **download_new_families.py** - Download new families from GBD

### Archived Scripts

Older/experimental scripts are in `benchmark/archive/`:
- Historical benchmarks
- One-off experiments
- Superseded versions

## Usage

All scripts accept standard arguments:
- `--timeout`: Timeout per instance (default: 30s)
- `--instances`: Number of instances to test
- `--verbose`: Show detailed output

Example:
```bash
python3 benchmark/quick_benchmark.py --timeout 60 --verbose
```

## Results

Results are saved as JSON files:
- `restart_benchmark_*.json` - Restart policy results
- `regression_results_*.json` - Regression test results
- `minisat_comparison_*.json` - MiniSat comparison results
