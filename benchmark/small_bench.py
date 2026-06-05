#!/usr/bin/env python3
"""Quick benchmark on SMALL instances only (vars < 300)."""

import subprocess
import os
import sys
import time
from pathlib import Path

SATIENCE_BIN = '/home/luca/git/opencode-sat-new/satience'
MINISAT_BIN = '/home/luca/bin/minisat'
TIMEOUT = 60  # seconds
INSTANCE_DIR = Path('/home/luca/git/opencode-sat-new/benchmark/gbd_instances')

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
    print("SATIENCE vs MINISAT BENCHMARK - SMALL INSTANCES (< 300 vars)")
    print("="*100)
    
    # Select small instances only
    test_instances = [
        # Very small (<50 vars)
        'algebra_xor_20_sat.cnf',
        'algebra_xor_30_unsat.cnf',
        'php_5p_6h_sat.cnf',
        '11c893b7c37aeb53cdaf5f677dda0b7d.cnf',  # 36 vars
        '18f54820956791d3028868b56a09c6cd.cnf',  # 50 vars
        
        # Small (50-150 vars)
        'arg_chain_50_sat.cnf',
        'arg_chain_100_sat.cnf',
        '0f4576a6e7399336e11f0828d32263dd.cnf',  # 200 vars
        
        # Medium (150-300 vars) - new families
        '6302401993fc837599b84a91ccb25f7e.cnf',  # cryptography
        'be93311b275fe6cdac545e41f626eaa7.cnf',  # cryptography
        'e2cc6f2ab367ffc3005b74e65c6b695a.cnf',  # cryptography
        '486b5f3133c95d04ca361e027d99e0d6.cnf',  # cryptography
        'f88106a9f3ec04f7221fd8511c13a1f1.cnf',  # cryptography
        
        '540e9882c0516c60705b8d17237257a0.cnf',  # subgraph-isomorphism
        'e47586f86868e83a5ebe7a3ecae204f1.cnf',  # subgraph-isomorphism
        'bb844a5223dc8aba071e71f152cd43f9.cnf',  # subgraph-isomorphism
        
        '4a669e0b7acaad990e5c5b0f8c7e4f5a.cnf',  # bitvector
        '2285ff99f985f8f90e5c5b0f8c7e4f5a.cnf',  # bitvector
        
        '77b103479539a71c0d9f8e6a5b4c3d2e.cnf',  # coloring
        '69d72f81a176c4770d9f8e6a5b4c3d2e.cnf',  # coloring
        
        '99e0a9b26bd19223f8e7d6c5b4a39281.cnf',  # quasigroup
        'f039f6c5b036c938f8e7d6c5b4a39281.cnf',  # quasigroup
        
        'd8225a58e2ca7c2f1e0d9c8b7a6f5e4d.cnf',  # antibandwidth
        '7d6bb9108cbc28d51e0d9c8b7a6f5e4d.cnf',  # antibandwidth
        
        '24075575c3dbd9e9f8e7d6c5b4a39281.cnf',  # diagnosis
        '9bd299393602718af8e7d6c5b4a39281.cnf',  # diagnosis
        
        'b40a1e31a6232b5e8f7e6d5c4b3a2918.cnf',  # scheduling
        '262ba88b7b11338a8f7e6d5c4b3a2918.cnf',  # scheduling
    ]
    
    results = []
    
    for inst_name in test_instances:
        filepath = INSTANCE_DIR / inst_name
        if not filepath.exists():
            print(f"Skipping {inst_name} (not found)")
            continue
        
        n_vars, n_clauses = get_instance_info(filepath)
        
        # Skip if too large
        if n_vars > 300:
            print(f"Skipping {inst_name} ({n_vars} vars > 300)")
            continue
        
        print(f"\n{'='*100}")
        print(f"Instance: {inst_name[:70]} ({n_vars} vars, {n_clauses} clauses)")
        
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
    
    print(f"\n{'Instance':<55} {'Vars':>5} {'Satience':>9} {'MiniSat':>9} {'Ratio':>7}")
    print(f"{'-'*55} {'-'*5} {'-'*9} {'-'*9} {'-'*7}")
    
    for r in results:
        speedup_str = f"{r['speedup']:.2f}x" if r['speedup'] else "TIMEOUT"
        print(f"{r['name'][:53]:<55} {r['vars']:>5} {r['sat_time']:>9.3f} {r['ms_time']:>9.3f} {speedup_str:>7}")
    
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
    output_file = Path('/home/luca/git/opencode-sat-new/benchmark/small_bench_results.csv')
    with open(output_file, 'w') as f:
        f.write('name,vars,clauses,sat_result,sat_time,sat_conflicts,ms_result,ms_time,ms_conflicts,speedup\n')
        for r in results:
            speedup_str = f"{r['speedup']:.4f}" if r['speedup'] else ''
            f.write(f"{r['name']},{r['vars']},{r['clauses']},{r['sat_result']},{r['sat_time']:.4f},{r['sat_conflicts']},{r['ms_result']},{r['ms_time']:.4f},{r['ms_conflicts']},{speedup_str}\n")
    
    print(f"\nResults written to: {output_file}")

if __name__ == '__main__':
    main()
