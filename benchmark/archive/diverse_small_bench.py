#!/usr/bin/env python3
"""Benchmark on diverse small instances from new families."""

import subprocess
import os
import sys
import time
from pathlib import Path

SATIENCE_BIN = '/home/luca/git/opencode-sat-new/satience'
MINISAT_BIN = '/home/luca/bin/minisat'
TIMEOUT = 60  # seconds
INSTANCE_DIR = Path('/home/luca/git/opencode-sat-new/benchmark/gbd_instances')

# Manually curated list of small instances from diverse families
test_instances = [
    # Existing tested instances
    ('algebra_xor_20_sat.cnf', 'algebra'),
    ('php_5p_6h_sat.cnf', 'pigeonhole'),
    ('11c893b7c37aeb53cdaf5f677dda0b7d.cnf', 'unknown'),
    ('18f54820956791d3028868b56a09c6cd.cnf', 'unknown'),
    ('arg_chain_50_sat.cnf', 'argchain'),
    ('arg_chain_100_sat.cnf', 'argchain'),
    ('0f4576a6e7399336e11f0828d32263dd.cnf', 'random'),
    
    # New families - small instances
    ('874bdedb23926bd0df8ef574e981fd2f.cnf', 'unknown'),  # 42 vars
    ('9a8546564631081cdfd3ead12990c266.cnf', 'unknown'),  # 44 vars
    ('cf4c9fdf0c55163b64cd7ddd69927cd1.cnf', 'unknown'),  # 50 vars
    ('274099073ca1be8ecc4123e63d24465a.cnf', 'diagnosis'),  # 80 vars
    ('be532f8c735071a37497fe02c436935d.cnf', 'unknown'),  # 80 vars
    ('91d6a078176e451b3abac9699638228e.cnf', 'unknown'),  # 89 vars
    ('8324f2fc969e2cf4ba541988fa985a5b.cnf', 'unknown'),  # 90 vars
    ('44092fcc83a5cba81419e82cfd18602c.cnf', 'unknown'),  # 90 vars
    ('7fa52f87c4556ea449f68b3369e82c24.cnf', 'unknown'),  # 100 vars
    ('961811bc85fb3ad199c351be6c5b800e.cnf', 'unknown'),  # 120 vars
    ('69d72f81a176c477372d8796c942875f.cnf', 'coloring'),  # 132 vars
    ('e273e0bf8ff1fa2a797b41f9a5f4beb5.cnf', 'unknown'),  # 141 vars
    ('11d97071b666335ff8d4e1aee76dd70a.cnf', 'coloring'),  # 175 vars
    ('85beed7b99ed71ed5e5b20235e490216.cnf', 'coloring'),  # 175 vars
    ('3d93794951995e1f307501ca932a8695.cnf', 'unknown'),  # 200 vars
    ('a45b60e53917968f922b97c6f8aa8db3.cnf', 'coloring'),  # 205 vars
    ('5dba8c37f6cf9110e29fabfd54eb9bba.cnf', 'unknown'),  # 208 vars
    ('96f850eb0ffc1a68322226f2958970fa.cnf', 'unknown'),  # 240 vars
    ('d4acb8ca0d73ed82d6ff7f11fcccfa0c.cnf', 'antibandwidth'),  # 246 vars
    ('46b70d0c89444f913e8c060c6839f8ee.cnf', 'unknown'),  # 250 vars
    ('3fd4d6a0c7efa6f547b3925cc3199ce5.cnf', 'unknown'),  # 250 vars
    ('30eb4ef44ad330ee289ccfb97bd7f4bd.cnf', 'unknown'),  # 300 vars
    ('566f366c824bf01a9ab4b54e9d06cbfa.cnf', 'unknown'),  # 300 vars
]

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
    print("SATIENCE vs MINISAT BENCHMARK - DIVERSE SMALL INSTANCES")
    print("="*100)
    
    results = []
    
    for inst_name, family in test_instances:
        filepath = INSTANCE_DIR / inst_name
        if not filepath.exists():
            print(f"Skipping {inst_name} (not found)")
            continue
        
        n_vars, n_clauses = get_instance_info(filepath)
        
        print(f"\n{'='*100}")
        print(f"Instance: {inst_name[:70]} ({n_vars} vars, {n_clauses} clauses)")
        print(f"Family: {family}")
        
        # Run Satience
        print("Running Satience...", end=' ', flush=True)
        sat_result, sat_time, sat_conflicts, sat_decisions = run_solver(
            SATIENCE_BIN, filepath, TIMEOUT
        )
        print(f"{sat_result:8s} Time: {sat_time:8.3f}s  Conflicts: {sat_conflicts:10d}")
        
        # Run MiniSat
        print("Running MiniSat...", end=' ', flush=True)
        ms_result, ms_time, ms_conflicts, ms_decisions = run_solver(
            MINISAT_BIN, filepath, TIMEOUT
        )
        print(f"{ms_result:8s} Time: {ms_time:8.3f}s  Conflicts: {ms_conflicts:10d}")
        
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
            'ms_result': ms_result,
            'ms_time': ms_time,
            'ms_conflicts': ms_conflicts,
            'speedup': speedup
        })
        
        if speedup:
            if speedup < 1.0:
                print(f"  → Satience is {1/speedup:.2f}x FASTER")
            else:
                print(f"  → Satience is {speedup:.2f}x slower")
    
    # Summary
    print("\n" + "="*100)
    print("SUMMARY")
    print("="*100)
    
    print(f"\n{'Instance':<50} {'Vars':>5} {'Family':<15} {'Satience':>8} {'MiniSat':>8} {'Ratio':>7}")
    print(f"{'-'*50} {'-'*5} {'-'*15} {'-'*8} {'-'*8} {'-'*7}")
    
    for r in results:
        speedup_str = f"{r['speedup']:.2f}x" if r['speedup'] else "TIMEOUT"
        family_str = r['family'][:13] if r['family'] else 'unknown'
        print(f"{r['name'][:48]:<50} {r['vars']:>5} {family_str:<15} {r['sat_time']:>8.3f} {r['ms_time']:>8.3f} {speedup_str:>7}")
    
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
    
    # Conflict analysis
    print("\n" + "="*100)
    print("CONFLICT ANALYSIS (solved by both)")
    print("="*100)
    
    solved_both = [r for r in results if r['sat_result'] == r['ms_result'] and r['sat_result'] not in ['TIMEOUT', 'UNKNOWN']]
    if solved_both:
        total_sat_conflicts = sum(r['sat_conflicts'] for r in solved_both)
        total_ms_conflicts = sum(r['ms_conflicts'] for r in solved_both)
        
        print(f"Total conflicts - Satience: {total_sat_conflicts:,}, MiniSat: {total_ms_conflicts:,}")
        print(f"Conflict ratio: {total_sat_conflicts/max(1,total_ms_conflicts):.2f}x")
    
    # Write results
    output_file = Path('/home/luca/git/opencode-sat-new/benchmark/diverse_small_bench_results.csv')
    with open(output_file, 'w') as f:
        f.write('name,family,vars,clauses,sat_result,sat_time,sat_conflicts,ms_result,ms_time,ms_conflicts,speedup\n')
        for r in results:
            speedup_str = f"{r['speedup']:.4f}" if r['speedup'] else ''
            f.write(f"{r['name']},{r['family']},{r['vars']},{r['clauses']},{r['sat_result']},{r['sat_time']:.4f},{r['sat_conflicts']},{r['ms_result']},{r['ms_time']:.4f},{r['ms_conflicts']},{speedup_str}\n")
    
    print(f"\nResults written to: {output_file}")

if __name__ == '__main__':
    main()
