#!/usr/bin/env python3
"""Benchmark satience solver on SAT instances from GBD."""

import os
import subprocess
import time
import tempfile
import polars as pl
from pathlib import Path
from gbd_core.api import GBD


def get_sat_instances(gbd_db_path: str, limit: int = 10) -> list[dict]:
    """Query GBD for SAT instances."""
    with GBD([gbd_db_path]) as gbd:
        df = gbd.query(
            "result=sat",
            resolve=["result", "family"]
        )
        instances = df.head(limit).to_dicts()
        return instances


def download_instance(instance_hash: str, output_dir: Path) -> Path:
    """Download a CNF instance from GBD."""
    cnf_path = output_dir / f"{instance_hash}.cnf"
    if cnf_path.exists():
        return cnf_path
    
    subprocess.run(
        ["gbd", "get", "instance", instance_hash, "-o", str(cnf_path)],
        check=True,
        capture_output=True
    )
    return cnf_path


def run_solver(solver_path: str, cnf_path: Path, timeout: int = 300) -> dict:
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
        return {
            "status": result.stdout.strip(),
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
    gbd_db_path: str,
    num_instances: int = 10,
    timeout: int = 300
) -> pl.DataFrame:
    """Benchmark satience on multiple SAT instances."""
    solver_path = os.path.abspath(solver_path)
    gbd_db_path = os.path.abspath(gbd_db_path)
    
    print(f"Benchmarking {solver_path}")
    print(f"GBD database: {gbd_db_path}")
    print(f"Instances: {num_instances}, Timeout: {timeout}s\n")
    
    with tempfile.TemporaryDirectory() as tmpdir:
        tmpdir_path = Path(tmpdir)
        instances = get_sat_instances(gbd_db_path, num_instances)
        
        results = []
        for i, inst in enumerate(instances, 1):
            instance_id = inst["hash"]
            print(f"[{i}/{num_instances}] Processing {instance_id[:16]}...")
            
            try:
                cnf_path = download_instance(instance_id, tmpdir_path)
                result = run_solver(solver_path, cnf_path, timeout)
                
                results.append({
                    "instance": instance_id,
                    "status": result["status"],
                    "exit_code": result["exit_code"],
                    "time": result["time"],
                    "timeout": result["timeout"]
                })
                
                status_str = f"{result['status']} ({result['time']:.2f}s)"
                print(f"  Result: {status_str}")
                
            except Exception as e:
                print(f"  Error: {e}")
                results.append({
                    "instance": instance_id,
                    "status": "ERROR",
                    "exit_code": -1,
                    "time": 0.0,
                    "timeout": False
                })
        
        return pl.DataFrame(results)


def main():
    solver_path = os.environ.get("SATIENCE_SOLVER", "../satience")
    gbd_db_path = os.environ.get("GBD_DB", "meta.db")
    
    if not os.path.exists(solver_path):
        print(f"Solver not found at {solver_path}")
        print("Set SATIENCE_SOLVER environment variable or build satience:")
        print("  cd .. && go build -o satience ./cmd/satience")
        print("  SATIENCE_SOLVER=/path/to/satience uv run python benchmark.py")
        return 1
    
    if not os.path.exists(gbd_db_path):
        print(f"GBD database not found at {gbd_db_path}")
        print("Set GBD_DB environment variable or download:")
        print("  curl -L -o meta.db https://benchmark-database.de/getdatabase/meta.db")
        return 1
    
    df = benchmark_solver(
        solver_path,
        gbd_db_path,
        num_instances=10,
        timeout=300
    )
    
    print("\n" + "=" * 60)
    print("SUMMARY")
    print("=" * 60)
    print(df)
    
    solved = df.filter(pl.col("status").is_in(["SAT", "UNSAT"]))
    print(f"\nSolved: {len(solved)}/{len(df)}")
    if len(solved) > 0:
        print(f"Average time: {solved['time'].mean():.2f}s")
    
    return 0


if __name__ == "__main__":
    exit(main())
