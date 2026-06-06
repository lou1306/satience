#!/usr/bin/env python3
"""
Baseline performance benchmark for satience solver.
Tests all small instances (< 200 vars).
"""

import subprocess
import time
import os
import sqlite3
from pathlib import Path

TIMEOUT = 60  # seconds
INSTANCE_DIR = Path("benchmark/gbd_instances")

def run_solver(solver_cmd, instance_path, timeout=TIMEOUT):
    """Run solver and return (result, time, conflicts, decisions)."""
    try:
        start = time.time()
        result = subprocess.run(
            solver_cmd + [str(instance_path)],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start
        
        output = result.stdout + result.stderr
        
        # Parse result
        if "SATISFIABLE" in output:
            sat_result = "SAT"
        elif "UNSATISFIABLE" in output:
            sat_result = "UNSAT"
        else:
            sat_result = "UNKNOWN"
        
        # Parse statistics if available
        conflicts = decisions = None
        for line in output.split('\n'):
            if line.startswith('c Conflicts:'):
                conflicts = int(line.split(':')[1].strip())
            elif line.startswith('c Decisions:'):
                decisions = int(line.split(':')[1].strip())
        
        return sat_result, elapsed, conflicts, decisions
    except subprocess.TimeoutExpired:
        return "TIMEOUT", timeout, None, None
    except Exception as e:
        return f"ERROR: {e}", 0, None, None

def get_expected_result(db_path, instance_name):
    """Get expected SAT/UNSAT result from meta.db."""
    try:
        conn = sqlite3.connect(db_path)
        cursor = conn.cursor()
        cursor.execute(
            "SELECT result FROM metadata WHERE hash = ?",
            (instance_name.replace('.cnf', ''),)
        )
        row = cursor.fetchone()
        conn.close()
        if row:
            return row[0]
    except:
        pass
    return None

def main():
    # Get all CNF files
    cnf_files = sorted(INSTANCE_DIR.glob("*.cnf"))
    
    # Filter to small instances (< 200 vars) for quick testing
    small_instances = []
    for cnf in cnf_files:
        # Read header to get variable count
        with open(cnf, 'r') as f:
            for line in f:
                if line.startswith('p cnf'):
                    parts = line.split()
                    if len(parts) >= 3:
                        n_vars = int(parts[2])
                        if n_vars <= 200:
                            small_instances.append((cnf, n_vars))
                    break
    
    print(f"Testing {len(small_instances)} instances with ≤200 variables")
    print(f"{'Instance':<35} {'Vars':<6} {'Exp':<5} {'Result':<8} {'Time(s)':<10} {'Confl':<10} {'Decis':<8} {'Status':<10}")
    print("="*100)
    
    db_path = "benchmark/meta.db"
    results = []
    
    for instance_path, n_vars in small_instances:
        instance_name = instance_path.name
        
        # Get expected result
        expected = get_expected_result(db_path, instance_name)
        
        # Run satience
        sat_result, sat_time, sat_conflicts, sat_decisions = run_solver(
            ["./satience"], instance_path
        )
        
        # Check correctness
        if expected:
            status = "✓" if sat_result == expected else "✗ WRONG"
        else:
            status = "?"
        
        time_str = f"{sat_time:.3f}" if sat_time < TIMEOUT else "TIMEOUT"
        confl_str = f"{sat_conflicts:,}" if sat_conflicts else "-"
        decis_str = f"{sat_decisions:,}" if sat_decisions else "-"
        
        print(f"{instance_name:<35} {n_vars:<6} {expected or 'unk':<5} {sat_result:<8} {time_str:<10} {confl_str:<10} {decis_str:<8} {status}")
        
        results.append({
            'instance': instance_name,
            'vars': n_vars,
            'expected': expected,
            'result': sat_result,
            'time': sat_time,
            'conflicts': sat_conflicts,
            'decisions': sat_decisions
        })
    
    # Summary
    print("\n" + "="*100)
    print("SUMMARY:")
    
    testable = [r for r in results if r['expected']]
    correct = sum(1 for r in testable if r['result'] == r['expected'])
    wrong = sum(1 for r in testable if r['result'] != r['expected'])
    timeouts = sum(1 for r in results if r['result'] == 'TIMEOUT')
    
    print(f"  Testable instances: {len(testable)}")
    print(f"  Correct: {correct}/{len(testable)} ({100*correct/len(testable):.1f}%)")
    print(f"  Wrong: {wrong}/{len(testable)}")
    print(f"  Timeouts: {timeouts}/{len(results)}")
    
    # Analyze timeouts
    if timeouts > 0:
        print(f"\n  Timeout instances ({timeouts}):")
        for r in results:
            if r['result'] == 'TIMEOUT':
                print(f"    - {r['instance']} ({r['vars']} vars, {r['expected'] or 'unknown'})")
    
    # Analyze by result type
    sat_times = [r['time'] for r in results if r['result'] == 'SAT' and r['time'] < TIMEOUT]
    unsat_times = [r['time'] for r in results if r['result'] == 'UNSAT' and r['time'] < TIMEOUT]
    
    if sat_times:
        print(f"\n  SAT instances: {len(sat_times)} solved")
        print(f"    Min: {min(sat_times):.3f}s, Max: {max(sat_times):.3f}s")
        print(f"    Median: {sorted(sat_times)[len(sat_times)//2]:.3f}s")
    
    if unsat_times:
        print(f"\n  UNSAT instances: {len(unsat_times)} solved")
        print(f"    Min: {min(unsat_times):.3f}s, Max: {max(unsat_times):.3f}s")
        print(f"    Median: {sorted(unsat_times)[len(unsat_times)//2]:.3f}s")

if __name__ == "__main__":
    main()
