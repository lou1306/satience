#!/usr/bin/env python3
"""Generate SAT benchmark instances with known solutions."""

import os

def format_cnf(n_vars: int, clauses: list[list[int]]) -> str:
    """Format as DIMACS CNF."""
    lines = [f"c Generated CNF formula", f"p cnf {n_vars} {len(clauses)}"]
    for clause in clauses:
        lines.append(" ".join(map(str, clause)) + " 0")
    return "\n".join(lines) + "\n"


def generate_trivial_sat(n_vars: int, n_clauses: int) -> str:
    """
    Generate a trivially satisfiable instance.
    All clauses are satisfied by setting all variables to True.
    """
    clauses = []
    for _ in range(n_clauses):
        # Each clause has at least one positive literal
        clause = [1]  # x1 is always true
        # Add 2 more random literals
        import random
        for _ in range(2):
            var = random.randint(1, n_vars)
            sign = random.choice([1, -1])
            if var not in clause and -var not in clause:
                clause.append(sign * var)
        clauses.append(clause)
    
    return format_cnf(n_vars, clauses)


def generate_trivial_unsat(n_vars: int) -> str:
    """
    Generate a trivially unsatisfiable instance.
    Contains both x1 and -x1 as unit clauses.
    """
    clauses = [[1], [-1]]  # x1 AND NOT x1 = contradiction
    return format_cnf(n_vars, clauses)


def generate_pigeonhole_sat(n_holes: int) -> str:
    """
    Generate satisfiable pigeonhole: n pigeons into n+1 holes.
    This is SAT - we can fit n pigeons into n+1 holes.
    """
    n_pigeons = n_holes
    n_holes_actual = n_holes + 1
    
    def var(p, h):
        return (p - 1) * n_holes_actual + h
    
    clauses = []
    
    # Each pigeon must be in at least one hole
    for p in range(1, n_pigeons + 1):
        clause = [var(p, h) for h in range(1, n_holes_actual + 1)]
        clauses.append(clause)
    
    # No two pigeons in the same hole
    for h in range(1, n_holes_actual + 1):
        for p1 in range(1, n_pigeons):
            for p2 in range(p1 + 1, n_pigeons + 1):
                clauses.append([-var(p1, h), -var(p2, h)])
    
    n_vars = n_pigeons * n_holes_actual
    return format_cnf(n_vars, clauses)


def generate_pigeonhole_unsat(n_holes: int) -> str:
    """
    Generate unsatisfiable pigeonhole: n+1 pigeons into n holes.
    This is UNSAT - cannot fit n+1 pigeons into n holes.
    """
    n_pigeons = n_holes + 1
    n_holes_actual = n_holes
    
    def var(p, h):
        return (p - 1) * n_holes_actual + h
    
    clauses = []
    
    # Each pigeon must be in at least one hole
    for p in range(1, n_pigeons + 1):
        clause = [var(p, h) for h in range(1, n_holes_actual + 1)]
        clauses.append(clause)
    
    # No two pigeons in the same hole
    for h in range(1, n_holes_actual + 1):
        for p1 in range(1, n_pigeons):
            for p2 in range(p1 + 1, n_pigeons + 1):
                clauses.append([-var(p1, h), -var(p2, h)])
    
    n_vars = n_pigeons * n_holes_actual
    return format_cnf(n_vars, clauses)


def generate_dubois_sat(n: int) -> str:
    """
    Generate a simple SAT instance (not the actual Dubois counter).
    Just n variables with no constraints - trivially SAT.
    """
    clauses = []
    # Add some random 3-clauses that are all satisfied by all-true assignment
    import random
    random.seed(42)
    for _ in range(n):
        clause = []
        for _ in range(3):
            var = random.randint(1, n)
            # Make sure at least one literal is positive
            if len(clause) == 0:
                clause.append(var)
            else:
                sign = random.choice([1, -1])
                clause.append(sign * var)
        clauses.append(clause)
    
    return format_cnf(n, clauses)


def main():
    os.makedirs("instances", exist_ok=True)
    
    # Generate SAT instances
    print("Generating SAT instances...")
    
    # Trivial SAT instances of increasing size
    for n_vars in [20, 30, 40, 50, 60, 70, 80, 90, 100, 150]:
        n_clauses = n_vars * 4
        cnf = generate_trivial_sat(n_vars, n_clauses)
        filename = f"instances/trivial_sat_{n_vars}_{n_clauses}.cnf"
        with open(filename, 'w') as f:
            f.write(cnf)
        print(f"  Created {filename}: {n_vars} vars, {n_clauses} clauses (SAT)")
    
    # Satisfiable pigeonhole instances
    print("\nGenerating satisfiable pigeonhole instances...")
    for n in range(5, 11):
        cnf = generate_pigeonhole_sat(n)
        n_pigeons = n
        n_holes = n + 1
        n_vars = n_pigeons * n_holes
        filename = f"instances/php_sat_{n}p_{n+1}h.cnf"
        with open(filename, 'w') as f:
            f.write(cnf)
        print(f"  Created {filename}: {n_vars} vars ({n} pigeons, {n+1} holes, SAT)")
    
    # Generate UNSAT instances
    print("\nGenerating UNSAT instances...")
    
    # Trivial UNSAT
    cnf = generate_trivial_unsat(10)
    filename = "instances/trivial_unsat.cnf"
    with open(filename, 'w') as f:
        f.write(cnf)
    print(f"  Created {filename}: 10 vars, 2 clauses (UNSAT)")
    
    # Unsatisfiable pigeonhole instances
    print("\nGenerating unsatisfiable pigeonhole instances...")
    for n in range(5, 11):
        cnf = generate_pigeonhole_unsat(n)
        n_pigeons = n + 1
        n_holes = n
        n_vars = n_pigeons * n_holes
        n_clauses = n_pigeons + n_holes * n_pigeons * (n_pigeons - 1) // 2
        filename = f"instances/php_unsat_{n+1}p_{n}h.cnf"
        with open(filename, 'w') as f:
            f.write(cnf)
        print(f"  Created {filename}: {n_vars} vars, ~{n_clauses} clauses ({n+1} pigeons, {n} holes, UNSAT)")
    
    # Simple Dubois-like SAT instances
    print("\nGenerating simple SAT instances...")
    for n in [20, 30, 40, 50, 60, 70]:
        cnf = generate_dubois_sat(n)
        filename = f"instances/simple_sat_{n}.cnf"
        with open(filename, 'w') as f:
            f.write(cnf)
        print(f"  Created {filename}: {n} vars, {n} clauses (SAT)")
    
    print("\nDone! Created instances in ./instances/")


if __name__ == "__main__":
    main()
