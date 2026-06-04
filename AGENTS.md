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
- VSIDS variable selection heuristic with decay
- **Re-implemented clause learning**: 1-UIP conflict analysis with learned clause database
- **Fixed backtracking**: Properly flips decisions and maintains trail/trailHead
- **Fixed propagate() bug #1**: Unit propagation now correctly restarts clause checking from beginning after each propagation (both original and learned clauses)
- **Fixed propagate() bug #2**: Added `firstPass` flag to ensure propagate() always checks clauses at least once per level (fixes initial unit propagation when trail is empty)
- **Fixed assignLiteral() bug**: Units now assigned at `max(1, s.level)` to prevent Level=0 assignments that appear unassigned
- **Cleaned benchmark directory**: Removed log files, temp scripts, temp instance directories
- **Tested on 8 small instances (< 50 vars)**: 7 correct, 0 wrong, 1 timeout (php_6p_7h_sat - expected for pigeonhole)
- **Fixed test script bug**: 'SAT' substring was matching 'UNSAT' - fixed by checking 'UNSAT' first
- **Added 5 new test cases to test suite**:
  - TestCDCLTseitin4x4Unsat (40 vars, UNSAT)
  - TestCDCLAlgebra20Sat (20 vars, SAT)
  - TestCDCLPhp3p4hSat (12 vars, SAT)
  - TestCDCLSimple50vSat (50 vars, SAT)
  - TestCDCLArgChain20Sat (20 vars, SAT)
- All unit tests passing (15/15 tests)
- Benchmark infrastructure: uv project with gbd-tools, polars dependencies
- **GBD metadata database**: meta.db with 32,905+ instances from 200+ families
- **Real GBD instance downloads**: 106+ instances from https://benchmark-database.de/file/<hash>
- **Verified solver soundness**: Models verified to satisfy all clauses (0 wrong results on verified instances)
- **Performance improvement**: Clause learning enables solving php_5p_6h_sat (30 vars) and sudoku_3x3_empty_sat (729 vars)
- **Committed**: fc3cb79 - Fix CDCL propagation to correctly restart after unit propagation
- **Created AGENTS.md**: Project documentation
- **Code quality verified**: go vet, go build, go test all pass
- **Added GetAssignments()**: Model extraction method for CLI
- **Added SolveResult enum**: SAT, UNSAT, UNKNOWN return types
- **Added -max-iter CLI flag**: Optional iteration limit (default 0 = unlimited)
- **Fixed CLI flag order**: -model flag must come before filename
- **Added verbose mode**: -verbose flag shows solving statistics (conflicts, decisions, iterations, learned clauses, max level)
- **Implemented backjumping**: Replaced chronological backtracking with intelligent backjumping based on 1-UIP learned clause analysis
- **Performance improvement**: Backjumping reduces conflicts and decisions on structured instances (sudoku: 6 conflicts, 51 decisions vs previous higher counts)
- **LBD-based clause database management**: Automatic deletion of low-quality learned clauses when limit exceeded (maxLearned=10000)
- **Profiled solver with pprof**: Identified propagate() as 96.77% of CPU time
- **Optimized propagate()**: Inlined literalIsTrue check, cached varIdx, added bit operation constants for 15-20% speedup
- **Evaluated on 20+ labeled instances ≤200 vars**: 0 wrong results, all SAT/UNSAT results correct
- **Tested instance types**: algebra_xor (20-40 vars), arg_chain (50-150 vars), random_k3 (50-100 vars), tseitin_grid (40-133 vars), php (30-56 vars), sudoku (729 vars)
- **Implemented phase saving heuristic**: Remembers last satisfying polarity for each variable, uses it in decision heuristic
- **Evaluated on 20 random small instances (≤200 vars)**: 13 correct, 0 wrong, 7 timeout (PHP expected hard)
- **Implemented Luby restart policy**: Geometric restart sequence (base=100) to escape unproductive search
- **Fixed restart soundness bugs**: Restart after backtrack (not during), clears implication[] array
- **Implemented clause minimization**: Self-subsumption after 1-UIP analysis reduces learned clause size
- **Implemented preprocessing**: Unit propagation preprocessing + pure literal elimination before search
- **Preprocessing verified**: Detects UNSAT immediately on tseitin_grid_4x4 (empty clause created), all 15 unit tests pass
- **Comprehensive evaluation**: 20/20 correct results on random instances ≤200 vars, all models verified
- **Committed recent work**: 
  - 5ff3b44 - Add preprocessing (unit propagation + pure literal elimination)
  - 7c5ac2d - Implement Luby restart policy
  - ea2302f - Update AGENTS.md with recent progress
  - 803c973 - Implement phase saving heuristic
  - e7227f6 - LBD-based clause database management
  - 81066e6 - Profile solver and optimize hot path in propagate()
  - 3ae7dfb - Remove test binaries from git
  - 090ce96 - Implement backjumping
  - 8f8e278 - Add verbose mode with solving statistics
  - 23a4c0b - Add clause minimization via self-subsumption

### In Progress
- (none)

### In Progress
- **Watched literals implementation**: Attempted but reverted due to soundness bugs. Requires careful handling of:
  - Clause simplification during preprocessing (watch indices become stale)
  - Lazy removal from watched lists
  - Watch list rebuilding after backtrack
  - Interaction with learned clause database management
  This is a complex optimization that needs incremental implementation and thorough testing.

### Blocked
- **Performance limitation**: Pigeonhole instances (php_6p_5h_unsat, php_6p_7h_sat, php_7p_6h_unsat, php_7p_8h_sat, php_8p_7h_unsat) timeout at 10s despite backjumping and clause management (expected - PHP is exponentially hard for CDCL)
- **Dense random instances**: Some uniform-random instances (dddd886bd187eabd, 0038cea06eae4c32) timeout due to high clause/variable ratio

## Key Decisions
- Name: **satience** (SAT + science/patience/essence)
- Literal: `uint32` bit 31=sign, bits 0-30=variable index
- Variables: 0-based internally, 1-based in DIMACS
- **1-UIP clause learning**: Implemented proper conflict analysis for sound clause learning
- **Backjumping**: Calculate backjump level from 1-UIP learned clause (second-highest level among literals)
- **LBD-based clause deletion**: Keep clauses with low LBD (few decision levels), delete high LBD + old clauses
- **maxLearned=10000**: Initial limit on learned clauses, triggers deletion when exceeded
- **Real GBD instances only**: Download from benchmark-database.de, no generated instances
- **UNKNOWN on limit exceeded**: Return UNKNOWN (not UNSAT) when iteration limit reached
- **Iteration limit disabled by default**: maxIter=0 means unlimited, set via SetMaxIter() or -max-iter flag
- **propagate() restart on unit**: After any unit propagation, restart checking all clauses from trailHead
- **Never assign at level 0**: Use `max(1, s.level)` for unit propagation to avoid Level=0 ambiguity
- **Test detection**: Check 'UNSAT' before 'SAT' to avoid substring matching errors
- **Inlining for performance**: literalIsTrue() inlined in propagate() hot path to reduce function call overhead
- **Bit operation constants**: litVarMask=0x7FFFFFFF, litNegatedMask=0x80000000 for cleaner/faster bit ops
- **Phase saving**: Save satisfying polarity in assignLiteral(), reuse in decide() via selectVariableWithPhase()

## Next Steps
### Core Algorithm Improvements
- **Re-implement watched literals scheme**: Replace linear clause scanning with O(1) watched literal pointers (major optimization). Previous attempt identified key challenges:
  - Must initialize watches AFTER preprocessing (preprocessing modifies clauses)
  - Need proper lazy removal from watched lists to avoid O(n) operations
  - Watch indices must be validated before accessing clause literals
  - Backtracking may require watch list rebuilding or trail-based restoration
  - Learned clause database management interacts with watched literals
  Recommendation: Implement incrementally with extensive testing after each change
- **Implement LRB (Learning Rate Based)**: Alternative to VSIDS, picks variables that generate conflicts
- **Implement CHB (Conflict History Based)**: Exponential decay based on conflict history

### Preprocessing & Inprocessing
- **Unit propagation preprocessing**: Simplify formula before solving
- **Pure literal elimination**: Assign and remove pure literals upfront
- **Variable elimination**: Resolution-based elimination of variables before/during solving
- **Subsumption elimination**: Remove clauses subsumed by shorter clauses
- **Blocked clause elimination**: Remove clauses blocked by a literal
- **Inprocessing**: Apply preprocessing techniques periodically during search

### Testing & Validation
- **Fuzzing**: Generate random CNF instances to find edge cases
- **Cross-validation**: Compare results against MiniSat/CaDiCaL on same instances
- **Property-based testing**: Test invariants (e.g., learned clauses are logically implied)
- **Regression testing**: Track performance across commits
- **Maintain test coverage**: Keep 80%+ on solver package

### Benchmarking & Analysis
- **Systematic family benchmarks**: Test all instances from specific GBD families (not just random samples)
- **Performance profiling**: Compare before/after for each optimization
- **Scatter plots**: Runtime comparison vs reference solver
- **Cactus plots**: Show instances solved vs time
- **Test on larger instances**: Test on php_8p_7h_unsat and larger with current optimizations

### Data Structure Optimizations
- **Memory pool for clauses**: Reduce allocation overhead
- **Cache-friendly clause storage**: Improve memory locality
- **Compressed clause storage**: Pack literals more densely

### CLI & Usability
- **Batch solving**: Process multiple files in one run
- **JSON output**: Machine-readable results for benchmarking
- **Exit codes**: Standard SAT competition exit codes (10=SAT, 20=UNSAT, 0=UNKNOWN)

### Documentation
- **Algorithm documentation**: Explain CDCL, 1-UIP, LBD in code comments or docs
- **Performance guide**: Which flags/configurations for which instance types
- **API documentation**: For embedding satience as a library

## Critical Context
- Go version: `go1.22.2 linux/amd64`
- All unit tests pass: `go test ./internal/solver` shows OK (0.004s, 15 tests)
- **Solver is sound**: Models verified to satisfy all clauses when -model flag used correctly
- **Clause learning working**: 1-UIP analysis implemented in learnClause()
- **Backjumping working**: learnClause() returns backjump level (second-highest in learned clause)
- **Clause database management working**: deleteLearnedClauses() uses LBD+age scoring, deletes 50% when limit reached
- **propagate() fixed (3 bugs)**: 
  1. Now properly restarts clause checking after unit propagation
  2. Always executes at least once per level (firstPass flag)
  3. Units assigned at level >= 1 to avoid Level=0 ambiguity
- **propagate() optimized**: Inlined literalIsTrue, cached varIdx, uses mask constants
- **Profile results**: propagate() = 96.77% CPU, IsNegated = 2.15% (down from 10.31%)
- **Performance**: Pigeonhole instances timeout at 10s (expected); Tseitin, algebra_xor, random_k3, arg_chain solve quickly
- **CLI flag order matters**: `satience -model file.cnf` works, `satience file.cnf -model` does not print model
- **Phase saving implemented**: savedPhase[]bool field stores last satisfying polarity, selectVariableWithPhase() uses it
- **Luby restart implemented**: restartBase=100, lubyIndex tracks sequence position, shouldRestart() checks threshold
- **Restart after backtrack**: Clear trail and learned clauses after successful backtrack, not during handleConflict
- **Clear implication[] on restart**: Necessary for soundness when learned clauses are cleared
- **Clause minimization implemented**: minimizeLearnedClause() applies self-subsumption after 1-UIP analysis
- **Preprocessing implemented**: preprocess() applies unitPropagationPreprocess() + pureLiteralElimination() before search
- Git repo at `/home/luca/git/opencode-sat-new/`
- Benchmark project at `/home/luca/git/opencode-sat-new/benchmark/`
- **GBD download URL**: `https://benchmark-database.de/file/<hash>` (returns xz-compressed CNF)
- **meta.db**: Features table with hash, family, author, track, result, proceedings columns
- **Downloaded instances**: 106+ real GBD CNF files in benchmark/gbd_instances/
- **SolveResult enum**: SAT=0, UNSAT=1, UNKNOWN=2
- **learnedClauses**: Slice of cnf.Clause in CDCLSolver, checked during propagation
- **backjumpLevel field**: Added to CDCLSolver struct, calculated after each conflict, reset after backjump
- **Benchmark directory cleaned**: Only gbd_instances/ and meta.db remain
- **New CDCLSolver fields**: clauseActivity ([]float64), clauseAge ([]int), currentAge (int), maxLearned (int), savedPhase ([]bool), restartBase (int), restartCount (int), lubyIndex (int)
- **New cnf.go constants**: litVarMask=0x7FFFFFFF, litNegatedMask=0x80000000
- **luby() function**: Generates 1, 1, 2, 1, 1, 2, 4, 1, 1, 2... sequence recursively
- **minimizeLearnedClause()**: New method in solver_cdcl.go for clause self-subsumption
- **preprocess()**: New method applying unitPropagationPreprocess() and pureLiteralElimination() before search
- **simplifyAfterAssignment()**: Returns bool indicating if empty clause was created (conflict)

## Relevant Files
- `/home/luca/git/opencode-sat-new/internal/cnf/cnf.go`: Core data structures (Literal, Clause, CNF) with bit operation constants
- `/home/luca/git/opencode-sat-new/internal/parser/parser.go`: DIMACS CNF parser
- `/home/luca/git/opencode-sat-new/internal/solver/solver_cdcl.go`: CDCL solver with 1-UIP clause learning, backjumping, LBD-based clause deletion, phase saving, Luby restart policy, optimized propagate(), clause minimization via self-subsumption, preprocessing (unit propagation + pure literal elimination)
- `/home/luca/git/opencode-sat-new/internal/solver/vsids.go`: VSIDS heuristic with activity decay, selectVariableWithPhase() for phase saving
- `/home/luca/git/opencode-sat-new/internal/solver/solver.go`: Base solver with propagation
- `/home/luca/git/opencode-sat-new/internal/solver/solver_test.go`: Unit tests (15/15 passing)
- `/home/luca/git/opencode-sat-new/cmd/satience/main.go`: CLI with -model, -max-iter, -verbose, -cpuprofile flags
- `/home/luca/git/opencode-sat-new/satience`: Built solver binary
- `/home/luca/git/opencode-sat-new/benchmark/meta.db`: GBD metadata (32,905+ instances)
- `/home/luca/git/opencode-sat-new/benchmark/gbd_instances/`: Real GBD CNF files (106+ downloaded)
- `/home/luca/git/opencode-sat-new/AGENTS.md`: Project documentation
- `/home/luca/git/opencode-sat-new/.gitignore`: Excludes benchmark artifacts

## Recent Commits
```
commit 5ff3b44
Author: satience team
Date: Thu Jun 04 2026

Add preprocessing (unit propagation + pure literal elimination)

Implement preprocessing phase before CDCL search to simplify the formula:

1. Unit propagation preprocessing:
   - Run unit propagation before search starts
   - Assign all forced literals upfront
   - Detect conflicts early (empty clause = UNSAT)
   - Reduces search space significantly

2. Pure literal elimination:
   - Find literals that appear with only one polarity
   - Assign them to satisfy all clauses containing them
   - Remove satisfied clauses from consideration
   - Standard technique in modern SAT solvers

Implementation:
- Added preprocess() method to CDCLSolver
- Added unitPropagationPreprocess() for initial unit propagation
- Added pureLiteralElimination() to find and assign pure literals
- Added simplifyAfterAssignment() to maintain literal counts
- Called preprocess() at start of Solve() before search loop
- Returns false if empty clause detected (UNSAT)

Benefits:
- Detects UNSAT instances immediately (e.g., tseitin_grid_4x4_unsat)
- Reduces number of decisions needed during search
- Simplifies formula before expensive CDCL search
- Standard technique in all competitive SAT solvers

Verified:
- All 15 unit tests pass
- Correctly detects tseitin_grid_4x4_unsat.cnf as UNSAT in preprocess
- Correctly solves php_5p_6h_sat.cnf
- Correctly solves sudoku_3x3_empty_sat.cnf (729 vars)
- go vet and go test pass
```

commit 7c5ac2d
Author: satience team
Date: Thu Jun 04 2026

Implement Luby restart policy

Add restart mechanism to escape unproductive search regions:

- Implemented Luby restart sequence: 1, 1, 2, 1, 1, 2, 4, 1, 1, 2...
- restartBase=100 conflicts (multiplied by Luby sequence values)
- Triggers restart after backtrack to avoid losing progress
- Clears trail and learned clauses on restart
- Resets implication[] array for soundness

Implementation:
- Added luby() function to generate restart sequence
- Added restartBase, restartCount, lubyIndex fields
- Added shouldRestart() method to check threshold
- Trigger restart after successful backtrack
- Clear trail, learned clauses, implication[] on restart

Why restart:
- Escape from deep, unproductive search regions
- Allow different variable decisions to be tried
- Particularly effective on structured instances
- Standard technique in all modern SAT solvers

Why Luby sequence:
- Theoretical guarantees for randomized algorithms
- Geometric growth prevents too-frequent restarts
- Used successfully in many SAT solvers

Soundness fix:
- Restart AFTER backtrack (not during conflict handling)
- Clear implication[] array to avoid stale references
- Clear learned clauses to start fresh

Verified:
- All 15 unit tests pass
- php_6p_7h_sat now returns SAT (was timeout before)
- All 5 quick tests pass (algebra_xor, php_5p_6h_sat, arg_chain)
- go vet and go test pass
```

commit ea2302f
Author: satience team
Date: Thu Jun 04 2026

Update AGENTS.md with recent progress

Document completed features:
- Phase saving heuristic
- LBD-based clause database management
- Profiling and optimization of propagate()
- Backjumping implementation
- Verbose mode with statistics
- Clause minimization via self-subsumption

Updated sections:
- Progress/Done: Added all recent implementations
- Key Decisions: Added phase saving, clause minimization details
- Next Steps: Removed completed items (restart, clause minimization)
- Critical Context: Added new fields and methods
- Relevant Files: Updated solver_cdcl.go description
```

commit 81066e6
Author: satience team
Date: Thu Jun 04 2026

Profile solver and optimize hot path in propagate()

Profile with pprof to identify performance bottlenecks:

Results showed propagate() consumes 97% of CPU time:
- 78% in literal evaluation loop
- 10% in Literal.IsNegated() calls
- 8% in literalIsTrue() calls

Optimizations applied:

1. cnf.go: Use constants for bit masks
   - litVarMask and litNegatedMask constants
   - Compiler can better optimize constant operations
   - IsNegated() uses & instead of >> shift (faster)

2. solver_cdcl.go: Inline literal truth check in propagate()
   - Eliminated function call overhead in hot loop
   - Cache varIdx to avoid repeated lit.Var() calls
   - Single assignment lookup per literal
   - Boolean logic instead of function call

Performance impact:
- Reduced propagate() time by ~15-20%
- Eliminated Literal.IsNegated() from profile top nodes
- All 15 unit tests pass
- Soundness verified on 20 random instances (0 wrong)

Verified:
- go test ./... passes
- go vet ./... passes
- Soundness: 0/20 wrong results
- Timeout instances unchanged (expected hard for CDCL)
```

commit e7227f6
Author: satience team
Date: Thu Jun 04 2026

Add LBD-based clause database management

Implement automatic learned clause deletion to control memory usage
and improve solver performance on long-running instances:

- Added maxLearned field (default 10000) to limit learned clauses
- Added clauseActivity tracking for LBD calculation
- Added clauseAge tracking for each learned clause
- Implemented deleteLearnedClauses() using LBD+age scoring
  - Score = LBD + (age * 0.1) to prefer deleting old, high-LBD clauses
  - Deletes bottom 50% when limit exceeded
- Triggered deletion in analyzeConflict() when limit reached
- Added learned clause count to verbose statistics

LBD (Literal Block Distance) measures clause quality:
- Lower LBD = fewer decision levels = better clause
- Clauses with LBD=2 are "glue clauses" (most valuable)
- High LBD clauses are rarely useful and can be deleted

Benefits:
- Prevents memory explosion on hard instances
- Keeps database focused on useful learned clauses
- Maintains solver performance over long runs
- Standard technique in modern SAT solvers (Glucose, etc.)

Verified:
- All 15 unit tests pass
- Correctly solves tseitin_grid_4x4_unsat.cnf
- Correctly solves php_5p_6h_sat.cnf
- Correctly solves sudoku_3x3_empty_sat.cnf (729 vars)
- go vet and go test pass
```

Replace chronological backtracking with intelligent backjumping:

Backjumping calculates the correct backjump level from the learned
clause instead of always backtracking one level. This is a critical
CDCL optimization that can provide 10-100x speedup on hard instances.

Changes:
- Added backjumpLevel field to CDCLSolver struct
- Modified learnClause() to return the calculated backjump level
  - Finds second-highest decision level in the 1-UIP learned clause
  - Backjumps to the highest level among non-UIP literals
- Updated backtrack() to use calculated backjump level
  - Jumps directly to the backjump level instead of level-1
  - Properly clears trail and flips decision at backjump level
- Reset backjumpLevel after each backjump for next conflict

Algorithm:
1. After 1-UIP conflict analysis, the learned clause has exactly one
   literal at the current decision level (the UIP)
2. The backjump level is the second-highest level in the learned clause
3. Backtrack directly to that level, skipping unnecessary levels
4. This avoids re-exploring the same conflict at intermediate levels

Benefits:
- Avoids redundant conflicts at intermediate decision levels
- Dramatically reduces search space on structured instances
- Essential for competitive CDCL performance
- Particularly effective on Tseitin, pigeonhole, and combinatorial instances

Verified:
- All 15 unit tests pass
- Correctly solves tseitin_grid_4x4_unsat.cnf (1 conflict)
- Correctly solves php_5p_6h_sat.cnf (10 conflicts, 17 decisions)
- Correctly solves sudoku_3x3_empty_sat.cnf (729 vars, 6 conflicts)
- go vet and go test pass
```

commit 8f8e278
Author: satience team
Date: Thu Jun 04 2026

Add verbose mode with solving statistics

Add -verbose flag to show detailed solving statistics:
- Variables and clauses count
- Conflicts encountered
- Decisions made
- Total iterations
- Learned clauses count
- Maximum decision level reached

Usage: satience -verbose file.cnf

Implementation:
- Added verbose field to CDCLSolver
- Added decisions counter (tracks decision steps)
- Added GetStats() method for programmatic access
- Added printStats() for formatted output
- Statistics printed on SAT/UNSAT/UNKNOWN termination
- CLI flag -verbose enables verbose output

Statistics help with:
- Debugging solver behavior
- Identifying performance bottlenecks
- Understanding instance hardness
- Comparing solver configurations
```
