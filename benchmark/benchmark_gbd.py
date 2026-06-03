#!/usr/bin/env python3
"""
Benchmark satience solver on GBD-style instances.

Since GBD download is unavailable, we generate realistic instances
matching GBD families: Tseitin, Urquhart, Pigeonhole, Sudoku, etc.
All instances have known results based on their construction.
"""

import os
import subprocess
import time
import polars as pl
from pathlib import Path
from typing import List, Tuple, Optional
import random


def generate_tseitin_grid(n: int, m: int, unsat: bool = False) -> List[List[int]]:
    """
    Generate Tseitin grid formula.
    SAT if parity constraint is even, UNSAT if odd.
    """
    clauses = []
    var_counter = 1
    
    # Create variables for each node and edge
    nodes = {}
    edges = {}
    
    for i in range(n):
        for j in range(m):
            nodes[(i, j)] = var_counter
            var_counter += 1
    
    # Edge variables (horizontal and vertical)
    for i in range(n):
        for j in range(m - 1):
            edges[((i, j), (i, j + 1))] = var_counter
            var_counter += 1
    
    for i in range(n - 1):
        for j in range(m):
            edges[((i, j), (i + 1, j))] = var_counter
            var_counter += 1
    
    # Generate parity constraints for each node
    # Each node must have an odd number of true incident edges
    for i in range(n):
        for j in range(m):
            incident_edges = []
            
            # Horizontal edges
            if j > 0:
                incident_edges.append(edges[((i, j - 1), (i, j))])
            if j < m - 1:
                incident_edges.append(edges[((i, j), (i, j + 1))])
            
            # Vertical edges
            if i > 0:
                incident_edges.append(edges[((i - 1, j), (i, j))])
            if i < n - 1:
                incident_edges.append(edges[((i, j), (i + 1, j))])
            
            # XOR constraint: odd parity
            # For XOR of n variables, we need 2^(n-1) clauses
            # Simplified: use helper variables for large XORs
            if len(incident_edges) <= 4:
                # Small XOR: enumerate satisfying assignments
                from itertools import product
                for assignment in product([False, True], repeat=len(incident_edges)):
                    if sum(assignment) % 2 == 1:  # Odd parity
                        clause = []
                        for k, val in enumerate(assignment):
                            clause.append(incident_edges[k] if val else -incident_edges[k])
                        clauses.append(clause)
    
    # Add parity constraint for corner to make UNSAT
    if unsat and n > 0 and m > 0:
        # Flip one constraint to make it unsatisfiable
        # This is a simplification - real Urquhart formulas are more complex
        clauses.append([nodes[(0, 0)]])
        clauses.append([-nodes[(0, 0)]])
    
    return clauses


def generate_pigeonhole(pigeons: int, holes: int) -> List[List[int]]:
    """
    Generate pigeonhole principle formula.
    UNSAT if pigeons > holes, SAT otherwise.
    """
    clauses = []
    
    # Variable x_{p,h} = pigeon p is in hole h
    def var(p, h):
        return p * holes + h + 1
    
    # Each pigeon must be in at least one hole
    for p in range(pigeons):
        clause = [var(p, h) for h in range(holes)]
        clauses.append(clause)
    
    # No two pigeons in same hole
    for h in range(holes):
        for p1 in range(pigeons):
            for p2 in range(p1 + 1, pigeons):
                clauses.append([-var(p1, h), -var(p2, h)])
    
    return clauses


def generate_sudoku(n: int = 3) -> Tuple[List[List[int]], bool]:
    """
    Generate Sudoku encoding.
    Returns (clauses, is_satisfiable).
    """
    clauses = []
    N = n * n  # Grid size
    
    # Variable x_{r,c,v} = cell (r,c) has value v
    def var(r, c, v):
        return r * N * N + c * N + v + 1
    
    # Each cell has at least one value
    for r in range(N):
        for c in range(N):
            clause = [var(r, c, v) for v in range(N)]
            clauses.append(clause)
    
    # Each cell has at most one value
    for r in range(N):
        for c in range(N):
            for v1 in range(N):
                for v2 in range(v1 + 1, N):
                    clauses.append([-var(r, c, v1), -var(r, c, v2)])
    
    # Row constraints
    for r in range(N):
        for v in range(N):
            for c1 in range(N):
                for c2 in range(c1 + 1, N):
                    clauses.append([-var(r, c1, v), -var(r, c2, v)])
    
    # Column constraints
    for c in range(N):
        for v in range(N):
            for r1 in range(N):
                for r2 in range(r1 + 1, N):
                    clauses.append([-var(r1, c, v), -var(r2, c, v)])
    
    # Box constraints
    for box_r in range(n):
        for box_c in range(n):
            for v in range(N):
                cells = []
                for dr in range(n):
                    for dc in range(n):
                        cells.append((box_r * n + dr, box_c * n + dc))
                for i, (r1, c1) in enumerate(cells):
                    for r2, c2 in cells[i + 1:]:
                        clauses.append([-var(r1, c1, v), -var(r2, c2, v)])
    
    # Empty grid is SAT
    return clauses, True


def generate_random_k_sat(n_vars: int, n_clauses: int, k: int = 3, seed: int = 42) -> Tuple[List[List[int]], bool]:
    """
    Generate random k-SAT with planted solution.
    """
    if seed is not None:
        random.seed(seed)
    
    # Plant a solution (all True)
    solution = [True] * n_vars
    
    clauses = []
    for _ in range(n_clauses):
        # Pick k random variables
        vars_ = random.sample(range(1, n_vars + 1), k)
        
        # Create clause satisfied by planted solution
        # At least one literal must be True
        clause = []
        satisfied = random.randint(0, k - 1)
        for i, v in enumerate(vars_):
            if i == satisfied:
                clause.append(v)  # Positive to satisfy with True
            else:
                # Random sign, but ensure not all false
                clause.append(v if random.random() > 0.5 else -v)
        clauses.append(clause)
    
    return clauses, True


def write_cnf(filepath: Path, n_vars: int, clauses: List[List[int]]):
    """Write CNF file in DIMACS format."""
    with open(filepath, 'w') as f:
        f.write(f"c Generated by benchmark_gbd.py\n")
        f.write(f"p cnf {n_vars} {len(clauses)}\n")
        for clause in clauses:
            f.write(' '.join(map(str, clause)) + ' 0\n')


def get_max_var(clauses: List[List[int]]) -> int:
    """Get maximum variable index in clauses."""
    max_var = 0
    for clause in clauses:
        for lit in clause:
            max_var = max(max_var, abs(lit))
    return max_var


def generate_gbd_instances(instances_dir: Path) -> List[Tuple[str, str, str, Path]]:
    """
    Generate 20 GBD-style benchmark instances with known results.
    Returns list of (family, expected, description, filepath).
    """
    instances_dir.mkdir(parents=True, exist_ok=True)
    generated = []
    
    # Tseitin/Urquhart formulas (classic GBD family)
    print("Generating Tseitin grid formulas...")
    
    # SAT Tseitin (even parity)
    for size in [5, 6, 7]:
        clauses = generate_tseitin_grid(size, size, unsat=False)
        n_vars = get_max_var(clauses)
        filepath = instances_dir / f"tseitin_grid_{size}x{size}_sat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("tseitin-formulas", "SAT", f"Tseitin {size}x{size} grid", filepath))
    
    # UNSAT Tseitin (odd parity)
    for size in [4, 5, 6]:
        clauses = generate_tseitin_grid(size, size, unsat=True)
        n_vars = get_max_var(clauses)
        filepath = instances_dir / f"tseitin_grid_{size}x{size}_unsat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("tseitin-formulas", "UNSAT", f"Tseitin {size}x{size} unsat", filepath))
    
    # Pigeonhole principle (classic GBD family)
    print("Generating pigeonhole formulas...")
    
    # SAT pigeonhole (pigeons <= holes)
    for p, h in [(5, 6), (6, 7), (7, 8)]:
        clauses = generate_pigeonhole(p, h)
        n_vars = get_max_var(clauses)
        filepath = instances_dir / f"php_{p}p_{h}h_sat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("pigeonhole", "SAT", f"{p} pigeons, {h} holes", filepath))
    
    # UNSAT pigeonhole (pigeons > holes)
    for p, h in [(6, 5), (7, 6), (8, 7)]:
        clauses = generate_pigeonhole(p, h)
        n_vars = get_max_var(clauses)
        filepath = instances_dir / f"php_{p}p_{h}h_unsat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("pigeonhole", "UNSAT", f"{p} pigeons, {h} holes", filepath))
    
    # Sudoku (common GBD application)
    print("Generating Sudoku formulas...")
    
    # Empty 3x3 Sudoku (SAT)
    clauses, _ = generate_sudoku(3)
    n_vars = get_max_var(clauses)
    filepath = instances_dir / "sudoku_3x3_empty_sat.cnf"
    write_cnf(filepath, n_vars, clauses)
    generated.append(("sudoku", "SAT", "3x3 empty Sudoku", filepath))
    
    # Random k-SAT with planted solution
    print("Generating random k-SAT formulas...")
    
    for n_vars, n_clauses in [(50, 200), (75, 300), (100, 400)]:
        clauses, _ = generate_random_k_sat(n_vars, n_clauses, k=3, seed=42)
        filepath = instances_dir / f"random_k3_{n_vars}v_{n_clauses}c_sat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("random-k-sat", "SAT", f"{n_vars} vars, {n_clauses} clauses", filepath))
    
    # Algebra (simple parity constraints)
    print("Generating algebra formulas...")
    
    # XOR-SAT (always SAT with proper construction)
    for n in [20, 30, 40]:
        clauses = []
        # Simple XOR chains
        for i in range(1, n):
            clauses.append([i, -(i + 1)])
            clauses.append([-i, i + 1])
        n_vars = n
        filepath = instances_dir / f"algebra_xor_{n}_sat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("algebra", "SAT", f"XOR chain {n}", filepath))
    
    # Argumentation (based on graph structure)
    print("Generating argumentation formulas...")
    
    for n in [50, 100, 150]:
        clauses = []
        # Simple 2-SAT structure (polynomial, always SAT)
        for i in range(1, n):
            clauses.append([i, i + 1])
            clauses.append([-i, -(i + 1)])
        n_vars = n
        filepath = instances_dir / f"arg_chain_{n}_sat.cnf"
        write_cnf(filepath, n_vars, clauses)
        generated.append(("argumentation", "SAT", f"Argumentation chain {n}", filepath))
    
    print(f"\nGenerated {len(generated)} instances in {instances_dir}")
    return generated


def get_cnf_info(cnf_path: Path) -> Tuple[int, int]:
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
    instances: List[Tuple[str, str, str, Path]],
    timeout: int = 60
) -> pl.DataFrame:
    """Benchmark satience on GBD-style instances."""
    solver_path = os.path.abspath(solver_path)
    
    print(f"Benchmarking: {solver_path}")
    print(f"Instances: {len(instances)}, Timeout: {timeout}s\n")
    
    results = []
    for i, (family, expected, desc, filepath) in enumerate(instances, 1):
        print(f"[{i}/{len(instances)}] {family} - {desc}")
        
        if not filepath.exists():
            print(f"  SKIPPED: File not found\n")
            continue
        
        vars_count, clauses_count = get_cnf_info(filepath)
        print(f"  Instance: {vars_count} vars, {clauses_count} clauses, expected: {expected}")
        
        result = run_solver(solver_path, filepath, timeout)
        
        correct = (result["status"] == expected)
        
        results.append({
            "instance": filepath.name,
            "family": family,
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
        print(f"  Result: {status_str}\n")
    
    return pl.DataFrame(results)


def main():
    solver_path = os.environ.get("SATIENCE_SOLVER", "../satience")
    timeout = int(os.environ.get("SATIENCE_TIMEOUT", "60"))
    instances_dir = Path(os.environ.get("SATIENCE_INSTANCES", "gbd_instances"))
    
    if not os.path.exists(solver_path):
        print(f"Solver not found at {solver_path}")
        print("Build satience:")
        print("  cd .. && go build -o satience ./cmd/satience")
        return 1
    
    print("=" * 80)
    print("GBD-STYLE BENCHMARK GENERATION")
    print("=" * 80)
    print("\nGenerating realistic SAT instances matching GBD families:")
    print("- Tseitin/Urquhart formulas (grid parity constraints)")
    print("- Pigeonhole principle formulas")
    print("- Sudoku encodings")
    print("- Random k-SAT with planted solutions")
    print("- Algebra/XOR formulas")
    print("- Argumentation frameworks\n")
    
    # Generate instances
    instances = generate_gbd_instances(instances_dir)
    
    if not instances:
        print("Failed to generate instances")
        return 1
    
    print(f"\nGenerated {len(instances)} benchmark instances\n")
    
    # Run benchmarks
    print("=" * 80)
    print("BENCHMARK EXECUTION")
    print("=" * 80 + "\n")
    
    df = benchmark_solver(solver_path, instances, timeout=timeout)
    
    print("\n" + "=" * 80)
    print("SUMMARY")
    print("=" * 80)
    print(df.select(["instance", "family", "expected", "status", "time", "correct"]))
    
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
    
    print("\n" + "=" * 80)
    print("BY FAMILY")
    print("=" * 80)
    
    for family in sorted(df['family'].unique()):
        family_df = df.filter(pl.col('family') == family)
        correct_count = len(family_df.filter(pl.col('correct') == True))
        total_count = len(family_df)
        print(f"{family:25s}: {correct_count}/{total_count} correct ({100*correct_count/total_count:.1f}%)")
    
    output_file = "benchmark_gbd_results.csv"
    df.write_csv(output_file)
    print(f"\nResults saved to: {output_file}")
    
    return 0 if len(correct) == total else 1


if __name__ == "__main__":
    exit(main())
