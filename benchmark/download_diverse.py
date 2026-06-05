#!/usr/bin/env python3
"""Download diverse benchmark instances from GBD across different families and sizes."""

import sqlite3
import subprocess
import sys
import os
from pathlib import Path

def get_diverse_instances(n_per_family=3, max_vars=500):
    """Get diverse instances from different families with varying sizes."""
    conn = sqlite3.connect('meta.db')
    c = conn.cursor()
    
    # Families to target (mix of SAT and UNSAT, different structures)
    target_families = [
        'bounded-model-checking',
        'circuit-multiplier',
        'circuit-equivalence-checking',
        'cardinality-constraints',
        'algebra',
        'tseitin',
        'random',
        'planning',
        'bioinformatics',
        'clique-formulas',
        'automata-synchronization',
        'bitvector',
    ]
    
    instances = []
    for family in target_families:
        # Get SAT and UNSAT instances of varying sizes
        c.execute("""
            SELECT hash, result, n_vars, n_clauses
            FROM features
            WHERE family = ? AND n_vars <= ? AND n_vars > 0
            ORDER BY n_vars
            LIMIT ?
        """, (family, max_vars, n_per_family))
        
        for row in c.fetchall():
            hash_val, result, n_vars, n_clauses = row
            instances.append((hash_val, family, result, n_vars, n_clauses))
    
    conn.close()
    return instances

def download_instance(hash_val, output_dir):
    """Download a single instance from GBD."""
    url = f"https://benchmark-database.de/file/{hash_val}"
    output_path = output_dir / f"{hash_val}.cnf.xz"
    
    if output_path.exists():
        return True
    
    try:
        subprocess.run(
            ['wget', '-q', '-O', str(output_path), url],
            timeout=30,
            check=True
        )
        return True
    except Exception as e:
        print(f"Failed to download {hash_val}: {e}")
        return False

def main():
    output_dir = Path('gbd_instances')
    output_dir.mkdir(exist_ok=True)
    
    print("Fetching diverse instances from GBD...")
    instances = get_diverse_instances(n_per_family=5, max_vars=500)
    print(f"Found {len(instances)} candidate instances")
    
    downloaded = 0
    for hash_val, family, result, n_vars, n_clauses in instances:
        if download_instance(hash_val, output_dir):
            print(f"✓ {hash_val[:8]}: {family}, {result}, {n_vars} vars, {n_clauses} clauses")
            downloaded += 1
    
    print(f"\nDownloaded {downloaded} instances")

if __name__ == '__main__':
    main()
