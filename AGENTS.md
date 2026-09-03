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

## Held-Out Gate (distributional generalization)

```bash
# Distributional held-out gate: paired A/B over parametric CNFgen families
# with fresh random draws. Accepts VARIANT_BINARY vs CONTROL_BINARY only if it
# generalizes across ALL families (no new TMO, bounded per-instance regression,
# no median regression) on a seed-partitioned VALIDATION rail.
# Requires: cnfgen
make heldout

# A/B example:
#   go build -o /tmp/ctl ./cmd/satience   # reference
#   go build -o /tmp/var ./cmd/satience   # variant under test
#   CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var make heldout
```

WHY: a static fixed corpus (the 72-instance suite, gbd_instances) gets
re-overfit — a detector can be tuned to the fixed deterministic trajectories
and their dec/conf/LBD signatures. Random draws within a parametric family
perturb the exact signature a gate keys to, so a threshold survives only if it
generalizes across the distribution. See `benchmark/heldout.sh` for the full
method and acceptance rule.

CRITICAL partition discipline: parameters may ONLY be tuned against the DEV
rail. Re-tuning after a VALIDATION failure and re-running burns the validation
distribution. Coverage is cnfgen combinational/structured families only
(k-SAT, Tseitin, kcolor, kclique); industrial encodings are NOT covered.

### BROAD held-out gate (`benchmark/heldout_broad.sh`) — for changes that need
a harder, larger distribution than `heldout.sh` can see

The fast-suite + `heldout.sh` rails are dominated by tiny (50-300 var) instances
the solver already solves in ~2s, so most improvements are unmeasurable there
(cold paths like FLP/in-processing/subsumption never fire; large-scale behavior
never manifests). `heldout_broad.sh` measures a harder, broader rail so a change
has real headroom:

- **Tier 1 (cnfgen, reproducible):** large random k-SAT (1000-2000 vars, near
  the phase transition — FLP budget binds, big BVE/matrices) + large structured
  (Tseitin grids/gnd, pigeonhole, k-color, binary k-clique).
- **Tier 2 (industrial, optional):** fixed SAT-competition/verification
  instances via `INDUSTRIAL_DIR` + `INDUSTRIAL_LIST`; verdicts cross-checked
  against minisat.

**Aggregate / expectation acceptance (differs from `heldout.sh`'s per-family
all-pass veto, which rejects any redistributive-but-better change).** A variant
is accepted on the VALIDATION rail iff: (1) every solved instance's verdict
matches control (and minisat on the industrial tier) — mismatches are a hard
FAIL; (2) geometric-mean PAR2 improves by >= `ACCEPT_IMPROVE_PCT` (default 5%);
(3) new-TMO fraction (control solved / variant TMO) <= `NEWTMO_MAX_FRAC` (0.10);
(4) regression fraction (variant >`REGRESS_MAX_PCT` slower above the noise
floor) <= `REGRESS_MAX_FRAC` (0.15). Redistribution across families is allowed:
a change that helps many and hurts a bounded few is landable. This is what
unblocks genuinely better-but-redistributive heuristics (the entire point of
adding this rail). Small-N runs are directional only — run at default
`VALIDATION_ITERATIONS` for the decision; identical-control noise is ~1-2% at
moderate N, so 5% is a safe threshold. See `benchmark/heldout_broad.sh`.

```bash
CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var ./benchmark/heldout_broad.sh
# env: DEV_ITERATIONS VALIDATION_ITERATIONS TIMEOUT JOBS ACCEPT_IMPROVE_PCT
#      NEWTMO_MAX_FRAC REGRESS_MAX_FRAC REGRESS_MAX_PCT NOISE_FLOOR
#      INDUSTRIAL_DIR INDUSTRIAL_LIST MINISAT_BIN
# A/B example:
#   go build -o /tmp/ctl ./cmd/satience
#   go build -o /tmp/var ./cmd/satience
#   CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var make heldout-broad
```

### Held-out env vars
- `CONTROL_BINARY` / `VARIANT_BINARY` — the two binaries to A/B (paired on identical draws)
- `DEV_ITERATIONS` — draws/family on the tuning rail (default 10)
- `VALIDATION_ITERATIONS` — draws/family on the accept/reject rail (default 50)
- `TIMEOUT` — per-solver timeout seconds (default 30); PAR2 TMO = 2×TIMEOUT
- `JOBS` — parallel workers (default 6)
- `REGRESS_MAX_FRAC` — max fraction of draws allowed to regress by >`REGRESS_MAX_PCT` (default 0.10 / 25)
- `MEDIAN_REGRESS_PCT` — max allowed median PAR2 regression % (default 5)
- `NOISE_FLOOR` — control solves below this seconds are exempt from % rules (default 0.25)

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
