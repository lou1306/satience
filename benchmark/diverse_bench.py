#!/usr/bin/env python3
"""Comprehensive benchmark across diverse SAT families.

Note: Runs instances sequentially (max 1 solver instance at a time).
Do not add parallel execution - limit is 8 concurrent solver instances.
"""

import subprocess
import time
import signal
import os
from pathlib import Path

TIMEOUT = 60
SATIENCE = "./satience"

# Diverse benchmark families - use existing small instances
INSTANCES = [
    # Algebra
    ("algebra", "algebra_xor_20_sat.cnf"),
    ("algebra", "algebra_xor_30_sat.cnf"),
    ("algebra", "algebra_xor_40_sat.cnf"),
    
    # Argumentation
    ("arg_chain", "arg_chain_50_sat.cnf"),
    ("arg_chain", "arg_chain_100_sat.cnf"),
    ("arg_chain", "arg_chain_150_sat.cnf"),
    
    # Random
    ("random", "random_k3_50v_200c_sat.cnf"),
    ("random", "random_k3_75v_300c_sat.cnf"),
    ("random", "random_k3_100v_400c_sat.cnf"),
    
    # Tseitin
    ("tseitin", "tseitin_grid_4x4_unsat.cnf"),
    ("tseitin", "tseitin_grid_5x5_sat.cnf"),
    ("tseitin", "tseitin_grid_6x6_sat.cnf"),
    ("tseitin", "tseitin_grid_7x7_sat.cnf"),
    
    # Pigeonhole
    ("php", "php_5p_6h_sat.cnf"),
    ("php", "php_6p_5h_unsat.cnf"),
    
    # Sudoku
    ("sudoku", "sudoku_3x3_empty_sat.cnf"),
]

def cleanup_solver_processes():
    """Kill any orphaned solver processes."""
    try:
        result = subprocess.run(
            "pgrep -f 'satience|minisat'",
            shell=True,
            capture_output=True,
            text=True
        )
        if result.stdout.strip():
            pids = [pid.strip() for pid in result.stdout.strip().split('\n') if pid.strip()]
            for pid in pids:
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except (ProcessLookupError, PermissionError, ValueError):
                    pass
            print(f"c [cleanup] Killed {len(pids)} orphaned solver processes")
    except Exception:
        pass

def run_solver(solver, cnf_path, timeout=TIMEOUT):
    """Run solver and return (result, time).
    
    Ensures process is properly killed even on timeout.
    """
    try:
        start = time.time()
        proc = subprocess.Popen(
            f"{solver} {cnf_path}",
            shell=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            preexec_fn=os.setsid
        )
        
        try:
            stdout, stderr = proc.communicate(timeout=timeout)
            elapsed = time.time() - start
            
            if "SAT" in stdout:
                return "SAT", elapsed
            elif "UNSAT" in stdout:
                return "UNSAT", elapsed
            else:
                return "UNKNOWN", elapsed
        except subprocess.TimeoutExpired:
            try:
                os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass
            proc.wait()
            return "TIMEOUT", timeout
    except Exception as e:
        return "ERROR", 0

def main():
    # Clean up orphaned processes
    cleanup_solver_processes()
    
    print("=" * 100)
    print("SATIENCE vs MiniSat - Diverse Family Benchmark")
    print("=" * 100)
    print(f"{'Instance':<45} {'Vars':>6} {'Cls':>8} {'Res':>6} {'Satience':>10} {'MiniSat':>10} {'Ratio':>8}")
    print("-" * 100)
    
    results = []
    solved = 0
    total_time_satience = 0
    total_time_minisat = 0
    
    for family, filename in INSTANCES:
        cnf_path = Path("benchmark/gbd_instances") / filename
        if not cnf_path.exists():
            print(f"{filename:<45} {'-':>6} {'-':>8} {'-':>6} {'-':>10} {'-':>10} {'-':>8} [NOT FOUND]")
            continue
        
        # Get instance stats
        vars_count = 0
        clauses_count = 0
        with open(cnf_path, 'r') as f:
            for line in f:
                if line.startswith('p cnf'):
                    parts = line.split()
                    vars_count = int(parts[2])
                    clauses_count = int(parts[3])
                    break
        
        # Run both solvers
        sat_result, sat_time = run_solver(SATIENCE, str(cnf_path))
        ms_result, ms_time = run_solver("minisat", str(cnf_path))
        
        # Calculate ratio
        ratio = sat_time / ms_time if sat_time > 0 and ms_time > 0 else 0
        ratio_str = f"{ratio:.2f}x" if ratio > 0 else "-"
        
        # Check if both agree
        match = "✓" if sat_result == ms_result else "✗"
        
        print(f"{filename:<45} {vars_count:>6} {clauses_count:>8} {sat_result:>6} {sat_time:>10.3f}s {ms_time:>10.3f}s {ratio_str:>8} [{family}] {match}")
        
        if sat_result not in ["TIMEOUT", "ERROR", "UNKNOWN"]:
            solved += 1
            total_time_satience += sat_time
            total_time_minisat += ms_time
            if ratio > 0:
                results.append((family, ratio, sat_time, ms_time))
    
    print("=" * 100)
    print(f"\nSUMMARY:")
    print(f"  Total: {len(INSTANCES)}, Solved: {solved}")
    if results:
        avg_ratio = sum(r[1] for r in results) / len(results)
        median_ratio = sorted(r[1] for r in results)[len(results)//2]
        print(f"  Avg ratio: {avg_ratio:.2f}x, Median: {median_ratio:.2f}x")
        print(f"  Total time - Satience: {total_time_satience:.2f}s, MiniSat: {total_time_minisat:.2f}s")
        
        # By family
        print(f"\nBY FAMILY:")
        families = {}
        for fam, ratio, st, mt in results:
            if fam not in families:
                families[fam] = []
            families[fam].append((ratio, st, mt))
        
        for fam in sorted(families.keys()):
            fam_results = families[fam]
            avg = sum(r[0] for r in fam_results) / len(fam_results)
            total_st = sum(r[1] for r in fam_results)
            total_mt = sum(r[2] for r in fam_results)
            print(f"  {fam:<20}: {avg:.2f}x average ({len(fam_results)} instances)")

if __name__ == "__main__":
    main()
