#!/usr/bin/env python3
"""Quick comparison of satience vs MiniSat.

Note: Runs instances sequentially (max 1 solver instance at a time).
Do not add parallel execution - limit is 8 concurrent solver instances.
"""

import subprocess
import time
from pathlib import Path

TIMEOUT = 30
SATIENCE = "./satience"
INSTANCES = [
    # Algebra XOR (easy, should be fast)
    "algebra_xor_20_sat.cnf",
    "algebra_xor_30_sat.cnf",
    "algebra_xor_40_sat.cnf",
    
    # Arg chain (structured, medium)
    "arg_chain_50_sat.cnf",
    "arg_chain_100_sat.cnf",
    "arg_chain_150_sat.cnf",
    
    # Random k3 (various densities)
    "random_k3_50v_200c_sat.cnf",
    "random_k3_75v_300c_sat.cnf",
    "random_k3_100v_400c_sat.cnf",
    
    # Tseitin (structured, grid-like)
    "tseitin_grid_4x4_unsat.cnf",
    "tseitin_grid_5x5_sat.cnf",
    "tseitin_grid_5x5_unsat.cnf",
    "tseitin_grid_6x6_sat.cnf",
    "tseitin_grid_6x6_unsat.cnf",
    "tseitin_grid_7x7_sat.cnf",
    
    # Pigeonhole (notoriously hard for CDCL)
    "php_5p_6h_sat.cnf",
    "php_6p_5h_unsat.cnf",
    "php_6p_7h_sat.cnf",
    "php_7p_6h_unsat.cnf",
    
    # Sudoku (real-world structured)
    "sudoku_3x3_empty_sat.cnf",
]

def run_solver(solver, cnf_path, timeout=TIMEOUT):
    """Run solver and return (result, time)."""
    try:
        start = time.time()
        result = subprocess.run(
            f"{solver} {cnf_path}",
            shell=True,
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start
        output = result.stdout + result.stderr
        
        if "SAT" in output and "UNSAT" not in output:
            return "SAT", elapsed
        elif "UNSAT" in output:
            return "UNSAT", elapsed
        else:
            return "UNKNOWN", elapsed
    except subprocess.TimeoutExpired:
        return "TIMEOUT", timeout
    except Exception as e:
        return "ERROR", None

def get_stats(cnf_path):
    """Get vars and clauses from CNF."""
    with open(cnf_path, 'r') as f:
        for line in f:
            if line.startswith('p cnf'):
                parts = line.split()
                return int(parts[2]), int(parts[3])
    return 0, 0

def main():
    benchmark_dir = Path("benchmark/gbd_instances")
    
    print("=" * 100)
    print("SATIENCE vs MiniSat Benchmark")
    print("=" * 100)
    print(f"{'Instance':<35} {'Vars':>6} {'Cls':>6} {'Res':>6} {'Satience':>9} {'MiniSat':>9} {'Ratio':>7}")
    print("-" * 100)
    
    results = []
    
    for inst_name in INSTANCES:
        cnf_path = benchmark_dir / inst_name
        if not cnf_path.exists():
            continue
        
        vars_count, clauses = get_stats(cnf_path)
        
        # Run satience
        sat_result, sat_time = run_solver(SATIENCE, cnf_path)
        
        # Run MiniSat
        mini_result, mini_time = run_solver("minisat", cnf_path)
        
        # Calculate ratio
        ratio_val = None
        if sat_time and mini_time and mini_time > 0:
            ratio_val = sat_time / mini_time
            ratio_str = f"{ratio_val:.2f}x"
        else:
            ratio_str = "N/A"
        
        # Check match
        if sat_result == mini_result:
            result_str = sat_result
        else:
            result_str = f"MATCH?"
        
        print(f"{inst_name:<35} {vars_count:>6} {clauses:>6} {result_str:>6} {sat_time:>9.3f}s {mini_time:>9.3f}s {ratio_str:>7}")
        
        results.append({
            'name': inst_name,
            'vars': vars_count,
            'clauses': clauses,
            'sat_result': sat_result,
            'sat_time': sat_time,
            'mini_result': mini_result,
            'mini_time': mini_time,
            'ratio': ratio_val
        })
    
    print("=" * 100)
    
    # Summary
    total = len(results)
    solved = sum(1 for r in results if r['sat_result'] != 'TIMEOUT')
    correct = sum(1 for r in results if r['sat_result'] == r['mini_result'] and r['sat_result'] != 'TIMEOUT')
    
    ratios = [r['ratio'] for r in results if r['ratio'] is not None]
    if ratios:
        avg_ratio = sum(ratios) / len(ratios)
        median_ratio = sorted(ratios)[len(ratios)//2]
        min_ratio = min(ratios)
        max_ratio = max(ratios)
    else:
        avg_ratio = median_ratio = min_ratio = max_ratio = None
    
    print(f"\nSUMMARY:")
    print(f"  Total: {total}, Solved: {solved}, Correct: {correct}")
    if avg_ratio:
        print(f"  Avg ratio: {avg_ratio:.2f}x, Median: {median_ratio:.2f}x, Range: {min_ratio:.2f}x - {max_ratio:.2f}x")
        
        if avg_ratio < 1.0:
            print(f"  ✅ Satience is FASTER on average!")
        elif avg_ratio < 2.0:
            print(f"  👍 Competitive (within 2x)")
        elif avg_ratio < 5.0:
            print(f"  ⚠️  {avg_ratio:.1f}x slower - room for improvement")
        else:
            print(f"  ❌ {avg_ratio:.1f}x slower - needs optimization")
    
    # By category
    print(f"\nBY CATEGORY:")
    categories = {
        'algebra': [],
        'arg_chain': [],
        'random': [],
        'tseitin': [],
        'php': [],
        'sudoku': []
    }
    
    for r in results:
        for cat in categories:
            if cat in r['name']:
                categories[cat].append(r)
                break
    
    for cat, cat_results in categories.items():
        if not cat_results:
            continue
        cat_ratios = [r['ratio'] for r in cat_results if r['ratio']]
        if cat_ratios:
            avg = sum(cat_ratios) / len(cat_ratios)
            print(f"  {cat:12s}: {avg:.2f}x average ({len(cat_results)} instances)")

if __name__ == "__main__":
    main()
