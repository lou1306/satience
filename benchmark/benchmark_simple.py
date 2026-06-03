#!/usr/bin/env python3
"""Benchmark satience solver on small SAT instances."""

import os
import subprocess
import time
import tempfile
import urllib.request
import polars as pl
from pathlib import Path

# List of small-to-medium SAT instances from SAT Competition
# Format: (URL, expected_result, name, description)
INSTANCES = [
    # Small satisfiable instances
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf20-01.cnf",
        "SAT",
        "uf20-01",
        "20 vars, 91 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf20-02.cnf",
        "SAT",
        "uf20-02",
        "20 vars, 91 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf20-03.cnf",
        "SAT",
        "uf20-03",
        "20 vars, 91 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf20-04.cnf",
        "SAT",
        "uf20-04",
        "20 vars, 91 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf20-05.cnf",
        "SAT",
        "uf20-05",
        "20 vars, 91 clauses, random SAT"
    ),
    # Medium satisfiable instances
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf50-01.cnf",
        "SAT",
        "uf50-01",
        "50 vars, 218 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf50-02.cnf",
        "SAT",
        "uf50-02",
        "50 vars, 218 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf50-03.cnf",
        "SAT",
        "uf50-03",
        "50 vars, 218 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf50-04.cnf",
        "SAT",
        "uf50-04",
        "50 vars, 218 clauses, random SAT"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/uf50-05.cnf",
        "SAT",
        "uf50-05",
        "50 vars, 218 clauses, random SAT"
    ),
    # Small unsatisfiable instances (hole/pigeonhole principle)
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/unsat/hole6.cnf",
        "UNSAT",
        "hole6",
        "6 holes, UNSAT pigeonhole"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/unsat/hole7.cnf",
        "UNSAT",
        "hole7",
        "7 holes, UNSAT pigeonhole"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/unsat/hole8.cnf",
        "UNSAT",
        "hole8",
        "8 holes, UNSAT pigeonhole"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/unsat/hole9.cnf",
        "UNSAT",
        "hole9",
        "9 holes, UNSAT pigeonhole"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/unsat/hole10.cnf",
        "UNSAT",
        "hole10",
        "10 holes, UNSAT pigeonhole"
    ),
    # Additional SAT instances
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/dup-03.cnf",
        "SAT",
        "dup-03",
        "Dubois benchmark"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/dup-04.cnf",
        "SAT",
        "dup-04",
        "Dubois benchmark"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/dup-05.cnf",
        "SAT",
        "dup-05",
        "Dubois benchmark"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/dup-06.cnf",
        "SAT",
        "dup-06",
        "Dubois benchmark"
    ),
    (
        "https://raw.githubusercontent.com/arminbiere/satqbf/master/benchmarks/sat/dup-07.cnf",
        "SAT",
        "dup-07",
        "Dubois benchmark"
    ),
]


def download_instance(url: str, output_path: Path) -> bool:
    """Download a CNF instance from URL."""
    if output_path.exists():
        return True
    
    try:
        urllib.request.urlretrieve(url, output_path)
        return True
    except Exception as e:
        print(f"  Download failed: {e}")
        return False


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
            [solver_path, cnf_path],
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
            "timeout": False,
            "correct": status == "SAT" or status == "UNSAT"
        }
    except subprocess.TimeoutExpired:
        return {
            "status": "TIMEOUT",
            "exit_code": -1,
            "time": timeout,
            "timeout": True,
            "correct": False
        }


def benchmark_solver(
    solver_path: str,
    instances: list[tuple],
    timeout: int = 60
) -> pl.DataFrame:
    """Benchmark satience on multiple SAT instances."""
    solver_path = os.path.abspath(solver_path)
    
    print(f"Benchmarking: {solver_path}")
    print(f"Instances: {len(instances)}, Timeout: {timeout}s\n")
    
    with tempfile.TemporaryDirectory() as tmpdir:
        tmpdir_path = Path(tmpdir)
        
        results = []
        for i, (url, expected, name, desc) in enumerate(instances, 1):
            cnf_path = tmpdir_path / f"{name}.cnf"
            print(f"[{i}/{len(instances)}] {name}: {desc}")
            
            if not download_instance(url, cnf_path):
                results.append({
                    "instance": name,
                    "expected": expected,
                    "status": "DOWNLOAD_ERROR",
                    "exit_code": -1,
                    "time": 0.0,
                    "timeout": False,
                    "correct": False,
                    "vars": 0,
                    "clauses": 0
                })
                continue
            
            vars_count, clauses_count = get_cnf_info(cnf_path)
            print(f"  Size: {vars_count} vars, {clauses_count} clauses")
            
            result = run_solver(solver_path, cnf_path, timeout)
            
            # Check if result matches expected
            if result["status"] == expected:
                result["correct"] = True
            elif result["status"] in ["SAT", "UNSAT"]:
                result["correct"] = False
            
            results.append({
                "instance": name,
                "expected": expected,
                "status": result["status"],
                "exit_code": result["exit_code"],
                "time": result["time"],
                "timeout": result["timeout"],
                "correct": result["correct"],
                "vars": vars_count,
                "clauses": clauses_count
            })
            
            status_str = f"{result['status']}"
            if result["correct"]:
                status_str += " ✓"
            else:
                status_str += f" (expected {expected})"
            status_str += f" ({result['time']:.3f}s)"
            print(f"  Result: {status_str}")
        
        return pl.DataFrame(results)


def main():
    solver_path = os.environ.get("SATIENCE_SOLVER", "../satience")
    timeout = int(os.environ.get("SATIENCE_TIMEOUT", "60"))
    
    if not os.path.exists(solver_path):
        print(f"Solver not found at {solver_path}")
        print("Set SATIENCE_SOLVER environment variable or build satience:")
        print("  cd .. && go build -o satience ./cmd/satience")
        return 1
    
    df = benchmark_solver(
        solver_path,
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
    
    # Save results
    output_file = "benchmark_results.csv"
    df.write_csv(output_file)
    print(f"\nResults saved to: {output_file}")
    
    return 0 if len(correct) == total else 1


if __name__ == "__main__":
    exit(main())
