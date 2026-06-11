#!/usr/bin/env python3
"""Comprehensive benchmark comparing Satience vs MiniSat on diverse instances."""

import subprocess
import os
import sys
import time
from pathlib import Path
from collections import defaultdict
import sqlite3

SATIENCE_BIN = '/home/luca/git/opencode-sat-new/satience'
MINISAT_BIN = '/home/luca/bin/minisat'
TIMEOUT = 60  # seconds
INSTANCE_DIR = Path('/home/luca/git/opencode-sat-new/benchmark/gbd_instances')
DB_PATH = Path('/home/luca/git/opencode-sat-new/benchmark/meta.db')

def get_instance_info(filepath):
    """Parse CNF file to get number of variables and clauses."""
    try:
        with open(filepath, 'r') as f:
            for line in f:
                if line.startswith('p cnf'):
                    parts = line.split()
                    if len(parts) >= 4:
                        n_vars = int(parts[2])
                        n_clauses = int(parts[3])
                        return n_vars, n_clauses
    except:
        pass
    return 0, 0

def get_family(filepath):
    """Get family from database."""
    hash_val = filepath.stem
    try:
        conn = sqlite3.connect(DB_PATH)
        c = conn.cursor()
        c.execute('SELECT family, result FROM features WHERE hash=?', (hash_val,))
        row = c.fetchone()
        conn.close()
        if row:
            return row[0], row[1]
    except:
        pass
    return 'unknown', 'unknown'

def run_solver(solver_cmd, instance_path, timeout):
    """Run a solver and return (result, time, conflicts, decisions)."""
    try:
        start = time.time()
        
        if 'minisat' in solver_cmd:
            result = subprocess.run(
                solver_cmd.split() + [str(instance_path)],
                capture_output=True,
                text=True,
                timeout=timeout
            )
            elapsed = time.time() - start
            
            output = result.stdout
            if 'SAT' in output:
                sat_result = 'SAT'
            elif 'UNSAT' in output:
                sat_result = 'UNSAT'
            else:
                sat_result = 'UNKNOWN'
            
            conflicts = 0
            decisions = 0
            for line in output.split('\n'):
                if 'conflicts' in line.lower():
                    try:
                        conflicts = int(line.split()[0])
                    except:
                        pass
                if 'decisions' in line.lower():
                    try:
                        decisions = int(line.split()[0])
                    except:
                        pass
            
            return sat_result, elapsed, conflicts, decisions
        else:
            result = subprocess.run(
                solver_cmd.split() + ['-verbose', str(instance_path)],
                capture_output=True,
                text=True,
                timeout=timeout
            )
            elapsed = time.time() - start
            
            output = result.stdout
            lines = output.split('\n')
            sat_result = 'UNKNOWN'
            for line in lines:
                line = line.strip()
                if line == 's SATISFIABLE':
                    sat_result = 'SAT'
                    break
                elif line == 's UNSATISFIABLE':
                    sat_result = 'UNSAT'
                    break
                elif line == 's UNKNOWN':
                    sat_result = 'UNKNOWN'
                    break
            
            conflicts = 0
            decisions = 0
            for line in lines:
                if 'Conflicts:' in line:
                    conflicts = int(line.split(':')[1].strip())
                if 'Decisions:' in line:
                    decisions = int(line.split(':')[1].strip())
            
            return sat_result, elapsed, conflicts, decisions
            
    except subprocess.TimeoutExpired:
        return 'TIMEOUT', timeout, 0, 0
    except Exception as e:
        return f'ERROR: {e}', 0, 0, 0

def main():
    print("="*100)
    print("COMPREHENSIVE SATIENCE vs MINISAT BENCHMARK - DIVERSE FAMILIES")
    print("="*100)
    
    # Select diverse instances across families and sizes
    test_instances = []
    
    # Small instances (<100 vars)
    test_instances.extend([
        'algebra_xor_20_sat.cnf',
        'algebra_xor_30_unsat.cnf',
        'php_5p_6h_sat.cnf',
        'arg_chain_50_sat.cnf',
        '11c893b7c37aeb53cdaf5f677dda0b7d.cnf',  # 36 vars
        '18f54820956791d3028868b56a09c6cd.cnf',  # 50 vars
    ])
    
    # Medium instances (100-500 vars)
    test_instances.extend([
        'arg_chain_100_sat.cnf',
        '0f4576a6e7399336e11f0828d32263dd.cnf',  # 200 vars
        '6302401993fc837599b84a91ccb25f7e.cnf',  # cryptography
        'be93311b275fe6cdac545e41f626eaa7.cnf',  # cryptography
        '4a669e0b7acaad990e5c5b0f8c7e4f5a.cnf',  # bitvector
        '77b103479539a71c0d9f8e6a5b4c3d2e.cnf',  # coloring
        '99e0a9b26bd19223f8e7d6c5b4a39281.cnf',  # quasigroup
        'd8225a58e2ca7c2f1e0d9c8b7a6f5e4d.cnf',  # antibandwidth
        '24075575c3dbd9e9f8e7d6c5b4a39281.cnf',  # diagnosis
        'b40a1e31a6232b5e8f7e6d5c4b3a2918.cnf',  # scheduling
    ])
    
    # Large instances (>500 vars)
    test_instances.extend([
        'sudoku_3x3_empty_sat.cnf',
        '02223564bd2f5c20768e63cf28c785e3.cnf',  # cardinality
        '166e1e5a9f63fcf94ddae8533fa2a090.cnf',  # cardinality
        '1a3320d3cf32f211b3e7b875745713e2.cnf',  # planning
        '28792301119a25fb8edec6cde91eee21.cnf',  # planning
        '540e9882c0516c60705b8d17237257a0.cnf',  # subgraph-isomorphism
        '8175dacde2d6517a91c1912847a34006.cnf',  # hardware-verification
    ])
    
    results = []
    family_stats = defaultdict(lambda: {'satience_time': 0, 'minisat_time': 0, 'count': 0})
    
    for inst_name in test_instances:
        filepath = INSTANCE_DIR / inst_name
        if not filepath.exists():
            print(f"Skipping {inst_name} (not found)")
            continue
        
        n_vars, n_clauses = get_instance_info(filepath)
        family, expected_result = get_family(filepath)
        
        print(f"\n{'='*100}")
        print(f"Instance: {inst_name[:70]}")
        print(f"Family: {family}, Expected: {expected_result}")
        print(f"Vars: {n_vars}, Clauses: {n_clauses}")
        
        # Run Satience
        print("\nRunning Satience...")
        sat_result, sat_time, sat_conflicts, sat_decisions = run_solver(
            SATIENCE_BIN, filepath, TIMEOUT
        )
        print(f"  Satience: {sat_result:8s} Time: {sat_time:8.3f}s  Conflicts: {sat_conflicts:10d}  Decisions: {sat_decisions:10d}")
        
        # Run MiniSat
        print("Running MiniSat...")
        ms_result, ms_time, ms_conflicts, ms_decisions = run_solver(
            MINISAT_BIN, filepath, TIMEOUT
        )
        print(f"  MiniSat:  {ms_result:8s} Time: {ms_time:8.3f}s  Conflicts: {ms_conflicts:10d}  Decisions: {ms_decisions:10d}")
        
        # Calculate speedup
        if sat_time < TIMEOUT and ms_time < TIMEOUT and ms_time > 0:
            speedup = sat_time / ms_time
        else:
            speedup = None
        
        results.append({
            'name': inst_name,
            'family': family,
            'vars': n_vars,
            'clauses': n_clauses,
            'sat_result': sat_result,
            'sat_time': sat_time,
            'sat_conflicts': sat_conflicts,
            'sat_decisions': sat_decisions,
            'ms_result': ms_result,
            'ms_time': ms_time,
            'ms_conflicts': ms_conflicts,
            'ms_decisions': ms_decisions,
            'speedup': speedup
        })
        
        if speedup:
            if speedup < 1.0:
                print(f"  → Satience is {1/speedup:.2f}x FASTER")
            else:
                print(f"  → Satience is {speedup:.2f}x slower")
        else:
            print(f"  → Speedup: N/A (timeout)")
        
        # Accumulate family stats
        if family and sat_time < TIMEOUT and ms_time < TIMEOUT:
            family_stats[family]['satience_time'] += sat_time
            family_stats[family]['minisat_time'] += ms_time
            family_stats[family]['count'] += 1
    
    # Summary
    print("\n" + "="*100)
    print("SUMMARY BY INSTANCE")
    print("="*100)
    
    print(f"\n{'Instance':<60} {'Vars':>6} {'Satience':>10} {'MiniSat':>10} {'Ratio':>8} {'Family'}")
    print(f"{'-'*60} {'-'*6} {'-'*10} {'-'*10} {'-'*8} {'-'*30}")
    
    for r in results:
        speedup_str = f"{r['speedup']:.2f}x" if r['speedup'] else "TIMEOUT"
        family_str = r['family'][:28] if r['family'] else 'unknown'
        print(f"{r['name'][:58]:<60} {r['vars']:>6} {r['sat_time']:>10.3f} {r['ms_time']:>10.3f} {speedup_str:>8} {family_str}")
    
    # Overall statistics
    print("\n" + "="*100)
    print("OVERALL STATISTICS")
    print("="*100)
    
    valid_speedups = [r['speedup'] for r in results if r['speedup'] is not None]
    solved_satience = sum(1 for r in results if r['sat_result'] not in ['TIMEOUT', 'UNKNOWN', 'ERROR'])
    solved_minisat = sum(1 for r in results if r['ms_result'] not in ['TIMEOUT', 'UNKNOWN', 'ERROR'])
    
    print(f"Total instances: {len(results)}")
    print(f"Satience solved: {solved_satience}/{len(results)} ({100*solved_satience/len(results):.1f}%)")
    print(f"MiniSat solved:  {solved_minisat}/{len(results)} ({100*solved_minisat/len(results):.1f}%)")
    
    if valid_speedups:
        median_speedup = sorted(valid_speedups)[len(valid_speedups)//2]
        avg_speedup = sum(valid_speedups) / len(valid_speedups)
        print(f"\nMedian slowdown: {median_speedup:.2f}x")
        print(f"Average slowdown: {avg_speedup:.2f}x")
        print(f"Satience faster: {sum(1 for s in valid_speedups if s < 1.0)} instances")
        print(f"MiniSat faster:  {sum(1 for s in valid_speedups if s >= 1.0)} instances")
    
    # Family breakdown
    print("\n" + "="*100)
    print("PERFORMANCE BY FAMILY")
    print("="*100)
    
    print(f"\n{'Family':<30} {'Instances':>10} {'Satience (s)':>14} {'MiniSat (s)':>12} {'Ratio':>8}")
    print(f"{'-'*30} {'-'*10} {'-'*14} {'-'*12} {'-'*8}")
    
    for family, stats in sorted(family_stats.items(), key=lambda x: -x[1]['count']):
        if stats['count'] > 0:
            ratio = stats['satience_time'] / max(0.001, stats['minisat_time'])
            print(f"{family:<30} {stats['count']:>10} {stats['satience_time']:>14.3f} {stats['minisat_time']:>12.3f} {ratio:>8.2f}x")
    
    # Conflict analysis
    print("\n" + "="*100)
    print("CONFLICT ANALYSIS (solved by both)")
    print("="*100)
    
    solved_both = [r for r in results if r['sat_result'] == r['ms_result'] and r['sat_result'] not in ['TIMEOUT', 'UNKNOWN']]
    if solved_both:
        total_sat_conflicts = sum(r['sat_conflicts'] for r in solved_both)
        total_ms_conflicts = sum(r['ms_conflicts'] for r in solved_both)
        total_sat_decisions = sum(r['sat_decisions'] for r in solved_both)
        total_ms_decisions = sum(r['ms_decisions'] for r in solved_both)
        
        print(f"Total conflicts - Satience: {total_sat_conflicts:,}, MiniSat: {total_ms_conflicts:,}")
        print(f"Conflict ratio: {total_sat_conflicts/max(1,total_ms_conflicts):.2f}x")
        print(f"Total decisions - Satience: {total_sat_decisions:,}, MiniSat: {total_ms_decisions:,}")
        print(f"Decision ratio: {total_sat_decisions/max(1,total_ms_decisions):.2f}x")
    
    # Size analysis
    print("\n" + "="*100)
    print("PERFORMANCE BY SIZE")
    print("="*100)
    
    size_buckets = [
        (0, 100, '0-100 vars'),
        (100, 500, '100-500 vars'),
        (500, 2000, '500-2000 vars'),
        (2000, 10000, '2000-10000 vars'),
    ]
    
    for min_v, max_v, label in size_buckets:
        bucket = [r for r in results if min_v <= r['vars'] < max_v]
        if bucket:
            valid = [r['speedup'] for r in bucket if r['speedup']]
            if valid:
                median = sorted(valid)[len(valid)//2]
                avg = sum(valid) / len(valid)
                print(f"{label:15s}: median={median:.2f}x, avg={avg:.2f}x, count={len(valid)}")
    
    # Write results
    output_file = Path('/home/luca/git/opencode-sat-new/benchmark/comprehensive_bench_results.csv')
    with open(output_file, 'w') as f:
        f.write('name,family,vars,clauses,sat_result,sat_time,sat_conflicts,sat_decisions,ms_result,ms_time,ms_conflicts,ms_decisions,speedup\n')
        for r in results:
            speedup_str = f"{r['speedup']:.4f}" if r['speedup'] else ''
            f.write(f"{r['name']},{r['family']},{r['vars']},{r['clauses']},{r['sat_result']},{r['sat_time']:.4f},{r['sat_conflicts']},{r['sat_decisions']},{r['ms_result']},{r['ms_time']:.4f},{r['ms_conflicts']},{r['ms_decisions']},{speedup_str}\n")
    
    print(f"\nResults written to: {output_file}")

if __name__ == '__main__':
    main()
