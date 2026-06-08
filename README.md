# Satience - CDCL SAT Solver in Go

[![Go Version](https://img.shields.io/badge/go-1.22+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

**Satience** is a sound and complete CDCL (Conflict-Driven Clause Learning) SAT solver written in Go. It features modern SAT solving techniques including 1-UIP conflict analysis, adaptive restarts, LBD-based clause management, and efficient watched literals propagation.

## Features

### Core CDCL Engine
- **1-UIP Conflict Analysis**: Learns asserting clauses from conflicts
- **Backjumping**: Intelligent backtracking to relevant decision level
- **LBD Management**: Clause database pruning based on Literal Block Distance
- **Adaptive Restarts**: Glucose-style restart strategy (LBD > 1.5× average)
- **Phase Saving**: Remembers satisfying polarity for variables
- **Watched Literals**: O(1) propagation for binary and large clauses

### Variable Selection Heuristics
- **VSIDS**: Variable State Independent Decaying Sum (default)
- **LRB**: Learning Rate Based heuristic (via `-lrb` flag)
- **Conflict Participation**: Tracks variables involved in conflicts

### Preprocessing & Inprocessing
- Unit propagation
- Pure literal elimination
- Subsumption elimination
- Hyper-binary resolution
- Equivalence detection (union-find)
- Failed literal elimination
- Variable elimination (resolution-based)
- Blocked clause elimination (BCE)
- **Inprocessing**: Subsumption during search (every 500 conflicts)

### CLI Features
- Model output (`-model`)
- Verbose statistics (`-verbose`)
- CPU profiling (`-cpuprofile`)
- Iteration limits (`-max-iter`)
- SAT Competition 2026 compliant output format

## Installation

```bash
# Clone the repository
git clone https://github.com/yourusername/satience.git
cd satience

# Build the solver
go build -o satience ./cmd/satience
```

## Usage

### Basic Usage

```bash
# Solve a CNF file
./satience instance.cnf

# Print satisfying assignment
./satience -model instance.cnf

# Show solving statistics
./satience -verbose instance.cnf
```

### Output Format

Satience uses SAT Competition 2026 output format:

```
c Comment lines start with 'c'
s SATISFIABLE
v 1 -2 3 -4 5 0
```

**Exit Codes:**
- `10`: SATISFIABLE
- `20`: UNSATISFIABLE  
- `0`: UNKNOWN (timeout/resource limit)

### Examples

```bash
# Solve and show model
./satience -model benchmarks/php_5p_6h_sat.cnf

# Verbose output with statistics
./satience -verbose benchmarks/tseitin_5x5_unsat.cnf

# Use LRB heuristic
./satience -lrb instance.cnf

# Limit to 10000 iterations
./satience -max-iter 10000 instance.cnf
```

## Performance

Satience is optimized for correctness first, with performance optimizations for the propagation hot path. Benchmarks on real GBD instances (June 2026):

| Instance Type | Performance vs MiniSat |
|--------------|------------------------|
| Algebra/XOR | 1.5-2× slower |
| Argument chains | 1.7-2× slower |
| PHP (all sizes) | Correct ✓ |
| Tseitin (small) | Competitive |
| Cardinality constraints | Often faster |

**Note:** Performance varies by instance family. Watched literals propagation provides significant speedup on propagation-heavy instances compared to linear scanning.

## Testing

```bash
# Run all unit tests
go test ./internal/solver -v

# Run with race detector
go test -race ./internal/solver

# Build and test fuzzer
go build -o fuzz ./cmd/fuzz
./fuzz -n 100 -mode random
```

## Project Structure

```
satience/
├── cmd/
│   ├── satience/          # CLI application
│   └── fuzz/              # Fuzzer for testing
├── internal/
│   ├── cnf/               # CNF data structures
│   ├── parser/            # DIMACS parser
│   └── solver/            # CDCL solver implementation
├── benchmark/             # Benchmark infrastructure
└── AGENTS.md              # Development notes
```

## Architecture

### Data Structures
- **Literal**: `uint32` (bit 31 = sign, bits 0-30 = variable index)
- **Variables**: 0-based internally, 1-based in DIMACS
- **Watched Literals**: Two watches per clause for O(1) propagation

### Key Algorithms
1. **Conflict Analysis**: 1-UIP with clause minimization
2. **Backjumping**: Compute backjump level from learned clause
3. **Restart Policy**: Adaptive (Glucose) + Luby fallback
4. **Clause Database**: LBD-based deletion (max 10,000 learned)

## DIMACS CNF Format

Satience accepts standard DIMACS CNF format:

```
c Comment line
p cnf 5 6
1 2 3 0
-1 4 5 0
2 -3 0
...
```

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests: `go test ./...`
5. Submit a pull request

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Acknowledgments

Satience implements standard CDCL techniques from:
- MiniSat (Eén & Sörensson)
- Glucose (Audemard & Simon)
- SAT Competition winning solvers

Thanks to the SAT research community for excellent benchmarks and test instances.

## Contact

- **Issues**: https://github.com/yourusername/satience/issues
- **Discussion**: https://github.com/yourusername/satience/discussions
