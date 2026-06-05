#!/usr/bin/env python3
"""Compare satience vs MiniSat on various SAT/UNSAT benchmarks."""

import subprocess
import sys
import os
import sqlite3
from pathlib import Path

TIMEOUT = 60  # seconds
BENCHMARK_DIR = Path(__file__).parent / "gbd_instances"
META_DB = Path(__file__).parent / "meta.db"

def run_solver(solver_cmd, cnf_file, timeout=TIMEOUT):
    """Run a solver and return (status, returncode, output)."""
    try:
        result = subprocess.run(
            f"{solver_cmd} {cnf_file}",
            shell=True,
            capture_output=True,
            text=True,
            timeout=timeout
        )
        output = result.stdout + result.stderr

        # Parse result
        if "SAT" in output and "UNSAT" not in output:
            status = "SAT"
        elif "UNSAT" in output:
            status = "UNSAT"
        else:
            status = "UNKNOWN"

        return status, result.returncode, output

    except subprocess.TimeoutExpired:
        return "TIMEOUT", None, ""
    except Exception as e:
        return "ERROR", str(e), ""

def time_solver(solver_cmd, cnf_file, timeout=TIMEOUT):
    """Time a solver run."""
    import time
    start = time.time()
    try:
        result = subprocess.run(
            f"{solver_cmd} {cnf_file}",
            shell=True,
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start
        output = result.stdout + result.stderr

        if "SAT" in output and "UNSAT" not in output:
            status = "SAT"
        elif "UNSAT" in output:
            status = "UNSAT"
        else:
            status = "UNKNOWN"

        return status, elapsed
    except subprocess.TimeoutExpired:
        return "TIMEOUT", timeout
    except Exception as e:
        return "ERROR", None

def get_instance_info(cnf_file):
    """Get instance info from meta.db."""
    cnf_hash = cnf_file.stem
    try:
        conn = sqlite3.connect(META_DB)
        cursor = conn.cursor()
        cursor.execute(
            "SELECT family, result, proceedings FROM features WHERE hash = ?",
            (cnf_hash,)
        )
        row = cursor.fetchone()
        conn.close()
        if row:
            return row[0] or "unknown", row[1] or "unknown", row[2] or "unknown"
    except:
        pass
    return "unknown", "unknown", "unknown"

def count_vars_clauses(cnf_file):
    """Count variables and clauses in CNF file."""
    with open(cnf_file, 'r') as f:
        for line in f:
            if line.startswith('p'):
                parts = line.split()
                if len(parts) >= 3 and parts[1] == 'cnf':
                    try:
                        return int(parts[2]), int(parts[3])
                    except ValueError:
                        pass
    return 0, 0

def main():
    # Select diverse instances
    cnf_files = list(BENCHMARK_DIR.glob("*.cnf"))

    # Get metadata for all files
    instances = []
    for cnf in cnf_files:
        family, known_result, track = get_instance_info(cnf)
        vars_count, clauses_count = count_vars_clauses(cnf)

        # Categorize
        if 'php' in cnf.name or 'pigeonhole' in family.lower():
            category = 'pigeonhole'
        elif 'tseitin' in family.lower() or 'tseitin' in cnf.name:
            category = 'tseitin'
        elif 'algebra' in family.lower() or 'xor' in family.lower():
            category = 'algebra_xor'
        elif 'arg_chain' in family.lower() or 'chain' in family.lower():
            category = 'chain'
        elif 'random' in family.lower() or 'random_k3' in family.lower():
            category = 'random'
        elif 'sudoku' in family.lower():
            category = 'sudoku'
        else:
            category = 'other'

        instances.append({
            'file': cnf,
            'family': family,
            'category': category,
            'known_result': known_result,
            'vars': vars_count,
            'clauses': clauses_count
        })

    # Select representative instances from each category
    selected = []
    categories = {}
    for inst in instances:
        cat = inst['category']
        if cat not in categories:
            categories[cat] = []
        categories[cat].append(inst)

    # Pick 2-3 from each category, mix of SAT/UNSAT, various sizes
    for cat, cat_insts in sorted(categories.items()):
        # Sort by vars
        cat_insts.sort(key=lambda x: x['vars'])

        # Pick small, medium if available
        selected.append(cat_insts[0])  # smallest
        if len(cat_insts) > 1:
            selected.append(cat_insts[len(cat_insts)//2])  # medium
        if len(cat_insts) > 3:
            selected.append(cat_insts[-1])  # largest

    print("=" * 100)
    print("SATIENCE vs MiniSat Benchmark Comparison")
    print("=" * 100)
    print(f"{'Instance':<45} {'Vars':>6} {'Clauses':>8} {'Result':>8} {'Satience':>10} {'MiniSat':>10} {'Ratio':>8}")
    print("-" * 100)

    satience_bin = Path(__file__).parent.parent / "satience"
    results = []

    for inst in sorted(selected, key=lambda x: x['vars']):
        cnf_path = inst['file']

        # Run satience
        sat_result, sat_time = time_solver(f"{satience_bin}", cnf_path, timeout=TIMEOUT)

        # Run MiniSat
        minisat_result, minisat_time = time_solver("minisat", cnf_path, timeout=TIMEOUT)

        # Calculate ratio
        if sat_time and minisat_time and minisat_time > 0:
            ratio_val = sat_time / minisat_time
            ratio_str = f"{ratio_val:.2f}x"
        else:
            ratio_val = None
            ratio_str = "N/A"

        # Determine result
        if sat_result == minisat_result:
            result_str = sat_result
        else:
            result_str = f"MISMATCH({sat_result}/{minisat_result})"

        print(f"{cnf_path.name:<45} {inst['vars']:>6} {inst['clauses']:>8} {result_str:>8} {sat_time:>10.3f}s {minisat_time:>10.3f}s {ratio_str:>8}")

        results.append({
            'file': cnf_path.name,
            'category': inst['category'],
            'vars': inst['vars'],
            'clauses': inst['clauses'],
            'sat_result': sat_result,
            'sat_time': sat_time,
            'minisat_result': minisat_result,
            'minisat_time': minisat_time,
            'ratio': ratio_val if 'ratio_val' in dir() else None
        })

    print("=" * 100)

    # Summary statistics
    print("\nSUMMARY BY CATEGORY:")
    print("-" * 100)

    by_category = {}
    for r in results:
        cat = r['category']
        if cat not in by_category:
            by_category[cat] = []
        by_category[cat].append(r)

    for cat in sorted(by_category.keys()):
        cat_results = by_category[cat]
        total = len(cat_results)
        solved = sum(1 for r in cat_results if r['sat_result'] != 'TIMEOUT' and r['sat_result'] != 'ERROR')
        correct = sum(1 for r in cat_results if r['sat_result'] == r['minisat_result'] and r['sat_result'] != 'TIMEOUT')
        wrong = sum(1 for r in cat_results if r['sat_result'] != r['minisat_result'] and r['minisat_result'] != 'TIMEOUT')

        ratios = [r['ratio'] for r in cat_results if r['ratio'] and isinstance(r['ratio'], (int, float))]
        if ratios:
            avg_ratio = sum(ratios) / len(ratios)
            min_ratio = min(ratios)
            max_ratio = max(ratios)
        else:
            avg_ratio = min_ratio = max_ratio = None

        print(f"\n{cat.upper()} ({total} instances):")
        print(f"  Solved: {solved}/{total}, Correct: {correct}, Wrong: {wrong}")
        if avg_ratio:
            print(f"  Avg ratio (satience/minisat): {avg_ratio:.2f}x (range: {min_ratio:.2f}x - {max_ratio:.2f}x)")

    # Overall statistics
    print("\n" + "=" * 100)
    print("OVERALL STATISTICS:")
    print("-" * 100)

    total = len(results)
    solved = sum(1 for r in results if r['sat_result'] != 'TIMEOUT' and r['sat_result'] != 'ERROR')
    correct = sum(1 for r in results if r['sat_result'] == r['minisat_result'] and r['sat_result'] != 'TIMEOUT')
    wrong = sum(1 for r in results if r['sat_result'] != r['minisat_result'] and r['minisat_result'] != 'TIMEOUT')
    timeouts = sum(1 for r in results if r['sat_result'] == 'TIMEOUT')

    all_ratios = [r['ratio'] for r in results if r['ratio'] and isinstance(r['ratio'], (int, float))]
    if all_ratios:
        avg_ratio = sum(all_ratios) / len(all_ratios)
        median_ratio = sorted(all_ratios)[len(all_ratios)//2]
        min_ratio = min(all_ratios)
        max_ratio = max(all_ratios)
    else:
        avg_ratio = median_ratio = min_ratio = max_ratio = None

    print(f"Total instances: {total}")
    print(f"Solved by satience: {solved}/{total}")
    print(f"Correct results: {correct}")
    print(f"Wrong results: {wrong}")
    print(f"Timeouts: {timeouts}")

    if avg_ratio:
        print(f"\nPerformance ratio (satience/minisat):")
        print(f"  Average: {avg_ratio:.2f}x")
        print(f"  Median: {median_ratio:.2f}x")
        print(f"  Range: {min_ratio:.2f}x - {max_ratio:.2f}x")

        if avg_ratio < 1.0:
            print(f"\n✅ Satience is FASTER on average!")
        elif avg_ratio < 2.0:
            print(f"\n👍 Satience is competitive (within 2x)")
        elif avg_ratio < 10.0:
            print(f"\n⚠️  Satience is {avg_ratio:.1f}x slower on average")
        else:
            print(f"\n❌ Satience is {avg_ratio:.1f}x slower on average - optimization needed")

    print("=" * 100)

if __name__ == "__main__":
    main()
