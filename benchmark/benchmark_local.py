#!/usr/bin/env python3
"""Benchmark satience solver on local SAT instances."""

import os
import subprocess
import time
import polars as pl
from pathlib import Path

# List of benchmark instances
# Format: (filename, expected_result, description)
INSTANCES = [
    # Trivial SAT instances (satisfiable)
    ("trivial_sat_20_80.cnf", "SAT", "20 vars, 80 clauses"),
    ("trivial_sat_30_120.cnf", "SAT", "30 vars, 120 clauses"),
    ("trivial_sat_40_160.cnf", "SAT", "40 vars, 160 clauses"),
    ("trivial_sat_50_200.cnf", "SAT", "50 vars, 200 clauses"),
    ("trivial_sat_60_240.cnf", "SAT", "60 vars, 240 clauses"),
    ("trivial_sat_70_280.cnf", "SAT", "70 vars, 280 clauses"),
    ("trivial_sat_80_320.cnf", "SAT", "80 vars, 320 clauses"),
    ("trivial_sat_90_360.cnf", "SAT", "90 vars, 360 clauses"),
    ("trivial_sat_100_400.cnf", "SAT", "100 vars, 400 clauses"),
    ("trivial_sat_150_600.cnf", "SAT", "150 vars, 600 clauses"),
    
    # Satisfiable pigeonhole
    ("php_sat_5p_6h.cnf", "SAT", "5 pigeons, 6 holes"),
    ("php_sat_6p_7h.cnf", "SAT", "6 pigeons, 7 holes"),
    ("php_sat_7p_8h.cnf", "SAT", "7 pigeons, 8 holes"),
    ("php_sat_8p_9h.cnf", "SAT", "8 pigeons, 9 holes"),
    ("php_sat_9p_10h.cnf", "SAT", "9 pigeons, 10 holes"),
    ("php_sat_10p_11h.cnf", "SAT", "10 pigeons, 11 holes"),
    
    # Trivial UNSAT
    ("trivial_unsat.cnf", "UNSAT", "trivial contradiction"),
    
    # Unsatisfiable pigeonhole
    ("php_unsat_6p_5h.cnf", "UNSAT", "6 pigeons, 5 holes"),
    ("php_unsat_7p_6h.cnf", "UNSAT", "7 pigeons, 6 holes"),
    ("php_unsat_8p_7h.cnf", "UNSAT", "8 pigeons, 7 holes"),
    ("php_unsat_9p_8h.cnf", "UNSAT", "9 pigeons, 8 holes"),
    ("php_unsat_10p_9h.cnf", "UNSAT", "10 pigeons, 9 holes"),
    ("php_unsat_11p_10h.cnf", "UNSAT", "11 pigeons, 10 holes"),
    
    # Simple SAT
    ("simple_sat_20.cnf", "SAT", "20 vars simple"),
    ("simple_sat_30.cnf", "SAT", "30 vars simple"),
    ("simple_sat_40.cnf", "SAT", "40 vars simple"),
    ("simple_sat_50.cnf", "SAT", "50 vars simple"),
    ("simple_sat_60.cnf", "SAT", "60 vars simple"),
    ("simple_sat_70.cnf", "SAT", "70 vars simple"),
]


def get_cnf_info(cnf_path: Path) -> tuple[int, int]:
    """Get number of variables and clauses from CNF file."""
    with open(cnf_path, 'r') as f:
        for line in f:
            if line.startswith('p cnf'):
                parts = line.split()
                return int(parts[2]), int(parts[3])
    return 0, 0


def run_solver(solver_path: str, cnf_path: Path, timeout: int = 60) -> dict:
    """Run satience solver on a CNF file."""
    start_time = time.time()
    try:
        result = subprocess.run(
            [solver_path, str(cnf_path)],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start_time
        status = result.stdout.strip()
        return {
            "status": status,
            "exit_code": result.returncode,
            "time": elapsed,
            "timeout": False
        }
    except subprocess.TimeoutExpired:
        return {
            "status": "TIMEOUT",
            "exit_code": -1,
            "time": timeout,
            "timeout": True
        }


def benchmark_solver(
    solver_path: str,
    instances_dir: str,
    instances: list[tuple],
    timeout: int = 60
) -> pl.DataFrame:
    """Benchmark satience on multiple SAT instances."""
    solver_path = os.path.abspath(solver_path)
    instances_dir = Path(instances_dir)
    
    print(f"Benchmarking: {solver_path}")
    print(f"Instances directory: {instances_dir}")
    print(f"Instances: {len(instances)}, Timeout: {timeout}s\n")
    
    results = []
    for i, (filename, expected, desc) in enumerate(instances, 1):
        cnf_path = instances_dir / filename
        
        if not cnf_path.exists():
            print(f"[{i}/{len(instances)}] {filename}: NOT FOUND")
            results.append({
                "instance": filename,
                "expected": expected,
                "status": "NOT_FOUND",
                "exit_code": -1,
                "time": 0.0,
                "timeout": False,
                "correct": False,
                "vars": 0,
                "clauses": 0
            })
            continue
        
        vars_count, clauses_count = get_cnf_info(cnf_path)
        print(f"[{i}/{len(instances)}] {filename}: {desc} ({vars_count} vars, {clauses_count} clauses)")
        
        result = run_solver(solver_path, cnf_path, timeout)
        
        # Check if result matches expected
        correct = (result["status"] == expected)
        
        results.append({
            "instance": filename,
            "expected": expected,
            "status": result["status"],
            "exit_code": result["exit_code"],
            "time": result["time"],
            "timeout": result["timeout"],
            "correct": correct,
            "vars": vars_count,
            "clauses": clauses_count
        })
        
        status_str = f"{result['status']}"
        if correct:
            status_str += " ✓"
        else:
            status_str += f" (expected {expected})"
        status_str += f" ({result['time']:.3f}s)"
        print(f"  Result: {status_str}")
    
    return pl.DataFrame(results)


def main():
    solver_path = os.environ.get("SATIENCE_SOLVER", "../satience")
    timeout = int(os.environ.get("SATIENCE_TIMEOUT", "60"))
    instances_dir = os.environ.get("SATIENCE_INSTANCES", "instances")
    
    if not os.path.exists(solver_path):
        print(f"Solver not found at {solver_path}")
        print("Set SATIENCE_SOLVER environment variable or build satience:")
        print("  cd .. && go build -o satience ./cmd/satience")
        return 1
    
    if not os.path.exists(instances_dir):
        print(f"Instances directory not found at {instances_dir}")
        print("Run: uv run python generate_instances.py")
        return 1
    
    df = benchmark_solver(
        solver_path,
        instances_dir,
        INSTANCES,
        timeout=timeout
    )
    
    print("\n" + "=" * 80)
    print("SUMMARY")
    print("=" * 80)
    print(df.select(["instance", "expected", "status", "time", "correct"]))
    
    # Statistics
    total = len(df)
    solved = df.filter(pl.col("status").is_in(["SAT", "UNSAT"]))
    correct = df.filter(pl.col("correct") == True)
    timeouts = df.filter(pl.col("timeout") == True)
    
    print(f"\nTotal: {total}")
    print(f"Solved: {len(solved)}/{total} ({100*len(solved)/total:.1f}%)")
    print(f"Correct: {len(correct)}/{total} ({100*len(correct)/total:.1f}%)")
    print(f"Timeouts: {len(timeouts)}/{total} ({100*len(timeouts)/total:.1f}%)")
    
    if len(solved) > 0:
        print(f"\nAverage time (solved): {solved['time'].mean():.3f}s")
        print(f"Min time: {solved['time'].min():.3f}s")
        print(f"Max time: {solved['time'].max():.3f}s")
    
    # By category
    print("\n" + "=" * 80)
    print("BY CATEGORY")
    print("=" * 80)
    
    sat_instances = df.filter(pl.col("expected") == "SAT")
    unsat_instances = df.filter(pl.col("expected") == "UNSAT")
    
    print(f"\nSAT instances: {len(sat_instances.filter(pl.col('correct') == True))}/{len(sat_instances)} correct")
    print(f"UNSAT instances: {len(unsat_instances.filter(pl.col('correct') == True))}/{len(unsat_instances)} correct")
    
    # Save results
    output_file = "benchmark_results.csv"
    df.write_csv(output_file)
    print(f"\nResults saved to: {output_file}")
    
    return 0 if len(correct) == total else 1


if __name__ == "__main__":
    exit(main())
