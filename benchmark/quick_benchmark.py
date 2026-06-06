#!/usr/bin/env python3
"""
Quick benchmark comparing Satience vs MiniSat on small instances
"""

import subprocess
import time
import os
from pathlib import Path

# Test instances (hash - expected result - vars approx)
TEST_INSTANCES = [
    # Small/fast instances
    ('algebra_xor_20v', 'benchmark/gbd_instances/109aa0f5e177c1efb72f133a6f8c723b.cnf', None),
    ('tseitin_4x4_unsat', 'benchmark/gbd_instances/11c893b7c37aeb53cdaf5f677dda0b7d.cnf', 'UNSAT'),
    ('arg_chain_50v', 'benchmark/gbd_instances/1a3320d3cf32f211b3e7b875745713e2.cnf', 'UNSAT'),
    ('cardinality_22v', 'benchmark/gbd_instances/02223564bd2f5c20768e63cf28c785e3.cnf', 'SAT'),
    ('coloring_27v', 'benchmark/gbd_instances/0f4576a6e7399336e11f0828d32263dd.cnf', 'SAT'),
    ('bitvector_30v', 'benchmark/gbd_instances/0f877a1f984f35fdfdce011cb7152123.cnf', 'UNSAT'),
    ('planning_35v', 'benchmark/gbd_instances/1a3320d3cf32f211b3e7b875745713e2.cnf', 'UNSAT'),
    ('clique_35v', 'benchmark/gbd_instances/172ecb98a80b859e62612ff192a53729.cnf', 'UNSAT'),
    ('bounded_model_50v', 'benchmark/gbd_instances/18f54820956791d3028868b56a09c6cd.cnf', 'UNSAT'),  # Equivalence-rich
    ('diagnosis_42v', 'benchmark/gbd_instances/24075575c3dbd9e9eae948dd2241b029.cnf', 'UNSAT'),
]

def run_satience(cnf_path, timeout=60):
    """Run Satience, return (result, time, conflicts, decisions)"""
    try:
        start = time.time()
        result = subprocess.run(
            ['./satience', '-verbose', cnf_path],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start

        conflicts = 0
        decisions = 0
        for line in result.stderr.split('\n'):
            if 'Conflicts:' in line:
                conflicts = int(line.split(':')[1].strip())
            elif 'Decisions:' in line:
                decisions = int(line.split(':')[1].strip())

        if result.returncode == 10:
            result_str = 'SAT'
        elif result.returncode == 20:
            result_str = 'UNSAT'
        else:
            result_str = 'TIMEOUT' if elapsed >= timeout else 'UNKNOWN'

        return result_str, elapsed, conflicts, decisions
    except subprocess.TimeoutExpired:
        return 'TIMEOUT', timeout, 0, 0
    except Exception as e:
        return f'ERROR: {e}', 0, 0, 0

def run_minisat(cnf_path, timeout=60):
    """Run MiniSat, return (result, time, conflicts)"""
    try:
        start = time.time()
        result = subprocess.run(
            ['minisat', cnf_path, '/tmp/minisat_out.txt'],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start

        conflicts = 0
        try:
            with open('/tmp/minisat_out.txt', 'r') as f:
                for line in f:
                    if line.startswith('conflicts'):
                        parts = line.split()
                        if len(parts) >= 2:
                            conflicts = int(parts[1].replace(',', ''))
                        break
        except:
            pass

        if result.returncode == 10:
            result_str = 'SAT'
        elif result.returncode == 20:
            result_str = 'UNSAT'
        else:
            result_str = 'TIMEOUT' if elapsed >= timeout else 'UNKNOWN'

        return result_str, elapsed, conflicts
    except subprocess.TimeoutExpired:
        return 'TIMEOUT', timeout, 0
    except FileNotFoundError:
        return 'NOT_INSTALLED', 0, 0
    except Exception as e:
        return f'ERROR: {e}', 0, 0

def main():
    print("="*80)
    print("Satience vs MiniSat Quick Benchmark")
    print("="*80)
    print()

    results = []

    for name, cnf_path, expected in TEST_INSTANCES:
        if not os.path.exists(cnf_path):
            print(f"Skipping {name}: file not found")
            continue

        print(f"\n{name} ({cnf_path})")
        print(f"Expected: {expected}")
        print("-"*60)

        # Satience
        sat_result, sat_time, sat_conflicts, sat_decisions = run_satience(cnf_path)
        print(f"Satience: {sat_result:8s} in {sat_time:8.4f}s ({sat_conflicts:6d} conflicts, {sat_decisions:6d} decisions)")

        # MiniSat
        min_result, min_time, min_conflicts = run_minisat(cnf_path)
        if min_result != 'NOT_INSTALLED':
            print(f"MiniSat:  {min_result:8s} in {min_time:8.4f}s ({min_conflicts:6d} conflicts)")

            # Calculate speedup
            if sat_time > 0 and min_time > 0 and sat_time < 60:
                speedup = min_time / sat_time
                if speedup > 1:
                    print(f"Speedup:  Satience is {speedup:.1f}x FASTER")
                else:
                    print(f"Speedup:  MiniSat is {1/speedup:.1f}x faster")

            # Check match
            if sat_result == min_result:
                print("Result:   ✓ MATCH")
            else:
                print(f"Result:   ✗ MISMATCH (Satience={sat_result}, MiniSat={min_result})")

        results.append({
            'name': name,
            'satience_result': sat_result,
            'satience_time': sat_time,
            'minisat_result': min_result,
            'minisat_time': min_time,
            'match': sat_result == min_result if min_result != 'NOT_INSTALLED' else None
        })

    # Summary
    print("\n" + "="*80)
    print("SUMMARY")
    print("="*80)

    total = len(results)
    matches = sum(1 for r in results if r['match'] is True)
    sat_timeouts = sum(1 for r in results if r['satience_result'] == 'TIMEOUT')
    min_timeouts = sum(1 for r in results if r['minisat_result'] == 'TIMEOUT')

    print(f"Total instances: {total}")
    print(f"Matches: {matches}/{total}")
    print(f"Satience timeouts: {sat_timeouts}")
    print(f"MiniSat timeouts: {min_timeouts}")

    # Calculate median speedup (excluding timeouts)
    speedups = []
    for r in results:
        if r['satience_time'] > 0 and r['minisat_time'] > 0:
            if r['satience_result'] != 'TIMEOUT' and r['minisat_result'] != 'TIMEOUT':
                speedups.append(r['minisat_time'] / r['satience_time'])

    if speedups:
        median_speedup = sorted(speedups)[len(speedups)//2]
        if median_speedup > 1:
            print(f"Median: Satience is {median_speedup:.2f}x FASTER than MiniSat")
        else:
            print(f"Median: MiniSat is {1/median_speedup:.2f}x faster than Satience")

if __name__ == '__main__':
    main()
