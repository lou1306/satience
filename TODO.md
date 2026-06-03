# satience TODO

A performant CDCL SAT solver in Go

---

## Phase 1: Minimal Viable Solver

- [x] Project structure & Go module
- [x] Core data structures
  - [x] Literal type (bit-packed: sign + variable index)
  - [x] Clause type (literals, learned flag)
  - [x] CNF formula (clauses, num vars)
- [x] DIMACS parser
  - [x] Parse `p cnf` header
  - [x] Parse clauses
  - [x] Handle comments
- [x] Basic DPLL solver
  - [x] Unit propagation
  - [x] Decision heuristic (simple: first unassigned)
  - [x] Backtracking
- [x] CLI
  - [x] Read DIMACS file
  - [x] Print SAT/UNSAT
  - [x] Exit codes (0=SAT, 1=UNSAT, 2=ERROR)
- [x] Unit tests

---

## Phase 2: CDCL Engine

- [ ] Watched literals
  - [ ] Watch list data structure
  - [ ] Efficient unit propagation
- [ ] Conflict-driven clause learning
  - [ ] Implication graph tracking
  - [ ] 1UIP conflict analysis
  - [ ] Learn new clauses
- [ ] VSIDS variable selection
  - [ ] Activity tracking
  - [ ] Decay mechanism
  - [ ] Priority queue (heap)
- [ ] Phase saving

---

## Phase 3: Performance Optimizations

- [ ] Restart policies
  - [ ] Luby sequence
  - [ ] Geometric
  - [ ] Configurable
- [ ] Clause database management
  - [ ] LBD (glue) scoring
  - [ ] Clause deletion strategy
  - [ ] Keep glue clauses
- [ ] Memory efficiency
  - [ ] Pre-allocate slices
  - [ ] Pool reused structures

---

## Phase 4: Testing & Benchmarking

- [ ] Unit tests
  - [ ] Parser tests
  - [ ] Solver tests
  - [ ] Edge cases
- [ ] Integration tests
  - [ ] SATLIB instances
  - [ ] Known SAT/UNSAT problems
- [ ] Benchmarks
  - [ ] Compare vs MiniSat
  - [ ] Track performance over time

---

## Phase 5: Future Extensions (Out of Scope)

- [ ] Incremental solving
- [ ] Parallel portfolio solving
- [ ] DRAT proof generation
- [ ] MaxSAT (WCNF) support
- [ ] Preprocessing (variable elimination, subsumption)
- [ ] Public Go API for embedding

---

## Implementation Order

1. **Data structures** (`internal/cnf/`)
2. **Parser** (`internal/parser/`)
3. **DPLL solver** (`internal/solver/`)
4. **CLI** (`cmd/satience/`)
5. **Tests**
6. **CDCL upgrade** (replace DPLL with full CDCL)
