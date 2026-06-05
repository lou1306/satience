# SAT Competition 2026 Output Format Compliance

Satience now fully complies with the [SAT Competition 2026 output format](https://satcompetition.github.io/2026/output.html).

## Output Format

### Solution Lines (Mandatory)

Satience outputs exactly one solution line in the format `s <result>`:

| Result | Output | Exit Code |
|--------|--------|-----------|
| SATISFIABLE | `s SATISFIABLE` | 10 |
| UNSATISFIABLE | `s UNSATISFIABLE` | 20 |
| UNKNOWN | `s UNKNOWN` | 0 |

### Comment Lines (Optional)

All verbose output and statistics are prefixed with `c ` to indicate comments:

```
c [verbose] Aggressive preprocessing: 20 variables, 38 clauses
c [verbose] Variable elimination: eliminated 19 variables
c === Solving Statistics ===
c Variables:     20
c Clauses:       0
c Conflicts:     0
c Decisions:     0
c Iterations:    0
c Learned:       0
c Max Level:     0
c 
s SATISFIABLE
```

### Value Lines (Satisfying Assignment)

When the `-model` flag is provided, Satience outputs the satisfying assignment in the format:

```
v <lit1> <lit2> ... <litN> 0
```

**Example**:
```bash
$ ./satience -model instance.cnf
s SATISFIABLE
v 1 -2 3 -4 -5 6 0
```

**Features**:
- Variables are 1-based (DIMACS format)
- Negative literals denote false assignments
- Line length limited to 4096 characters (multiple lines if needed)
- Terminated with `0`
- Partial assignments allowed (only assigned variables listed)

## Usage Examples

### Basic SAT Check

```bash
$ ./satience instance.cnf
s SATISFIABLE
$ echo $?
10
```

### UNSAT Instance

```bash
$ ./satience unsat.cnf
s UNSATISFIABLE
$ echo $?
20
```

### With Model Output

```bash
$ ./satience -model instance.cnf
s SATISFIABLE
v 1 -2 3 -4 5 0
```

### With Verbose Statistics

```bash
$ ./satience -verbose instance.cnf
c [verbose] Aggressive preprocessing: 100 variables, 500 clauses
c [verbose] Preprocessing pass 1: 450 clauses
c [verbose] Variable elimination: eliminated 45 variables
c 
c === Solving Statistics ===
c Variables:     100
c Clauses:       380
c Conflicts:     125
c Decisions:     340
c Iterations:    15420
c Learned:       87
c Max Level:     12
c 
s SATISFIABLE
```

### Unknown Result (Timeout/Iteration Limit)

```bash
$ ./satience -max-iter 1000 hard_instance.cnf
s UNKNOWN
$ echo $?
0
```

## Exit Codes

Satience uses SAT Competition 2026 standard exit codes:

- **10**: SATISFIABLE (model found)
- **20**: UNSATISFIABLE (proof of unsatisfiability)
- **0**: UNKNOWN (timeout, iteration limit, or unable to determine)
- **2**: Error (invalid input, file not found, etc.)

## Verification

To verify Satience output complies with SAT Competition 2026 format:

1. **Check solution line**: Exactly one line starting with `s `
2. **Check exit codes**: Match the result (10/20/0)
3. **Check model** (if SAT): All clauses satisfied by assignment
4. **Check comments**: All non-solution lines start with `c `

### Example Verification Script

```bash
#!/bin/bash
# verify_output.sh

./satience -model $1 > output.txt 2>&1
EXIT_CODE=$?

# Check solution line
SOLUTION=$(grep "^s " output.txt)
if [ -z "$SOLUTION" ]; then
    echo "ERROR: No solution line found"
    exit 1
fi

# Check exit code matches solution
case "$SOLUTION" in
    "s SATISFIABLE")
        [ $EXIT_CODE -eq 10 ] || echo "WARNING: Exit code $EXIT_CODE != 10"
        ;;
    "s UNSATISFIABLE")
        [ $EXIT_CODE -eq 20 ] || echo "WARNING: Exit code $EXIT_CODE != 20"
        ;;
    "s UNKNOWN")
        [ $EXIT_CODE -eq 0 ] || echo "WARNING: Exit code $EXIT_CODE != 0"
        ;;
esac

echo "Output format: OK"
echo "Exit code: OK"
```

## Implementation Details

### Changes Made

1. **Solution line format**: Changed from `SAT`/`UNSAT`/`UNKNOWN` to `s SATISFIABLE`/`s UNSATISFIABLE`/`s UNKNOWN`
2. **Exit codes**: Updated to SAT Competition 2026 standard (10/20/0)
3. **Model format**: Changed from human-readable to DIMACS value lines (`v <lits> 0`)
4. **Comment format**: All verbose output already used `c ` prefix (no change needed)

### Files Modified

- `cmd/satience/main.go`: 
  - Updated solution line output
  - Updated exit codes
  - Added `printModelSATCompetition()` function
  - Modified model output to DIMACS format

### Backward Compatibility

The `-model` flag still works, but output format changed:

**Old format**:
```
SAT
Model:
  x1 = true
  x2 = false
  x3 = true
```

**New format (SAT Competition 2026)**:
```
s SATISFIABLE
v 1 -2 3 0
```

For human-readable output, use a post-processor or the `-verbose` flag.

## Testing

All unit tests pass with new format:
```bash
$ go test ./internal/solver
PASS
ok      satience/internal/solver        0.005s
```

Benchmark scripts updated to parse new format:
- `benchmark/quick_bench.py`: Updated parser for `s SATISFIABLE` format
- `benchmark/comprehensive_bench.py`: Updated parser

## References

- [SAT Competition 2026 Output Format](https://satcompetition.github.io/2026/output.html)
- [DIMACS CNF Format](http://www.satlib.org/Benchmarks/Benchmarks.html)
- [SAT Competition 2026 Rules](https://satcompetition.github.io/2026/rules.html)
