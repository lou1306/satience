#!/usr/bin/env python3
"""
Benchmark CHB heuristic vs VSIDS and MiniSat on random GBD instances.

Generates CSV output with per-instance results and summary statistics.
"""

import sqlite3
import subprocess
import time
import random
import os
import sys
from pathlib import Path
from dataclasses import dataclass, asdict
from typing import Optional, Tuple

@dataclass
class BenchmarkResult:
    instance_hash: str
    instance_file: str
    family: str
    n_vars: int
    n_clauses: int
    expected_result: str
    
    # Satience VSIDS
    vsids_result: str = "UNKNOWN"
    vsids_time: float = 0.0
    vsids_conflicts: int = 0
    vsids_decisions: int = 0
    
    # Satience CHB
    chb_result: str = "UNKNOWN"
    chb_time: float = 0.0
    chb_conflicts: int = 0
    chb_decisions: int = 0
    
    # MiniSat
    minisat_result: str = "UNKNOWN"
    minisat_time: float = 0.0
    minisat_conflicts: int = 0
    minisat_decisions: int = 0
    
    # Calculated
    chb_vs_vsids_speedup: float = 0.0
    satience_vs_minisat_speedup: float = 0.0


def get_downloaded_instances(instances_dir: Path) -> list[str]:
    """Get list of downloaded instance hashes."""
    if not instances_dir.exists():
        return []
    
    hashes = []
    for f in instances_dir.glob("*.cnf"):
        hashes.append(f.stem)
    return hashes


def get_instance_metadata(db_path: Path, instance_hash: str) -> Optional[dict]:
    """Get instance metadata from database."""
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    cursor = conn.cursor()
    
    cursor.execute("""
        SELECT f.hash, f.family, f.result, f.minisat1m,
               l.value as local_path
        FROM features f
        JOIN local l ON f.hash = l.hash
        WHERE f.hash = ?
    """, (instance_hash,))
    
    row = cursor.fetchone()
    conn.close()
    
    if not row:
        return None
    
    return {
        'hash': row['hash'],
        'family': row['family'] or 'unknown',
        'result': row['result'] or 'unknown',
        'minisat1m': row['minisat1m'] or '',
        'local_path': row['local_path'] if row['local_path'] != 'None' else None
    }


def get_instance_stats(cnf_file: Path) -> Tuple[int, int]:
    """Get number of variables and clauses from CNF file."""
    with open(cnf_file, 'r') as f:
        for line in f:
            if line.startswith('p cnf'):
                parts = line.split()
                if len(parts) >= 4:
                    return int(parts[2]), int(parts[3])
    return 0, 0


def select_random_instances(
    db_path: Path,
    instances_dir: Path,
    n_instances: int = 45,
    timeout: float = 30.0
) -> list[dict]:
    """
    Select random instances for benchmarking.
    
    Strategy:
    - Get all downloaded instances with known results
    - Stratify by size (small/medium/large)
    - Ensure family diversity
    """
    downloaded = get_downloaded_instances(instances_dir)
    print(f"Found {len(downloaded)} downloaded instances")
    
    # Get metadata for all downloaded instances
    instances = []
    for hash_val in downloaded:
        meta = get_instance_metadata(db_path, hash_val)
        if not meta or meta['result'] not in ('sat', 'unsat'):
            continue
        
        cnf_file = instances_dir / f"{hash_val}.cnf"
        if not cnf_file.exists():
            continue
        
        n_vars, n_clauses = get_instance_stats(cnf_file)
        if n_vars == 0:
            continue
        
        # Categorize by size
        if n_vars < 500:
            size_cat = 'small'
        elif n_vars < 5000:
            size_cat = 'medium'
        else:
            size_cat = 'large'
        
        instances.append({
            'hash': hash_val,
            'family': meta['family'],
            'result': meta['result'],
            'n_vars': n_vars,
            'n_clauses': n_clauses,
            'size_cat': size_cat,
            'file': cnf_file
        })
    
    print(f"Found {len(instances)} instances with metadata")
    
    # Stratified sampling: aim for balanced size distribution
    small = [i for i in instances if i['size_cat'] == 'small']
    medium = [i for i in instances if i['size_cat'] == 'medium']
    large = [i for i in instances if i['size_cat'] == 'large']
    
    # Target: 15 small, 15 medium, 15 large (adjust based on availability)
    target_per_size = n_instances // 3
    
    selected = []
    random.seed(42)  # For reproducibility
    
    for pool, name in [(small, 'small'), (medium, 'medium'), (large, 'large')]:
        if len(pool) < target_per_size:
            # Take all from this category
            selected.extend(pool)
            print(f"  {name}: taking all {len(pool)} instances")
        else:
            # Random sample
            sample = random.sample(pool, target_per_size)
            selected.extend(sample)
            print(f"  {name}: sampled {target_per_size} from {len(pool)}")
    
    # Ensure family diversity
    families = set(i['family'] for i in selected)
    print(f"Selected {len(selected)} instances from {len(families)} families")
    
    return selected


def run_solver(
    solver_cmd: list[str],
    cnf_file: Path,
    timeout_sec: float = 30.0
) -> Tuple[str, float, Optional[int], Optional[int]]:
    """
    Run solver and parse result.
    
    Returns: (result, time_seconds, conflicts, decisions)
    """
    start_time = time.time()
    
    try:
        result = subprocess.run(
            solver_cmd + [str(cnf_file)],
            capture_output=True,
            text=True,
            timeout=timeout_sec
        )
        elapsed = time.time() - start_time
        output = result.stdout + result.stderr
        
        # Parse result
        if 'SATISFIABLE' in output or 'SAT' in output:
            solver_result = 'SAT'
        elif 'UNSATISFIABLE' in output or 'UNSAT' in output:
            solver_result = 'UNSAT'
        else:
            solver_result = 'UNKNOWN'
        
        # Parse statistics from verbose output (if available)
        conflicts = None
        decisions = None
        
        for line in output.split('\n'):
            if 'conflicts' in line.lower():
                try:
                    # Look for patterns like "1234 conflicts"
                    parts = line.split()
                    for i, part in enumerate(parts):
                        if 'conflicts' in part.lower() and i > 0:
                            conflicts = int(parts[i-1].replace(',', ''))
                            break
                except:
                    pass
            
            if 'decisions' in line.lower():
                try:
                    parts = line.split()
                    for i, part in enumerate(parts):
                        if 'decisions' in part.lower() and i > 0:
                            decisions = int(parts[i-1].replace(',', ''))
                            break
                except:
                    pass
        
        return solver_result, elapsed, conflicts, decisions
        
    except subprocess.TimeoutExpired:
        elapsed = time.time() - start_time
        return 'TIMEOUT', elapsed, None, None
    except Exception as e:
        elapsed = time.time() - start_time
        return f'ERROR: {e}', elapsed, None, None


def run_benchmark(
    instances: list[dict],
    satience_bin: Path,
    minisat_bin: Path,
    timeout_sec: float = 30.0
) -> list[BenchmarkResult]:
    """Run benchmark on all instances."""
    results = []
    
    for i, inst in enumerate(instances):
        print(f"\n[{i+1}/{len(instances)}] {inst['hash']} ({inst['n_vars']}v, {inst['family']})")
        
        result = BenchmarkResult(
            instance_hash=inst['hash'],
            instance_file=str(inst['file']),
            family=inst['family'],
            n_vars=inst['n_vars'],
            n_clauses=inst['n_clauses'],
            expected_result=inst['result']
        )
        
        # Run Satience with VSIDS (default)
        print(f"  VSIDS...", end=' ', flush=True)
        vsids_result, vsids_time, vsids_conflicts, vsids_decisions = run_solver(
            [str(satience_bin), '-verbose'],
            inst['file'],
            timeout_sec
        )
        result.vsids_result = vsids_result
        result.vsids_time = vsids_time
        result.vsids_conflicts = vsids_conflicts or 0
        result.vsids_decisions = vsids_decisions or 0
        print(f"{vsids_result} ({vsids_time:.3f}s)")
        
        # Run Satience with CHB
        print(f"  CHB...", end=' ', flush=True)
        chb_result, chb_time, chb_conflicts, chb_decisions = run_solver(
            [str(satience_bin), '-verbose', '-chb'],
            inst['file'],
            timeout_sec
        )
        result.chb_result = chb_result
        result.chb_time = chb_time
        result.chb_conflicts = chb_conflicts or 0
        result.chb_decisions = chb_decisions or 0
        print(f"{chb_result} ({chb_time:.3f}s)")
        
        # Run MiniSat
        print(f"  MiniSat...", end=' ', flush=True)
        minisat_result, minisat_time, minisat_conflicts, minisat_decisions = run_solver(
            [str(minisat_bin)],
            inst['file'],
            timeout_sec
        )
        result.minisat_result = minisat_result
        result.minisat_time = minisat_time
        result.minisat_conflicts = minisat_conflicts or 0
        result.minisat_decisions = minisat_decisions or 0
        print(f"{minisat_result} ({minisat_time:.3f}s)")
        
        # Calculate speedups
        if vsids_time > 0 and chb_time > 0:
            result.chb_vs_vsids_speedup = vsids_time / chb_time
        
        if minisat_time > 0 and chb_time > 0:
            result.satience_vs_minisat_speedup = minisat_time / chb_time
        
        results.append(result)
    
    return results


def save_results_csv(results: list[BenchmarkResult], output_file: Path):
    """Save results to CSV file."""
    import csv
    
    fieldnames = [
        'instance_hash', 'instance_file', 'family', 'n_vars', 'n_clauses', 'expected_result',
        'vsids_result', 'vsids_time', 'vsids_conflicts', 'vsids_decisions',
        'chb_result', 'chb_time', 'chb_conflicts', 'chb_decisions',
        'minisat_result', 'minisat_time', 'minisat_conflicts', 'minisat_decisions',
        'chb_vs_vsids_speedup', 'satience_vs_minisat_speedup'
    ]
    
    with open(output_file, 'w', newline='') as f:
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        
        for result in results:
            writer.writerow(asdict(result))
    
    print(f"\nResults saved to: {output_file}")


def generate_summary(results: list[BenchmarkResult], output_file: Path):
    """Generate summary statistics."""
    
    def count_correct(results, result_field):
        correct = 0
        timeout = 0
        wrong = 0
        for r in results:
            res = getattr(r, result_field)
            if res == 'TIMEOUT':
                timeout += 1
            elif res == r.expected_result:
                correct += 1
            else:
                wrong += 1
        return correct, timeout, wrong
    
    def avg_time(results, time_field, filter_field=None, filter_value=None):
        times = []
        for r in results:
            if filter_value is None or (filter_field and getattr(r, filter_field) == filter_value):
                t = getattr(r, time_field)
                if t > 0 and t < 1000:  # Exclude timeouts
                    times.append(t)
        return sum(times) / len(times) if times else 0.0
    
    vsids_correct, vsids_timeout, vsids_wrong = count_correct(results, 'vsids_result')
    chb_correct, chb_timeout, chb_wrong = count_correct(results, 'chb_result')
    minisat_correct, minisat_timeout, minisat_wrong = count_correct(results, 'minisat_result')
    
    summary = []
    summary.append("=" * 70)
    summary.append("CHB vs VSIDS vs MiniSat Benchmark Summary")
    summary.append("=" * 70)
    summary.append(f"\nTotal instances: {len(results)}")
    summary.append("\n--- Solve Rates ---")
    summary.append(f"VSIDS:  {vsids_correct}/{len(results)} ({100*vsids_correct/len(results):.1f}%) - {vsids_timeout} timeouts")
    summary.append(f"CHB:    {chb_correct}/{len(results)} ({100*chb_correct/len(results):.1f}%) - {chb_timeout} timeouts")
    summary.append(f"MiniSat: {minisat_correct}/{len(results)} ({100*minisat_correct/len(results):.1f}%) - {minisat_timeout} timeouts")
    
    summary.append("\n--- Average Solve Time (solved instances only) ---")
    vsids_avg = avg_time(results, 'vsids_time', 'vsids_result', 'SAT')
    vsids_avg_unsat = avg_time(results, 'vsids_time', 'vsids_result', 'UNSAT')
    chb_avg = avg_time(results, 'chb_time', 'chb_result', 'SAT')
    chb_avg_unsat = avg_time(results, 'chb_time', 'chb_result', 'UNSAT')
    minisat_avg = avg_time(results, 'minisat_time', 'minisat_result', 'SAT')
    minisat_avg_unsat = avg_time(results, 'minisat_time', 'minisat_result', 'UNSAT')
    
    # Combine SAT and UNSAT times
    vsids_avg = (vsids_avg + vsids_avg_unsat) / 2 if vsids_avg and vsids_avg_unsat else vsids_avg or vsids_avg_unsat or 0
    chb_avg = (chb_avg + chb_avg_unsat) / 2 if chb_avg and chb_avg_unsat else chb_avg or chb_avg_unsat or 0
    minisat_avg = (minisat_avg + minisat_avg_unsat) / 2 if minisat_avg and minisat_avg_unsat else minisat_avg or minisat_avg_unsat or 0
    
    summary.append(f"VSIDS:  {vsids_avg:.3f}s")
    summary.append(f"CHB:    {chb_avg:.3f}s")
    summary.append(f"MiniSat: {minisat_avg:.3f}s")
    
    # CHB vs VSIDS comparison
    summary.append("\n--- CHB vs VSIDS ---")
    chb_faster = sum(1 for r in results if r.chb_vs_vsids_speedup > 1.0)
    vsids_faster = sum(1 for r in results if r.chb_vs_vsids_speedup < 1.0 and r.chb_vs_vsids_speedup > 0)
    summary.append(f"CHB faster: {chb_faster}/{len(results)} instances")
    summary.append(f"VSIDS faster: {vsids_faster}/{len(results)} instances")
    
    if chb_faster > 0:
        avg_speedup = sum(r.chb_vs_vsids_speedup for r in results if r.chb_vs_vsids_speedup > 1.0) / chb_faster
        summary.append(f"Average speedup when CHB wins: {avg_speedup:.2f}x")
    
    # Satience vs MiniSat
    summary.append("\n--- Satience (CHB) vs MiniSat ---")
    satience_faster = sum(1 for r in results if r.satience_vs_minisat_speedup > 1.0)
    minisat_faster = sum(1 for r in results if r.satience_vs_minisat_speedup < 1.0 and r.satience_vs_minisat_speedup > 0)
    summary.append(f"Satience faster: {satience_faster}/{len(results)} instances")
    summary.append(f"MiniSat faster: {minisat_faster}/{len(results)} instances")
    
    if satience_faster > 0:
        avg_speedup = sum(r.satience_vs_minisat_speedup for r in results if r.satience_vs_minisat_speedup > 1.0) / satience_faster
        summary.append(f"Average speedup when Satience wins: {avg_speedup:.2f}x")
    
    # Family breakdown
    summary.append("\n--- Performance by Family ---")
    families = {}
    for r in results:
        if r.family not in families:
            families[r.family] = {'total': 0, 'chb_faster': 0, 'vsids_faster': 0}
        families[r.family]['total'] += 1
        if r.chb_vs_vsids_speedup > 1.0:
            families[r.family]['chb_faster'] += 1
        elif r.chb_vs_vsids_speedup > 0:
            families[r.family]['vsids_faster'] += 1
    
    for family, stats in sorted(families.items()):
        if stats['total'] >= 2:  # Only show families with multiple instances
            summary.append(f"{family}: {stats['chb_faster']}/{stats['total']} CHB faster")
    
    summary.append("\n" + "=" * 70)
    
    summary_text = '\n'.join(summary)
    print(summary_text)
    
    with open(output_file, 'w') as f:
        f.write(summary_text)
    
    print(f"\nSummary saved to: {output_file}")


def main():
    # Paths
    benchmark_dir = Path(__file__).parent
    project_root = benchmark_dir.parent
    
    db_path = benchmark_dir / "meta.db"
    instances_dir = benchmark_dir / "gbd_instances"
    satience_bin = benchmark_dir / "satience_bench"
    minisat_bin = Path("/home/luca/bin/minisat")
    
    # Output files
    timestamp = time.strftime("%Y%m%d_%H%M%S")
    csv_output = benchmark_dir / f"chb_vs_minisat_results_{timestamp}.csv"
    summary_output = benchmark_dir / f"chb_vs_minisat_summary_{timestamp}.txt"
    
    # Build satience if needed
    if not satience_bin.exists():
        print("Building satience...")
        subprocess.run(["go", "build", "-o", str(satience_bin), str(project_root / "cmd/satience")], check=True)
    
    # Check MiniSat
    if not minisat_bin.exists():
        print(f"ERROR: MiniSat not found at {minisat_bin}")
        sys.exit(1)
    
    print("=" * 70)
    print("CHB vs VSIDS vs MiniSat Benchmark")
    print("=" * 70)
    print(f"Timeout: 30s per instance")
    print(f"Instances directory: {instances_dir}")
    print()
    
    # Select instances
    print("Selecting random instances...")
    instances = select_random_instances(db_path, instances_dir, n_instances=45)
    
    if not instances:
        print("ERROR: No instances found")
        sys.exit(1)
    
    print(f"\nBenchmarking {len(instances)} instances...")
    
    # Run benchmark
    results = run_benchmark(instances, satience_bin, minisat_bin, timeout_sec=30.0)
    
    # Save results
    save_results_csv(results, csv_output)
    generate_summary(results, summary_output)
    
    print("\nBenchmark complete!")


if __name__ == "__main__":
    main()
