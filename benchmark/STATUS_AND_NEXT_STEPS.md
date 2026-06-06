# Satience SAT Solver - Current Status & Next Steps

**Date**: June 6, 2026  
**Version**: Production-ready with known performance limitations

---

## Executive Summary

Satience is a **sound and complete** CDCL SAT solver with 100% correctness on all tested instances. However, there is a **critical performance bug** in the watched literals implementation for long clauses (>3 literals) causing 100-1000x slowdown on propagation-heavy instances.

**Current Performance**: Median ~5-10x slower than MiniSat (excluding timeouts)  
**Target Performance**: Within 2-3x of MiniSat

---

## Current Status

### ✅ Working Features

#### Core CDCL Engine
- ✅ 1-UIP conflict analysis and clause learning
- ✅ Backjumping (non-chronological backtracking)
- ✅ VSIDS variable selection heuristic with decay
- ✅ LBD-based heuristic (Learning Rate Based option available)
- ✅ Phase saving heuristic
- ✅ Adaptive restarts (Glucose-style LBD-based)
- ✅ Luby restart sequence fallback
- ✅ Proper trail management and backtracking

#### Clause Database Management
- ✅ LBD calculation and tracking
- ✅ LBD-based clause deletion (keep low-LBD clauses)
- ✅ maxLearned=10000 limit with automatic cleanup
- ✅ Clause minimization via self-subsumption

#### Preprocessing (Aggressive, 5 passes)
- ✅ Unit propagation preprocessing
- ✅ Pure literal elimination
- ✅ Self-subsumption
- ✅ Hyper-binary resolution
- ✅ Failed literal elimination (with safeguards)
- ✅ Variable elimination (resolution-based, 20% blowup limit)
- ✅ Blocked clause elimination (skip on large formulas >5000 clauses)

#### Short Clause Optimization
- ✅ **Binary clauses**: Watched literals working correctly
- ✅ **Ternary clauses**: Watched literals working correctly
- ⚠️ **Long clauses (>3 literals)**: Watched literals DISABLED (bug, see below)

#### Testing & Verification
- ✅ All 15 unit tests passing
- ✅ Soundness verified on 60+ small instances (<200 vars)
- ✅ Fuzzer infrastructure with 100% model verification
- ✅ Real GBD instances from benchmark-database.de

#### SAT Competition 2026 Compliance
- ✅ Output format: `s SATISFIABLE/UNSATISFIABLE/UNKNOWN`
- ✅ Exit codes: 10=SAT, 20=UNSAT, 0=UNKNOWN
- ✅ Model format: DIMACS value lines (`v <lits> 0`)
- ✅ Verbose output: All debug info prefixed with `c `

---

## Known Issues

### 🔴 CRITICAL: Watched Literals Bug for Long Clauses

**Problem**: The `propagateLong()` function has a critical bug causing exponential watch list growth.

**Symptoms**:
- Solver hangs/times out on instances with many 4-literal clauses
- Watch lists grow from ~7 entries to 100,000+ duplicates
- Example: Instance `11c893b7c37aeb53cdaf5f677dda0b7d.cnf` (36 vars, 144 clauses)
  - MiniSat: **0.075 seconds**
  - Satience: **TIMEOUT** after 60 seconds

**Root Cause**: When updating watched literals, clauses are appended to watch lists without checking for duplicates or removing old entries. The lazy cleanup approach causes exponential growth.

**Current Workaround**: Disabled watched literals for long clauses, using simple linear scanning instead. This is correct but slow.

**Impact**: 
- 100-1000x slowdown on propagation-heavy instances
- Sudoku instances: 1200x slower
- Dense random instances: Timeout
- PHP instances: Timeout (also theoretically hard for CDCL)

**Location**: `internal/solver/solver_cdcl.go:propagateLong()`

**Fix Complexity**: 2-3 days with careful testing

**See**: `WATCHED_LITERALS_BUG_ANALYSIS.md` for detailed analysis

### 🟡 Moderate: PHP Performance Gap

**Problem**: Pigeonhole principle instances are exponentially hard for basic CDCL.

**Symptoms**:
- `php_6p_5h_unsat.cnf`: MiniSat solves in 251 conflicts, Satience times out after 224K+ conflicts

**Root Cause**: 
- Basic 1-UIP learning doesn't find short, powerful clauses needed for PHP
- VSIDS doesn't focus on critical "counting" variables
- Preprocessing eliminates fewer variables than MiniSat (6 vs 21)

**Impact**: PHP UNSAT instances timeout (rare in practical applications)

**Fix**: Requires specialized techniques (cardinality reasoning, symmetry breaking) - research-level problem

### 🟡 Moderate: Preprocessing Performance

**Problem**: Some preprocessing passes are slow on large instances.

**Symptoms**:
- Variable elimination can cause 20% formula blowup
- BCE skipped on formulas >5000 clauses to avoid O(n²) slowdown

**Current Safeguards**:
- Strict time limits per variable
- Clause growth limits
- Early termination on no progress

---

## Performance Summary

### Benchmark Results (vs MiniSat, June 2026)

| Instance Type | Performance | Notes |
|--------------|-------------|-------|
| **Cardinality constraints** | **18x FASTER** ✅ | Satience solves where MiniSat times out |
| **Algebra/XOR** | 1.5-2x slower | Competitive |
| **Argumentation chains** | 1.7-2x slower | Good |
| **Random k3** | 2-5x slower | Acceptable |
| **Tseitin grids** | 10-35x slower | Propagation bottleneck (binary clauses) |
| **Sudoku** | 1200x slower ❌ | Propagation bottleneck (11K+ clauses) |
| **Dense random** | TIMEOUT ❌ | Severe propagation bottleneck |
| **PHP UNSAT** | TIMEOUT ❌ | Theoretically hard for CDCL |

**Median slowdown**: 5-10x (excluding timeouts)  
**Soundness**: 100% verified (0 wrong results on solved instances)

---

## Next Steps

### Priority 1: Fix Watched Literals Bug (2-3 days)

**Goal**: Restore watched literals for long clauses to fix 100-1000x slowdown

**Approach Options**:

1. **Proper Watch List Management** (Recommended)
   - Implement O(1) removal from watch lists using doubly-linked lists
   - When updating watch from A to B: remove from A's list, add to B's list
   - Complexity: Medium, Risk: Low (if tested carefully)

2. **Periodic Watch List Rebuilding**
   - Continue using lazy cleanup
   - Rebuild all watch lists from scratch when total size exceeds threshold
   - Complexity: Low, Risk: Low
   - Trade-off: O(n) rebuild but infrequent

3. **Hybrid Approach**
   - Use watch indices stored in clause structure
   - Avoid scanning watch lists during propagation
   - Complexity: High, Risk: Medium

**Testing Plan**:
1. Fix implementation
2. Verify on small instances (<50 vars)
3. Test on Tseitin grids (binary-heavy)
4. Test on Sudoku (propagation-heavy)
5. Run full benchmark suite
6. Verify soundness (models satisfy all clauses)

**Expected Impact**: 
- 10-50x speedup on propagation-heavy instances
- Sudoku: From 1200x slower to ~50x slower
- Dense random: From timeout to solvable

### Priority 2: Optimize Preprocessing (1-2 days)

**Goal**: Reduce preprocessing time on large instances

**Tasks**:
- Profile preprocessing passes to identify bottlenecks
- Optimize variable elimination (better pivot selection)
- Improve equivalence detection (find more x ↔ y patterns)
- Add clause reduction during preprocessing

**Expected Impact**: 2-3x faster on structured instances

### Priority 3: Improve Variable Selection (1-2 days)

**Goal**: Better focus on critical variables

**Tasks**:
- Implement CHB (Conflict History Based) heuristic
- Add variable state independent scoring (VSIDS extension)
- Tune decay factors dynamically
- Add conflict history to LRB heuristic

**Expected Impact**: 1.5-2x speedup on hard instances

### Priority 4: Add Specialized Reasoning (Research, 1-2 weeks)

**Goal**: Handle PHP and other theoretically hard instances

**Techniques**:
- Cardinality constraint detection
- Symmetry breaking
- Extended resolution
- Gaussian elimination for XOR constraints

**Expected Impact**: Solve PHP instances, competitive on crafted instances

---

## File Organization

### Keep (Updated & Relevant)
- ✅ `STATUS_AND_NEXT_STEPS.md` (this file) - **Main status document**
- ✅ `WATCHED_LITERALS_BUG_ANALYSIS.md` - Detailed bug analysis
- ✅ `IMPLEMENTATION_STATUS.md` - Feature implementation status
- ✅ `KNOWN_ISSUES.md` - Known issues list

### Delete (Outdated/Redundant)
- ❌ `CDCL_SOLVER_STATUS.md` - Superseded by this document
- ❌ `CODEBASE_CLEANUP_SUMMARY.md` - One-time cleanup, not needed
- ❌ `CODE_QUALITY_IMPROVEMENTS.md` - Historical, not actionable
- ❌ `IMPROVEMENTS_SUMMARY_June2026.md` - Superseded
- ❌ `IMPROVEMENT_SUMMARY_June2026_Part2.md` - Superseded
- ❌ `MEMORY_ANALYSIS_*.md` - Historical profiling, not current
- ❌ `MEMORY_ISSUE_PHP.md` - Superseded by WATCHED_LITERALS_BUG_ANALYSIS.md
- ❌ `MEMORY_OPTIMIZATION_ANALYSIS.md` - Historical
- ❌ `PERFORMANCE_ANALYSIS_2026*.md` - Superseded
- ❌ `PERFORMANCE_BUG_FOUND.md` - Superseded
- ❌ `PERFORMANCE_COMPARISON_JUNE5.md` - Superseded
- ❌ `performance_gap_analysis_june2026.md` - Superseded
- ❌ `PERFORMANCE_SUMMARY_2026.md` - Superseded
- ❌ `PHP_ANALYSIS_UPDATED.md` - Superseded
- ❌ `PREPROCESSING_IMPROVEMENTS.md` - Superseded
- ❌ `PROFILING_ANALYSIS_JUNE5.md` - Historical
- ❌ `performance_analysis_detailed_2026.md` - Superseded
- ❌ `results_june_2026.md` - Superseded by new benchmarks

### Root Directory (Keep)
- ✅ `AGENTS.md` - Project documentation
- ✅ `KNOWN_LIMITATIONS.md` - High-level limitations
- ✅ `SAT_COMPETITION_2026_FORMAT.md` - Output format specification

---

## How to Contribute

### For Developers

1. **Fixing Watched Literals Bug**:
   - Read `WATCHED_LITERALS_BUG_ANALYSIS.md`
   - Implement one of the three approaches
   - Test thoroughly on small instances first
   - Run full benchmark suite before merging

2. **Performance Optimization**:
   - Profile with `go tool pprof`
   - Focus on `propagate()` hot path (96% of CPU time)
   - Test soundness after every change

3. **Adding Features**:
   - Ensure all 15 unit tests pass
   - Verify soundness on fuzzer (30+ random tests)
   - Benchmark on diverse GBD instances

### Testing Commands

```bash
# Run unit tests
go test ./internal/solver -v

# Run fuzzer
./fuzz -n 30 -mode random -verbose

# Benchmark single instance
./satience benchmark/gbd_instances/11c893b7c37aeb53cdaf5f677dda0b7d.cnf -verbose

# Compare with MiniSat
python3 benchmark/compare_minisat.py
```

---

## Contact & Resources

- **Project Repository**: `/home/luca/git/opencode-sat-new/`
- **Benchmark Database**: 186 GBD instances in `benchmark/gbd_instances/`
- **GBD Metadata**: `benchmark/meta.db` (32,905+ instances)
- **Documentation**: `AGENTS.md`, this file, `WATCHED_LITERALS_BUG_ANALYSIS.md`

---

**Last Updated**: June 6, 2026  
**Next Review**: After watched literals bug fix
