#!/usr/bin/env python3
"""
Comprehensive benchmarking of Satience vs MiniSat
Tests on diverse GBD instances and generates detailed analysis
"""

import subprocess
import sqlite3
import os
import time
import json
from pathlib import Path
from dataclasses import dataclass, asdict
from typing import Optional, Dict, List, Tuple

@dataclass
class BenchmarkResult:
    instance: str
    hash: str
    family: str
    track: str
    expected_result: str
    vars: int
    clauses: int
    satience_time: float
    satience_result: str
    satience_conflicts: int
    satience_decisions: int
    minisat_time: float
    minisat_result: str
    minisat_conflicts: int
    speedup: float  # minisat_time / satience_time
    match: bool
    timeout: bool

def get_cnf_info(cnf_path: str) -> Tuple[int, int]:
    """Get variable and clause count from CNF file"""
    with open(cnf_path, 'r') as f:
        for line in f:
            if line.startswith('p cnf'):
                parts = line.split()
                try:
                    return int(parts[2]), int(parts[3])
                except (IndexError, ValueError):
                    pass
    return 0, 0

def run_satience(cnf_path: str, timeout: int = 60) -> Tuple[str, float, int, int]:
    """Run Satience solver, return (result, time, conflicts, decisions)"""
    try:
        start = time.time()
        result = subprocess.run(
            ['./satience', '-verbose', cnf_path],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start

        # Parse output
        conflicts = 0
        decisions = 0
        for line in result.stderr.split('\n'):
            if 'Conflicts:' in line:
                conflicts = int(line.split(':')[1].strip())
            elif 'Decisions:' in line:
                decisions = int(line.split(':')[1].strip())

        # Determine result
        if result.returncode == 10:
            result_str = 'SAT'
        elif result.returncode == 20:
            result_str = 'UNSAT'
        else:
            result_str = 'TIMEOUT' if result.returncode == 0 and elapsed >= timeout else 'UNKNOWN'

        return result_str, elapsed, conflicts, decisions
    except subprocess.TimeoutExpired:
        return 'TIMEOUT', timeout, 0, 0
    except Exception as e:
        print(f"Error running satience: {e}")
        return 'ERROR', 0, 0, 0

def run_minisat(cnf_path: str, timeout: int = 60) -> Tuple[str, float, int]:
    """Run MiniSat solver, return (result, time, conflicts)"""
    try:
        start = time.time()
        result = subprocess.run(
            ['minisat', cnf_path, '/tmp/minisat_out.txt'],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start

        # Parse MiniSat output
        conflicts = 0
        with open('/tmp/minisat_out.txt', 'r') as f:
            for line in f:
                if line.startswith('conflicts'):
                    parts = line.split()
                    if len(parts) >= 2:
                        conflicts = int(parts[1].replace(',', ''))
                    break

        # Determine result
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
        print(f"Error running minisat: {e}")
        return 'ERROR', 0, 0

def select_instances(n: int = 50, max_vars: int = 500) -> List[str]:
    """Select diverse instances from GBD database"""
    # Get all instance files that exist
    instance_files = [f for f in os.listdir('benchmark/gbd_instances') if f.endswith('.cnf')]

    # Get metadata for these instances
    hashes = [f.replace('.cnf', '') for f in instance_files]
    if not hashes:
        return []

    conn = sqlite3.connect('benchmark/meta.db')
    cursor = conn.cursor()

    placeholders = ','.join('?' * len(hashes))
    cursor.execute(f'''
        SELECT hash, family, result, track
        FROM Features
        WHERE hash IN ({placeholders})
          AND result IS NOT NULL
          AND result != 'unknown'
        ORDER BY RANDOM()
        LIMIT ?
    ''', hashes + [n])

    selected = []
    for row in cursor.fetchall():
        hash_val, family, result, track = row
        cnf_path = f'benchmark/gbd_instances/{hash_val}.cnf'
        if os.path.exists(cnf_path):
            # Check variable count
            vars_count, _ = get_cnf_info(cnf_path)
            if vars_count <= max_vars:
                selected.append(cnf_path)

    conn.close()
    return selected[:n]

def benchmark_instance(cnf_path: str, minisat_available: bool) -> Optional[BenchmarkResult]:
    """Benchmark a single instance"""
    hash_val = Path(cnf_path).stem

    # Get metadata
    conn = sqlite3.connect('benchmark/meta.db')
    cursor = conn.cursor()
    cursor.execute('SELECT family, result, track FROM Features WHERE hash = ?', (hash_val,))
    row = cursor.fetchone()
    conn.close()

    if not row:
        return None

    family, expected_result, track = row
    vars_count, clauses_count = get_cnf_info(cnf_path)

    print(f"\n{'='*60}")
    print(f"Benchmarking: {hash_val}")
    print(f"Family: {family}, Expected: {expected_result}")
    print(f"Vars: {vars_count}, Clauses: {clauses_count}")
    print(f"{'='*60}")

    # Run Satience
    print("Running Satience...")
    sat_result, sat_time, sat_conflicts, sat_decisions = run_satience(cnf_path)
    print(f"Satience: {sat_result} in {sat_time:.3f}s ({sat_conflicts} conflicts, {sat_decisions} decisions)")

    # Run MiniSat
    min_result = 'N/A'
    min_time = 0
    min_conflicts = 0

    if minisat_available:
        print("Running MiniSat...")
        min_result, min_time, min_conflicts = run_minisat(cnf_path)
        print(f"MiniSat: {min_result} in {min_time:.3f}s ({min_conflicts} conflicts)")
    else:
        print("MiniSat not available, skipping")

    # Calculate speedup
    speedup = min_time / sat_time if (sat_time > 0 and min_time > 0) else 0
    match = (sat_result == min_result) if minisat_available else True
    timeout = (sat_result == 'TIMEOUT')

    return BenchmarkResult(
        instance=cnf_path,
        hash=hash_val,
        family=family,
        track=track or 'unknown',
        expected_result=expected_result,
        vars=vars_count,
        clauses=clauses_count,
        satience_time=sat_time,
        satience_result=sat_result,
        satience_conflicts=sat_conflicts,
        satience_decisions=sat_decisions,
        minisat_time=min_time,
        minisat_result=min_result,
        minisat_conflicts=min_conflicts,
        speedup=speedup,
        match=match,
        timeout=timeout
    )

def generate_report(results: List[BenchmarkResult], output_path: str):
    """Generate comprehensive benchmark report"""

    # Filter out errors
    valid_results = [r for r in results if r.satience_result not in ['ERROR']]

    # Calculate statistics
    total = len(valid_results)
    sat_correct = sum(1 for r in valid_results if r.satience_result == r.expected_result)
    minisat_available = any(r.minisat_result != 'N/A' for r in valid_results)

    if minisat_available:
        matches = sum(1 for r in valid_results if r.match)
        sat_faster = sum(1 for r in valid_results if r.speedup < 1.0)
        minisat_faster = sum(1 for r in valid_results if r.speedup > 1.0)

        # Calculate median speedup (excluding timeouts)
        speedups = [r.speedup for r in valid_results if r.speedup > 0 and not r.timeout]
        median_speedup = sorted(speedups)[len(speedups)//2] if speedups else 0
    else:
        matches = 0
        sat_faster = 0
        minisat_faster = 0
        median_speedup = 0

    # Group by family
    families = {}
    for r in valid_results:
        if r.family not in families:
            families[r.family] = []
        families[r.family].append(r)

    # Generate report
    report = []
    report.append("# Comprehensive Benchmark Results")
    report.append("")
    report.append(f"**Date**: {time.strftime('%Y-%m-%d %H:%M:%S')}")
    report.append(f"**Instances**: {total} (from GBD database)")
    report.append(f"**Timeout**: 60 seconds")
    report.append("")

    report.append("## Summary")
    report.append("")
    report.append(f"- **Total instances**: {total}")
    report.append(f"- **Satience correct**: {sat_correct}/{total} ({100*sat_correct/total:.1f}%)")

    if minisat_available:
        report.append(f"- **Matches MiniSat**: {matches}/{total} ({100*matches/total:.1f}%)")
        report.append(f"- **Satience faster**: {sat_faster}/{total}")
        report.append(f"- **MiniSat faster**: {minisat_faster}/{total}")
        report.append(f"- **Median speedup**: {median_speedup:.2f}x (MiniSat/Satience)")
        report.append("")

        if median_speedup > 0:
            if median_speedup < 1.0:
                report.append(f"**Satience is {1.0/median_speedup:.1f}x FASTER than MiniSat on median!**")
            else:
                report.append(f"**Satience is {median_speedup:.1f}x slower than MiniSat on median**")
    report.append("")

    report.append("## Results by Family")
    report.append("")

    for family, family_results in sorted(families.items()):
        report.append(f"### {family} ({len(family_results)} instances)")
        report.append("")
        report.append("| Instance | Vars | Clauses | Expected | Satience | MiniSat | Time (Sat/Min) | Speedup |")
        report.append("|----------|------|---------|----------|----------|---------|----------------|---------|")

        for r in sorted(family_results, key=lambda x: x.instance):
            sat_time_str = f"{r.satience_time:.3f}s" if r.satience_time < 60 else f"{r.satience_time:.1f}s (TO)"
            min_time_str = f"{r.minisat_time:.3f}s" if r.minisat_time < 60 and r.minisat_time > 0 else "N/A"
            speedup_str = f"{r.speedup:.2f}x" if r.speedup > 0 else "N/A"

            report.append(f"| {r.hash[:16]}... | {r.vars} | {r.clauses} | {r.expected_result} | {r.satience_result} | {r.minisat_result} | {sat_time_str}/{min_time_str} | {speedup_str} |")

        report.append("")

    report.append("## Detailed Results")
    report.append("")
    report.append("| Hash | Family | Vars | Clauses | Satience | MiniSat | Speedup | Match |")
    report.append("|------|--------|------|---------|----------|---------|---------|-------|")

    for r in sorted(valid_results, key=lambda x: x.satience_time):
        speedup_str = f"{r.speedup:.2f}x" if r.speedup > 0 else "N/A"
        match_str = "✓" if r.match else "✗"
        report.append(f"| {r.hash[:16]}... | {r.family} | {r.vars} | {r.clauses} | {r.satience_time:.3f}s | {r.minisat_time:.3f}s | {speedup_str} | {match_str} |")

    report.append("")

    # JSON output
    json_data = {
        'summary': {
            'total': total,
            'satience_correct': sat_correct,
            'matches': matches,
            'median_speedup': median_speedup
        },
        'results': [asdict(r) for r in valid_results]
    }

    with open(output_path.replace('.md', '.json'), 'w') as f:
        json.dump(json_data, f, indent=2)

    return '\n'.join(report)

def main():
    import argparse
    parser = argparse.ArgumentParser(description='Comprehensive benchmarking')
    parser.add_argument('-n', type=int, default=30, help='Number of instances to test')
    parser.add_argument('--max-vars', type=int, default=500, help='Maximum variables')
    parser.add_argument('--output', type=str, default='benchmark/comprehensive_results.md', help='Output file')
    parser.add_argument('--no-minisat', action='store_true', help='Skip MiniSat comparison')
    args = parser.parse_args()

    # Check if MiniSat is available
    minisat_available = not args.no_minisat
    try:
        subprocess.run(['minisat', '--help'], capture_output=True, timeout=5)
    except (subprocess.TimeoutExpired, FileNotFoundError):
        minisat_available = False

    print(f"Starting comprehensive benchmark...")
    print(f"Instances: {args.n}, Max vars: {args.max_vars}")
    print(f"MiniSat: {'Available' if minisat_available else 'Not available'}")

    # Select instances
    instances = select_instances(args.n, args.max_vars)
    print(f"Selected {len(instances)} instances")

    # Benchmark each instance
    results = []
    for i, cnf_path in enumerate(instances, 1):
        print(f"\n[{i}/{len(instances)}] {cnf_path}")
        result = benchmark_instance(cnf_path, minisat_available)
        if result:
            results.append(result)

    # Generate report
    if results:
        report = generate_report(results, args.output)
        with open(args.output, 'w') as f:
            f.write(report)
        print(f"\n\nReport saved to: {args.output}")
        print(f"JSON data saved to: {args.output.replace('.md', '.json')}")
    else:
        print("No results to report!")

if __name__ == '__main__':
    main()
