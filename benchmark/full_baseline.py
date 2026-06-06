#!/usr/bin/env python3
"""
Full baseline benchmark - all instances < 200 vars.
Tests complete small instance database.
"""

import subprocess
import time
import sqlite3
from pathlib import Path

TIMEOUT = 30
INSTANCE_DIR = Path("benchmark/gbd_instances")
DB_PATH = "benchmark/meta.db"

def get_expected_result(instance_name):
    """Get expected result from meta.db."""
    try:
        conn = sqlite3.connect(DB_PATH)
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

def run_solver(instance_path, timeout=TIMEOUT):
    """Run solver and return results."""
    try:
        start = time.time()
        result = subprocess.run(
            ["./satience", "-verbose", str(instance_path)],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start
        output = result.stdout + result.stderr
        
        if "UNSATISFIABLE" in output:
            sat_result = "UNSAT"
        elif "SATISFIABLE" in output:
            sat_result = "SAT"
        else:
            sat_result = "UNKNOWN"
        
        # Parse statistics
        stats = {}
        for line in output.split('\n'):
            if ':' in line:
                parts = line.split(':')
                if len(parts) == 2:
                    key = parts[0].strip()
                    val = parts[1].strip()
                    if key in ['Conflicts', 'Decisions', 'Variables', 'Clauses']:
                        try:
                            stats[key] = int(val)
                        except:
                            pass
        
        return sat_result, elapsed, stats
    except subprocess.TimeoutExpired:
        return "TIMEOUT", timeout, {}
    except Exception as e:
        return f"ERROR", 0, {}

def main():
    # Get all CNF files with < 200 vars
    cnf_files = sorted(INSTANCE_DIR.glob("*.cnf"))
    instances = []
    
    for cnf in cnf_files:
        with open(cnf, 'r') as f:
            for line in f:
                if line.startswith('p cnf'):
                    parts = line.split()
                    if len(parts) >= 3:
                        n_vars = int(parts[2])
                        if n_vars <= 200:
                            instances.append((cnf, n_vars))
                    break
    
    print(f"Testing {len(instances)} instances with ≤200 variables")
    print("="*110)
    print(f"{'Instance':<35} {'Vars':<6} {'Exp':<6} {'Result':<8} {'Time(s)':<10} {'Conflicts':<10} {'Decis':<8} {'Status':<8}")
    print("="*110)
    
    results = []
    by_type = {}
    
    for inst_path, n_vars in sorted(instances):
        inst_name = inst_path.name
        expected = get_expected_result(inst_name)
        
        sat_result, elapsed, stats = run_solver(inst_path)
        
        # Determine instance type from name
        inst_type = "unknown"
        for typ in ['algebra', 'php', 'arg_chain', 'tseitin', 'random', 'cardinality', 'sudoku']:
            if typ in inst_name.lower():
                inst_type = typ
                break
        
        if inst_type not in by_type:
            by_type[inst_type] = []
        by_type[inst_type].append((inst_name, sat_result, elapsed, stats.get('Conflicts'), stats.get('Decisions')))
        
        # Check correctness
        status = "?"
        if expected:
            if sat_result == expected:
                status = "✓"
            else:
                status = "✗ WRONG"
        
        time_str = f"{elapsed:.3f}" if elapsed < TIMEOUT else "TIMEOUT"
        confl_str = f"{stats.get('Conflicts', '-'):,}" if 'Conflicts' in stats else "-"
        decis_str = f"{stats.get('Decisions', '-'):,}" if 'Decisions' in stats else "-"
        
        print(f"{inst_name:<35} {n_vars:<6} {expected or 'unk':<6} {sat_result:<8} {time_str:<10} {confl_str:<10} {decis_str:<8} {status}")
        
        results.append({
            'name': inst_name,
            'vars': n_vars,
            'expected': expected,
            'result': sat_result,
            'time': elapsed,
            'conflicts': stats.get('Conflicts'),
            'decisions': stats.get('Decisions'),
            'type': inst_type
        })
    
    # Summary by type
    print("\n" + "="*110)
    print("SUMMARY BY INSTANCE TYPE:")
    print("="*110)
    
    for inst_type in sorted(by_type.keys()):
        type_results = by_type[inst_type]
        solved = sum(1 for _, r, _, _, _ in type_results if r != "TIMEOUT" and not r.startswith("ERROR"))
        total = len(type_results)
        times = [t for _, r, t, _, _ in type_results if r != "TIMEOUT" and not r.startswith("ERROR") and t < TIMEOUT]
        
        if times:
            median_time = sorted(times)[len(times)//2]
            print(f"{inst_type:<15}: {solved}/{total} solved, median time: {median_time:.3f}s")
        else:
            print(f"{inst_type:<15}: {solved}/{total} solved")
    
    # Overall summary
    print("\n" + "="*110)
    print("OVERALL SUMMARY:")
    print("="*110)
    
    testable = [r for r in results if r['expected']]
    correct = sum(1 for r in testable if r['result'] == r['expected'])
    wrong = sum(1 for r in testable if r['result'] != r['expected'])
    timeouts = sum(1 for r in results if r['result'] == 'TIMEOUT')
    
    print(f"Total instances: {len(results)}")
    print(f"Testable (with expected result): {len(testable)}")
    print(f"Correct: {correct}/{len(testable)} ({100*correct/len(testable):.1f}%)")
    print(f"Wrong: {wrong}/{len(testable)}")
    print(f"Timeouts: {timeouts}/{len(results)}")
    
    if timeouts > 0:
        print(f"\nTimeout instances:")
        for r in results:
            if r['result'] == 'TIMEOUT':
                print(f"  - {r['name']} ({r['vars']} vars, {r['type']})")

if __name__ == "__main__":
    main()
