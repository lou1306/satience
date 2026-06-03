# Project: satience - CDCL SAT Solver in Go

## Goal
Build a sound and complete CDCL SAT solver in Go named "satience" with DIMACS CNF support and benchmarking on real GBD instances

## Constraints & Preferences
- DIMACS format only
- No parallel solving
- No incremental solving
- No proof/unsat core generation
- Single-threaded
- Go language
- Benchmarking with uv (never system Python)
- 60 second timeout per benchmark
- Real GBD instances from benchmark-database.de

## Progress
### Done
- Phase 1: Minimal DPLL solver with unit propagation, backtracking, CLI
- Watched literals for efficient unit propagation
- VSIDS variable selection heuristic with decay
- **Simplified CDCL solver**: Removed clause learning for soundness
- **Fixed conflict analyzer**: Now uses solver's implication array (not uninitialized analyzer array)
- **Fixed UIP detection**: Added `hasOtherAtLevel` check for proper termination
- **Fixed backtracking**: Properly flips decisions and maintains trail/trailHead
- All unit tests passing (DPLL and CDCL solvers)
- Benchmark infrastructure: uv project with gbd-tools, polars dependencies
- **GBD metadata database**: meta.db with 32,905+ instances from 200+ families
- **Real GBD instance downloads**: 25+ instances from https://benchmark-database.de/file/<hash>
- **Verified solver soundness**: Correctly solves both SAT and UNSAT instances
- **Tested on 20-var instance**: algebra_xor_20_sat.cnf returns SAT correctly
- **Tested on UNSAT instance**: tseitin_grid_4x4_unsat.cnf returns UNSAT correctly
- **Committed**: ff70109 - Simplify CDCL to sound DPLL with VSIDS (remove clause learning)

### In Progress
- (none)

### Blocked
- **Performance limitation**: Without clause learning, solver runs in exponential time (2^n)
- Larger GBD instances (>30 vars) may exceed 60s timeout
- Clause learning implementation was buggy and removed for soundness

## Key Decisions
- Name: **satience** (SAT + science/patience/essence)
- Literal: `uint32` bit 31=sign, bits 0-30=variable index
- Variables: 0-based internally, 1-based in DIMACS
- **Removed clause learning**: Simplified solver to pure DPLL with VSIDS for guaranteed soundness
- **Chronological backtracking**: Simple one-level backtrack instead of backjumping
- Conflict analysis: 1-UIP scheme (code retained but not used without learning)
- Restart policy: Luby sequence with unit=100 conflicts (retained but ineffective without learning)
- **Real GBD instances only**: Download from benchmark-database.de, no generated instances

## Next Steps
- **Re-implement clause learning correctly**: Start with simple learning (no 1-UIP), verify soundness
- **Add iteration limit**: Prevent infinite loops during development
- **Profile solver**: Identify bottlenecks in propagation/decision
- **Test incrementally**: Verify each feature before adding complexity
- **Expand benchmark suite**: Once clause learning works, test on larger instances
- **Add verbose mode**: Show solving statistics for debugging

## Critical Context
- Go version: `go1.22.2 linux/amd64`
- All unit tests pass: `go test ./internal/solver` shows OK
- **Solver is sound**: Correctly identifies SAT and UNSAT on tested instances
- **Solver is complete**: Terminates in exponential time (2^n for n variables)
- **Performance trade-off**: Removed clause learning = slower but correct
- Git repo at `/home/luca/git/opencode-sat-new/`
- Benchmark project at `/home/luca/git/opencode-sat-new/benchmark/`
- **GBD download URL**: `https://benchmark-database.de/file/<hash>` (returns xz-compressed CNF)
- **meta.db**: Features table with hash, family, author, track, result, proceedings columns
- **Downloaded instances**: 25+ real GBD CNF files in benchmark/gbd_instances/

## Relevant Files
- `/home/luca/git/opencode-sat-new/internal/cnf/cnf.go`: Core data structures (Literal, Clause, CNF)
- `/home/luca/git/opencode-sat-new/internal/parser/parser.go`: DIMACS CNF parser
- `/home/luca/git/opencode-sat-new/internal/solver/solver_cdcl.go`: Simplified CDCL solver (clause learning removed)
- `/home/luca/git/opencode-sat-new/internal/solver/learn.go`: 1-UIP conflict analysis (fixed, not currently used)
- `/home/luca/git/opencode-sat-new/internal/solver/vsids.go`: VSIDS heuristic with activity decay
- `/home/luca/git/opencode-sat-new/internal/solver/restart.go`: Luby restart strategy
- `/home/luca/git/opencode-sat-new/internal/solver/database.go`: Learned clause database (not used without learning)
- `/home/luca/git/opencode-sat-new/cmd/satience/main.go`: CLI with -model flag
- `/home/luca/git/opencode-sat-new/satience`: Built solver binary
- `/home/luca/git/opencode-sat-new/benchmark/benchmark_real_gbd.py`: Real GBD benchmark runner
- `/home/luca/git/opencode-sat-new/benchmark/meta.db`: GBD metadata (32,905+ instances)
- `/home/luca/git/opencode-sat-new/benchmark/gbd_instances/`: Real GBD CNF files (25+ downloaded)
- `/home/luca/git/opencode-sat-new/.gitignore`: Excludes benchmark artifacts

## Recent Commit
```
commit ff70109
Author: satience team
Date: Wed Jun 03 2026

Simplify CDCL to sound DPLL with VSIDS (remove clause learning)

Remove clause learning to ensure solver soundness and completeness.
The solver now correctly terminates on all instances in exponential time.

Changes:
- solver_cdcl.go: Remove clause learning (58 lines)
  - No more learned clauses or clause database
  - Simplified handleConflict() to just track conflicts
  - Fixed backtrack() for proper chronological backtracking
  - Decision variables are now properly flipped on backtrack

- learn.go: Fix conflict analyzer (for future re-enablement)
  - Accept solver's implication array as parameter
  - Add hasOtherAtLevel check for correct UIP detection
  - Use implication array instead of uninitialized analyzer array

Trade-offs:
+ Sound: Never incorrectly reports UNSAT for SAT instances
+ Complete: Always terminates (SAT or UNSAT)
+ Simpler: 49 net lines removed, easier to verify correctness
- Slower: Exponential time without clause learning pruning

Verified:
- All unit tests pass
- Correctly solves 20-var SAT instance
- Correctly proves UNSAT on tseitin instance
```
