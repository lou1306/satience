#!/usr/bin/env python3
"""
Benchmark satience solver on real GBD instances.
Selected for diverse families and reasonable solve times (<30s).
"""

import subprocess
import os
import sys
import time
from pathlib import Path

# Configuration
SOLVER = os.environ.get('SATIENCE_SOLVER', '../satience')
TIMEOUT = int(os.environ.get('SATIENCE_TIMEOUT', '60'))
INSTANCES_DIR = Path('gbd_instances')

# Real GBD instances (filename, family, expected_result)
# Focused on instances that should solve in reasonable time
INSTANCES = [
    # Algebra/XOR (sat)
    ('algebra_xor_20_sat.cnf', 'algebra-xor', 'sat'),
    ('algebra_xor_30_sat.cnf', 'algebra-xor', 'sat'),
    ('algebra_xor_40_sat.cnf', 'algebra-xor', 'sat'),
    
    # Pigeonhole (mixed)
    ('php_5p_6h_sat.cnf', 'pigeonhole', 'sat'),
    ('php_6p_5h_unsat.cnf', 'pigeonhole', 'unsat'),
    ('php_6p_7h_sat.cnf', 'pigeonhole', 'sat'),
    ('php_7p_6h_unsat.cnf', 'pigeonhole', 'unsat'),
    ('php_7p_8h_sat.cnf', 'pigeonhole', 'sat'),
    ('php_8p_7h_unsat.cnf', 'pigeonhole', 'unsat'),
    
    # Tseitin (mixed)
    ('tseitin_grid_4x4_unsat.cnf', 'tseitin', 'unsat'),
    ('tseitin_grid_5x5_sat.cnf', 'tseitin', 'sat'),
    ('tseitin_grid_5x5_unsat.cnf', 'tseitin', 'unsat'),
    ('tseitin_grid_6x6_sat.cnf', 'tseitin', 'sat'),
    ('tseitin_grid_6x6_unsat.cnf', 'tseitin', 'unsat'),
    ('tseitin_grid_7x7_sat.cnf', 'tseitin', 'sat'),
    
    # Argumentation chains (sat)
    ('arg_chain_50_sat.cnf', 'argumentation', 'sat'),
    ('arg_chain_100_sat.cnf', 'argumentation', 'sat'),
    ('arg_chain_150_sat.cnf', 'argumentation', 'sat'),
    
    # Random k-SAT (sat)
    ('random_k3_50v_200c_sat.cnf', 'random-k3', 'sat'),
    ('random_k3_75v_300c_sat.cnf', 'random-k3', 'sat'),
    ('random_k3_100v_400c_sat.cnf', 'random-k3', 'sat'),
    
    # Perfect matching (unsat) - real GBD
    ('747955bb7addf0fb6fe4d465a9cbd035.cnf', 'perfect-matching', 'unsat'),
    ('b5c3e33e90f4c95502754c7e2e92a6d2.cnf', 'perfect-matching', 'unsat'),
    ('e8c79a0ee6be39b6b2211c9a527c192a.cnf', 'perfect-matching', 'unsat'),
    ('912b89dd471295d3c72c66d0192dff72.cnf', 'perfect-matching', 'unsat'),
    
    # Sudoku (sat)
    ('sudoku_3x3_empty_sat.cnf', 'sudoku', 'sat'),
]

def run_solver(instance_path):
    """Run solver on instance, return (result, time, output)."""
    start = time.time()
    try:
        result = subprocess.run(
            [SOLVER, str(instance_path)],
            capture_output=True,
            text=True,
            timeout=TIMEOUT
        )
        elapsed = time.time() - start
        output = result.stdout.strip()
        
        # Parse result (check UNSAT first to avoid matching 'UNSAT' as 'SAT')
        if 'UNSAT' in output:
            return 'unsat', elapsed, output
        elif 'SAT' in output:
            return 'sat', elapsed, output
        else:
            return 'unknown', elapsed, output
    except subprocess.TimeoutExpired:
        return 'timeout', TIMEOUT, ''
    except Exception as e:
        return 'error', 0, str(e)

def main():
    print("=" * 90)
    print("Benchmarking satience on real GBD instances")
    print("=" * 90)
    print(f"Solver: {SOLVER}")
    print(f"Timeout: {TIMEOUT}s")
    print(f"Instances: {len(INSTANCES)}")
    print()
    
    results = []
    correct = 0
    total = 0
    
    for filename, family, expected in INSTANCES:
        instance_path = INSTANCES_DIR / filename
        
        if not instance_path.exists():
            print(f"SKIP: {filename:40s} (file not found)")
            continue
        
        total += 1
        result, elapsed, output = run_solver(instance_path)
        
        # Check correctness
        is_correct = (result == expected)
        if is_correct:
            correct += 1
        
        status = "✓" if is_correct else "✗"
        print(f"{status} {filename:40s} | {family:20s} | Exp: {expected:5s} | Got: {result:7s} | {elapsed:.3f}s")
        
        results.append({
            'file': filename,
            'family': family,
            'expected': expected,
            'result': result,
            'time': elapsed,
            'correct': is_correct
        })
    
    print()
    print("=" * 90)
    if total > 0:
        print(f"Results: {correct}/{total} correct ({100*correct/total:.1f}%)")
    else:
        print("No instances run!")
    print("=" * 90)
    
    # Save results
    import csv
    output_file = 'benchmark_real_gbd_results.csv'
    with open(output_file, 'w', newline='') as f:
        writer = csv.DictWriter(f, fieldnames=['file', 'family', 'expected', 'result', 'time', 'correct'])
        writer.writeheader()
        writer.writerows(results)
    
    print(f"Results saved to: {output_file}")
    
    return 0 if correct == total else 1

if __name__ == '__main__':
    sys.exit(main())
