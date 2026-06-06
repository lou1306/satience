#!/usr/bin/env python3
"""
Baseline performance benchmark for satience solver.
Date: 2026-06-06
Status: Post watched-literals-bug fix (linear scanning for long clauses)
"""

import subprocess
import time
from pathlib import Path

TIMEOUT = 30
INSTANCE_DIR = Path("benchmark/gbd_instances")

# Representative test instances across different types
test_instances = [
    # Algebra/XOR - should be fast
    ("algebra_xor_20_sat.cnf", "SAT"),
    ("algebra_xor_40_sat.cnf", "SAT"),
    # PHP - SAT versions (easy)
    ("php_5p_6h_sat.cnf", "SAT"),
    ("php_6p_7h_sat.cnf", "SAT"),
    # Arg chain - structured
    ("arg_chain_50_sat.cnf", "SAT"),
    ("arg_chain_100_sat.cnf", "SAT"),
    ("arg_chain_150_sat.cnf", "SAT"),
    # Tseitin - UNSAT
    ("tseitin_grid_4x4_unsat.cnf", "UNSAT"),
    ("tseitin_grid_5x5_unsat.cnf", "UNSAT"),
    # Random k3
    ("random_k3_50_sat.cnf", "SAT"),
    ("random_k3_75_sat.cnf", "SAT"),
    ("random_k3_100_sat.cnf", "SAT"),
    # Cardinality (should be fast with our preprocessing)
    ("cardinality_100_sat.cnf", "SAT"),
    # Hard instances (expected to timeout or be slow)
    ("sudoku_3x3_empty_sat.cnf", "SAT"),
]

print("="*100)
print("SATIENCE BASELINE BENCHMARK - 2026-06-06")
print("Post watched-literals bug fix (linear scanning for clauses >= 4 literals)")
print("="*100)
print(f"{'Instance':<35} {'Vars':<6} {'Exp':<5} {'Result':<8} {'Time(s)':<10} {'Conflicts':<10} {'Decis':<8} {'Status':<8}")
print("="*100)

correct = 0
wrong = 0
timeouts = 0
all_times = []

for inst_name, expected in test_instances:
    inst_path = INSTANCE_DIR / inst_name
    if not inst_path.exists():
        print(f"{inst_name:<35} FILE NOT FOUND")
        continue
    
    # Get var count
    with open(inst_path, 'r') as f:
        n_vars = 0
        for line in f:
            if line.startswith('p cnf'):
                n_vars = int(line.split()[2])
                break
    
    try:
        start = time.time()
        # Use -verbose to get statistics
        result = subprocess.run(
            ["./satience", "-verbose", str(inst_path)],
            capture_output=True,
            text=True,
            timeout=TIMEOUT
        )
        elapsed = time.time() - start
        output = result.stdout + result.stderr
        
        if "UNSATISFIABLE" in output:
            sat_result = "UNSAT"
        elif "SATISFIABLE" in output:
            sat_result = "SAT"
        else:
            sat_result = "UNKNOWN"
        
        # Parse statistics from verbose output
        conflicts = decisions = None
        for line in output.split('\n'):
            if 'Conflicts:' in line:
                try:
                    conflicts = int(line.split(':')[1].strip())
                except:
                    pass
            elif 'Decisions:' in line:
                try:
                    decisions = int(line.split(':')[1].strip())
                except:
                    pass
        
        status = "✓" if sat_result == expected else "✗ WRONG"
        if sat_result == expected:
            correct += 1
        else:
            wrong += 1
        
        time_str = f"{elapsed:.3f}"
        confl_str = f"{conflicts:,}" if conflicts else "-"
        decis_str = f"{decisions:,}" if decisions else "-"
        
        print(f"{inst_name:<35} {n_vars:<6} {expected:<5} {sat_result:<8} {time_str:<10} {confl_str:<10} {decis_str:<8} {status}")
        
        if elapsed < TIMEOUT:
            all_times.append((inst_name, elapsed, conflicts, decisions))
        
    except subprocess.TimeoutExpired:
        timeouts += 1
        print(f"{inst_name:<35} {n_vars:<6} {expected:<5} TIMEOUT     {TIMEOUT:>5.1f}s       {'-':<10} {'-':<8} ✗")

print("="*100)
print(f"SUMMARY: {correct} correct, {wrong} wrong, {timeouts} timeouts out of {correct+wrong+timeouts} instances")

if all_times:
    times_only = [t[1] for t in all_times]
    print(f"\nTiming statistics (for solved instances):")
    print(f"  Min: {min(times_only):.3f}s")
    print(f"  Max: {max(times_only):.3f}s")
    print(f"  Median: {sorted(times_only)[len(times_only)//2]:.3f}s")
    print(f"  Mean: {sum(times_only)/len(times_only):.3f}s")

print("="*100)

