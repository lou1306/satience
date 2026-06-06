#!/usr/bin/env python3
"""
Quick benchmark on small GBD instances
"""

import subprocess
import time
import os

# Small test instances (filename, expected_result)
TEST_INSTANCES = [
    # Very small (< 50 vars)
    ('algebra_xor_20_sat.cnf', 'SAT'),
    ('algebra_xor_30_sat.cnf', 'SAT'),
    ('php_5p_6h_sat.cnf', 'SAT'),
    ('php_6p_5h_unsat.cnf', 'UNSAT'),
    ('11c893b7c37aeb53cdaf5f677dda0b7d.cnf', 'UNSAT'),  # tseitin 36v
    ('tseitin_grid_4x4_unsat.cnf', 'UNSAT'),
    ('algebra_xor_40_sat.cnf', 'SAT'),
    ('874bdedb23926bd0b0f2a56f7c0059d6.cnf', 'UNSAT'),  # 42v
    ('tseitin_grid_5x5_sat.cnf', 'SAT'),
    ('18f54820956791d3028868b56a09c6cd.cnf', 'UNSAT'),  # equivalence-rich 50v
    ('arg_chain_50_sat.cnf', 'SAT'),
    ('random_k3_50v_200c_sat.cnf', 'SAT'),
]

def run_satience(cnf_path, timeout=60):
    try:
        start = time.time()
        result = subprocess.run(
            ['./satience', cnf_path],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start

        if result.returncode == 10:
            return 'SAT', elapsed
        elif result.returncode == 20:
            return 'UNSAT', elapsed
        else:
            return 'TIMEOUT', elapsed
    except subprocess.TimeoutExpired:
        return 'TIMEOUT', timeout
    except Exception as e:
        return f'ERROR', 0

def run_minisat(cnf_path, timeout=60):
    try:
        start = time.time()
        result = subprocess.run(
            ['minisat', cnf_path, '/tmp/minisat_out.txt'],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start

        if result.returncode == 10:
            return 'SAT', elapsed
        elif result.returncode == 20:
            return 'UNSAT', elapsed
        else:
            return 'TIMEOUT', elapsed
    except subprocess.TimeoutExpired:
        return 'TIMEOUT', timeout
    except FileNotFoundError:
        return 'NOT_INSTALLED', 0
    except Exception as e:
        return f'ERROR', 0

def main():
    print("="*70)
    print("Satience vs MiniSat - Small Instances Benchmark")
    print("="*70)
    print()

    results = []

    for filename, expected in TEST_INSTANCES:
        cnf_path = f'benchmark/gbd_instances/{filename}'
        if not os.path.exists(cnf_path):
            print(f"SKIP: {filename} (not found)")
            continue

        # Get instance info
        vars_count, clauses_count = 0, 0
        with open(cnf_path, 'r') as f:
            for line in f:
                if line.startswith('p cnf'):
                    parts = line.split()
                    vars_count = int(parts[2])
                    clauses_count = int(parts[3])
                    break

        print(f"{filename[:35]:35s} ({vars_count:3d}v/{clauses_count:4d}c) Expected: {expected:5s}  ", end='')

        sat_result, sat_time = run_satience(cnf_path)
        min_result, min_time = run_minisat(cnf_path)

        if min_result == 'NOT_INSTALLED':
            print(f"Satience: {sat_result:8s} {sat_time:8.4f}s")
            match = None
            speedup = None
        else:
            if sat_time > 0.001 and min_time > 0.001:
                speedup = min_time / sat_time
            else:
                speedup = None

            match = (sat_result == min_result)
            match_str = "✓" if match else "✗"

            if speedup and speedup > 0:
                if speedup > 1:
                    print(f"Sat: {sat_time:7.4f}s  Min: {min_time:7.4f}s  [{speedup:6.1f}x faster] {match_str}")
                else:
                    print(f"Sat: {sat_time:7.4f}s  Min: {min_time:7.4f}s  [{1/speedup:6.1f}x slower] {match_str}")
            else:
                print(f"Sat: {sat_time:7.4f}s  Min: {min_time:7.4f}s  {match_str}")

        results.append({
            'name': filename,
            'vars': vars_count,
            'clauses': clauses_count,
            'satience_result': sat_result,
            'satience_time': sat_time,
            'minisat_result': min_result,
            'minisat_time': min_time,
            'match': match,
            'speedup': speedup
        })

    # Summary
    print()
    print("="*70)
    print("SUMMARY")
    print("="*70)

    total = len(results)
    matches = sum(1 for r in results if r['match'] is True)
    mismatches = sum(1 for r in results if r['match'] is False)
    sat_timeouts = sum(1 for r in results if r['satience_result'] == 'TIMEOUT')
    min_timeouts = sum(1 for r in results if r['minisat_result'] == 'TIMEOUT')

    print(f"Total instances: {total}")
    print(f"Matches: {matches}/{total}")
    if mismatches > 0:
        print(f"Mismatches: {mismatches}")
    print(f"Satience timeouts: {sat_timeouts}")
    print(f"MiniSat timeouts: {min_timeouts}")

    # Calculate median speedup
    speedups = [r['speedup'] for r in results if r['speedup'] is not None and r['speedup'] > 0]
    if speedups:
        median_speedup = sorted(speedups)[len(speedups)//2]
        if median_speedup > 1:
            print(f"\nMedian speedup: Satience is {median_speedup:.2f}x FASTER than MiniSat")
        else:
            print(f"\nMedian speedup: MiniSat is {1/median_speedup:.2f}x faster than Satience")

if __name__ == '__main__':
    main()
