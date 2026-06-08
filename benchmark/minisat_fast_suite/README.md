# MiniSat Fast Benchmark Suite

**Purpose**: Benchmark instances that MiniSat solves in ≤10 seconds, used to measure Satience's performance relative to MiniSat.

**Total Instances**: 32

## Composition

- **20 hash-named instances**: Various families from GBD (solved by MiniSat in <10s)
- **6 PHP instances**: Pigeonhole Principle problems (SAT and UNSAT)
- **5 Tseitin grid instances**: Grid-based Tseitin formulas (SAT and UNSAT)
- **1 Arg chain instance**: Argument chain reasoning problem

## Usage

```bash
# Run Satience on all instances
while read instance; do
    ./satience "benchmark/gbd_instances/$instance"
done < benchmark/minisat_fast_suite/instances.txt

# Compare with MiniSat
while read instance; do
    echo "=== $instance ==="
    time ./satience "benchmark/gbd_instances/$instance"
    time minisat "benchmark/gbd_instances/$instance" /tmp/out.txt
done < benchmark/minisat_fast_suite/instances.txt
```

## Selection Criteria

Instances were selected if:
1. MiniSat solves them in ≤10 CPU seconds
2. Instance is from the GBD benchmark database
3. Mix of SAT and UNSAT instances
4. Diverse problem families represented

## Performance Targets

| Instance Type | Target Slowdown |
|--------------|-----------------|
| PHP | ≤2× |
| Tseitin (small) | ≤5× |
| Arg chain | ≤3× |
| Hash instances | ≤10× |

## Notes

- All instances should return the same result (SAT/UNSAT) for both solvers
- Models from SAT instances should be verified to satisfy all clauses
- Timeout: 60 seconds per instance for Satience

## Files

- `instances.txt`: List of instance filenames (one per line)
- `README.md`: This file
