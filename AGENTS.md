# Satience — Agent Development Notes

## Build Commands

```bash
# Release build (optimized, stripped, GOAMD64=v3)
make satience
# or directly:
GOAMD64=v3 go build -tags release -ldflags="-s -w -buildid=" -trimpath -o satience ./cmd/satience

# Benchmark build (optimized, with debug info)
GOAMD64=v3 go build -o satience_bench ./cmd/satience

# Debug build (no optimizations, with assertions)
make debug
```

## Test Commands

```bash
make test          # Run all unit tests
make test-verbose  # Verbose test output
make test-race     # Run with race detector
make vet           # go vet static analysis
make bench         # Run benchmarks
```

## Fuzz Commands

```bash
# External (requires: cnfgen via pipx, minisat)
make fuzz           # CNFgen soundness suite (known-answer families)
make fuzz-random    # Random k-SAT cross-check vs minisat
make fuzz-structured # Structured random instances (Tseitin, kcolor, kclique)

# Go native (no external deps)
make fuzz-parser    # Parser robustness fuzz (FuzzParser, ~60s)
make fuzz-property  # Transformation invariance fuzz (FuzzTransformInvariance, ~60s)

# All fuzzers
make fuzz-all
```

### Fuzzer env vars
- `ITERATIONS` — number of instances for shell fuzzers (default 100)
- `TIMEOUT` — per-solver timeout in seconds (default 10)
- `JOBS` — parallel workers (default 6)

### Fuzz failure artifacts
Failed instances are saved to `benchmark/fuzz_failures/` for reproducibility.

## Benchmark Commands

```bash
# Build benchmark binary
GOAMD64=v3 go build -o benchmark/satience_b4 ./cmd/satience

# Run the fast benchmark suite
cd benchmark && BINARY=./satience_b4 ./run_satience_fast_suite.sh
```

Baseline B3: PAR2 ~1.58s, 100% solve rate on 72 instances.

## Architecture

- `cmd/satience/` — CLI entry point
- `internal/parser/` — DIMACS CNF parser
- `internal/cnf/` — CNF data structures (Literal, Clause, Watch, CNF)
- `internal/solver/` — CDCL solver core
  - `solver_cdcl.go` — Main solver: propagation, conflict analysis, restarts
  - `verify.go` — Model verification (`VerifySolution`)
  - `ve.go` — Vivification engine
- `benchmark/` — Benchmark scripts and fuzz harnesses

## Key Design Decisions

- `watch.Blit` goes stale when the OTHER watch of the same clause moves — `trueReplacementLit` optimization keeps it accurate
- Original clauses use SoA (Structure of Arrays) via `literalPool` + `originalClauseLocs` for cache efficiency
- `NewCNF` caps pre-allocation at 1M clauses to prevent OOM from malformed headers
- Parser rejects literals exceeding `int32` range to prevent `uint32` truncation aliasing
