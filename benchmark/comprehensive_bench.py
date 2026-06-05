#!/usr/bin/env python3
"""Comprehensive benchmark comparing Satience vs MiniSat across diverse instances."""

import subprocess
import os
import sys
import time
from pathlib import Path
from collections import defaultdict

SATIENCE_BIN = './satience'
MINISAT_BIN = '/home/luca/bin/minisat'
TIMEOUT = 60  # seconds
INSTANCE_DIR = Path('gbd_instances')

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
            if lines and 'SAT' in lines[0]:
                sat_result = 'SAT'
            elif lines and 'UNSAT' in lines[0]:
                sat_result = 'UNSAT'
            else:
                sat_result = 'UNKNOWN'
            
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

def categorize_instance(n_vars, n_clauses):
    """Categorize instance by size and density."""
    if n_vars < 100:
        size_cat = 'small'
    elif n_vars < 300:
        size_cat = 'medium'
    elif n_vars < 1000:
        size_cat = 'large'
    else:
        size_cat = 'xlarge'
    
    density = n_clauses / max(1, n_vars)
    if density < 3:
        density_cat = 'sparse'
    elif density < 10:
        density_cat = 'medium'
    else:
        density_cat = 'dense'
    
    return f"{size_cat}_{density_cat}"

def main():
    print("="*90)
    print("SATIENCE vs MINISAT COMPREHENSIVE BENCHMARK")
    print("="*90)
    
    # Collect all instances
    all_instances = []
    for filepath in INSTANCE_DIR.glob('*.cnf'):
        if not filepath.name.startswith('.'):
            n_vars, n_clauses = get_instance_info(filepath)
            if n_vars > 0:
                category = categorize_instance(n_vars, n_clauses)
                all_instances.append((filepath, category, n_vars, n_clauses))
    
    print(f"\nFound {len(all_instances)} instances")
    
    # Select diverse subset (max 3 per category to keep benchmark manageable)
    by_category = defaultdict(list)
    for inst in all_instances:
        by_category[inst[1]].append(inst)
    
    selected = []
    for cat, instances in sorted(by_category.items()):
        # Sort by vars and pick diverse sizes
        instances.sort(key=lambda x: x[2])
        n_select = min(3, len(instances))
        step = max(1, len(instances) // n_select)
        for i in range(0, len(instances), step)[:n_select]:
            selected.append(instances[i])
    
    print(f"Selected {len(selected)} instances for benchmarking")
    
    results = []
    
    for filepath, category, n_vars, n_clauses in selected:
        print(f"\n{'='*90}")
        print(f"Instance: {filepath.name[:50]}")
        print(f"Category: {category}, Vars: {n_vars}, Clauses: {n_clauses}")
        
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
        result_match = (sat_result == ms_result) if sat_result not in ['TIMEOUT', 'ERROR'] else 'N/A'
        
        results.append({
            'name': filepath.name,
            'category': category,
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
    print("SUMMARY BY CATEGORY")
    print("="*90)
    
    # Group by category
    by_category = defaultdict(list)
    for r in results:
        by_category[r['category']].append(r)
    
    for category, cat_results in sorted(by_category.items()):
        print(f"\n{category.upper()} ({len(cat_results)} instances):")
        print(f"  {'Instance':<45} {'Vars':>6} {'Clauses':>8} {'Satience':>10} {'MiniSat':>10} {'Ratio':>8}")
        print(f"  {'-'*45} {'-'*6} {'-'*8} {'-'*10} {'-'*10} {'-'*8}")
        
        for r in cat_results:
            speedup_str = f"{r['speedup']:.2f}x" if r['speedup'] else "TIMEOUT"
            print(f"  {r['name'][:45]:<45} {r['vars']:>6} {r['clauses']:>8} {r['sat_time']:>10.3f} {r['ms_time']:>10.3f} {speedup_str:>8}")
    
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
    output_file = Path('benchmark_results.csv')
    with open(output_file, 'w') as f:
        f.write('name,category,vars,clauses,sat_result,sat_time,sat_conflicts,sat_decisions,ms_result,ms_time,ms_conflicts,ms_decisions,speedup\n')
        for r in results:
            speedup_str = f"{r['speedup']:.4f}" if r['speedup'] else ''
            f.write(f"{r['name']},{r['category']},{r['vars']},{r['clauses']},{r['sat_result']},{r['sat_time']:.4f},{r['sat_conflicts']},{r['sat_decisions']},{r['ms_result']},{r['ms_time']:.4f},{r['ms_conflicts']},{r['ms_decisions']},{speedup_str}\n")
    
    print(f"\nResults written to: {output_file}")

if __name__ == '__main__':
    main()
