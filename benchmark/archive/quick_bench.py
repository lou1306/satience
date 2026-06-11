#!/usr/bin/env python3
"""Quick benchmark comparing Satience vs MiniSat on selected instances."""

import subprocess
import os
import sys
import time
from pathlib import Path
from collections import defaultdict

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
            # Look for SAT Competition 2026 format: "s SATISFIABLE", "s UNSATISFIABLE", "s UNKNOWN"
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
    print("="*90)
    print("SATIENCE vs MINISAT BENCHMARK")
    print("="*90)
    
    # Select specific instances for benchmarking
    test_instances = [
        # Small instances
        'algebra_xor_20_sat.cnf',
        'algebra_xor_30_unsat.cnf',
        'php_5p_6h_sat.cnf',
        'tseitin_grid_5x5_sat.cnf',
        'arg_chain_50_sat.cnf',
        
        # Medium instances  
        'arg_chain_100_sat.cnf',
        'tseitin_grid_7x7_sat.cnf',
        '0f4576a6e7399336e11f0828d32263dd.cnf',  # 200 vars
        '11c893b7c37aeb53cdaf5f677dda0b7d.cnf',  # 36 vars
        '18f54820956791d3028868b56a09c6cd.cnf',  # 50 vars
        
        # Large instances
        '02223564bd2f5c20768e63cf28c785e3.cnf',  # cardinality
        '166e1e5a9f63fcf94ddae8533fa2a090.cnf',  # cardinality
        'sudoku_3x3_empty_sat.cnf',
    ]
    
    results = []
    
    for inst_name in test_instances:
        filepath = INSTANCE_DIR / inst_name
        if not filepath.exists():
            print(f"Skipping {inst_name} (not found)")
            continue
        
        n_vars, n_clauses = get_instance_info(filepath)
        
        print(f"\n{'='*90}")
        print(f"Instance: {inst_name[:60]}")
        print(f"Vars: {n_vars}, Clauses: {n_clauses}")
        
        # Run Satience
        print("\nRunning Satience...")
        sat_result, sat_time, sat_conflicts, sat_decisions = run_solver(
            SATIENCE_BIN, filepath, TIMEOUT
        )
        print(f"  Satience: {sat_result:8s} Time: {sat_time:8.3f}s  Conflicts: {sat_conflicts:8d}  Decisions: {sat_decisions:8d}")
        
        # Run MiniSat
        print("Running MiniSat...")
        ms_result, ms_time, ms_conflicts, ms_decisions = run_solver(
            MINISAT_BIN, filepath, TIMEOUT
        )
        print(f"  MiniSat:  {ms_result:8s} Time: {ms_time:8.3f}s  Conflicts: {ms_conflicts:8d}  Decisions: {ms_decisions:8d}")
        
        # Calculate speedup
        if sat_time < TIMEOUT and ms_time < TIMEOUT and ms_time > 0 and ms_time < TIMEOUT:
            speedup = sat_time / ms_time
        else:
            speedup = None
        
        # Check result agreement
        result_match = (sat_result == ms_result) if sat_result not in ['TIMEOUT', 'UNKNOWN', 'ERROR'] else 'N/A'
        
        results.append({
            'name': inst_name,
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
            'speedup': speedup,
            'result_match': result_match
        })
        
        if speedup:
            if speedup < 1.0:
                print(f"  → Satience is {1/speedup:.2f}x FASTER")
            else:
                print(f"  → Satience is {speedup:.2f}x slower")
        else:
            print(f"  → Speedup: N/A (timeout)")
    
    # Summary
    print("\n" + "="*90)
    print("SUMMARY")
    print("="*90)
    
    print(f"\n{'Instance':<50} {'Vars':>6} {'Clauses':>8} {'Satience':>10} {'MiniSat':>10} {'Ratio':>8}")
    print(f"{'-'*50} {'-'*6} {'-'*8} {'-'*10} {'-'*10} {'-'*8}")
    
    for r in results:
        speedup_str = f"{r['speedup']:.2f}x" if r['speedup'] else "TIMEOUT"
        print(f"{r['name'][:50]:<50} {r['vars']:>6} {r['clauses']:>8} {r['sat_time']:>10.3f} {r['ms_time']:>10.3f} {speedup_str:>8}")
    
    # Overall statistics
    print("\n" + "="*90)
    print("OVERALL STATISTICS")
    print("="*90)
    
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
    print("\n" + "="*90)
    print("CONFLICT ANALYSIS (solved instances only)")
    print("="*90)
    
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
    
    # Write results to file
    output_file = Path('/home/luca/git/opencode-sat-new/benchmark/quick_bench_results.csv')
    with open(output_file, 'w') as f:
        f.write('name,vars,clauses,sat_result,sat_time,sat_conflicts,sat_decisions,ms_result,ms_time,ms_conflicts,ms_decisions,speedup\n')
        for r in results:
            speedup_str = f"{r['speedup']:.4f}" if r['speedup'] else ''
            f.write(f"{r['name']},{r['vars']},{r['clauses']},{r['sat_result']},{r['sat_time']:.4f},{r['sat_conflicts']},{r['sat_decisions']},{r['ms_result']},{r['ms_time']:.4f},{r['ms_conflicts']},{r['ms_decisions']},{speedup_str}\n")
    
    print(f"\nResults written to: {output_file}")

if __name__ == '__main__':
    main()
