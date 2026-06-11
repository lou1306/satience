#!/usr/bin/env python3
"""Quick baseline benchmark - instances that solve in < 5 seconds."""

import subprocess
import time
from pathlib import Path

TIMEOUT = 5
INSTANCE_DIR = Path("benchmark/gbd_instances")

test_instances = [
    "algebra_xor_20_sat.cnf",
    "algebra_xor_40_sat.cnf",
    "php_5p_6h_sat.cnf",
    "php_6p_7h_sat.cnf",
    "arg_chain_50_sat.cnf",
    "arg_chain_100_sat.cnf",
    "arg_chain_150_sat.cnf",
    "tseitin_grid_4x4_unsat.cnf",
    "tseitin_grid_5x5_unsat.cnf",
    "random_k3_50_sat.cnf",
    "random_k3_75_sat.cnf",
    "random_k3_100_sat.cnf",
]

print(f"{'Instance':<35} {'Vars':<6} {'Result':<8} {'Time(s)':<10} {'Conflicts':<12} {'Decisions':<10} {'Status':<8}")
print("="*95)

correct = 0
total = 0
timeouts = 0

for inst_name in test_instances:
    inst_path = INSTANCE_DIR / inst_name
    if not inst_path.exists():
        continue
    
    with open(inst_path, 'r') as f:
        n_vars = 0
        for line in f:
            if line.startswith('p cnf'):
                n_vars = int(line.split()[2])
                break
    
    try:
        start = time.time()
        result = subprocess.run(
            ["./satience", str(inst_path)],
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
        
        conflicts = decisions = "-"
        for line in output.split('\n'):
            if line.startswith('c Conflicts:'):
                conflicts = line.split(':')[1].strip()
            elif line.startswith('c Decisions:'):
                decisions = line.split(':')[1].strip()
        
        # Expected: check unsat BEFORE sat
        expected = None
        if "_unsat" in inst_name.lower():
            expected = "UNSAT"
        elif "_sat" in inst_name.lower():
            expected = "SAT"
        
        status = "?"
        if expected:
            if sat_result == expected:
                status = "✓"
                correct += 1
            else:
                status = "✗ WRONG"
        total += 1
        
        print(f"{inst_name:<35} {n_vars:<6} {sat_result:<8} {elapsed:<10.4f} {conflicts:<12} {decisions:<10} {status}")
        
    except subprocess.TimeoutExpired:
        timeouts += 1
        total += 1
        print(f"{inst_name:<35} {n_vars:<6} TIMEOUT     {TIMEOUT:>5.1f}s       {'-':<12} {'-':<10} ✗")

print("="*95)
print(f"Results: {correct}/{total} correct, {timeouts} timeouts")

