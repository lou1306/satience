#!/usr/bin/env python3
"""
Download GBD instances that are not available locally.

Usage:
    python download_instances.py [n_instances] [max_vars]
    
    n_instances: Number of instances to download (default: 20)
    max_vars: Maximum number of variables (default: 200)
    
Examples:
    python download_instances.py 20 200  # Download 20 instances with <= 200 vars
    python download_instances.py 50 100  # Download 50 instances with <= 100 vars
"""

import os
import sys
import sqlite3
import requests
import lzma
from pathlib import Path

# Configuration
DB_PATH = Path(__file__).parent / "meta.db"
INSTANCES_DIR = Path(__file__).parent / "gbd_instances"
GBD_BASE_URL = "https://benchmark-database.de/file/"

def ensure_dirs():
    """Ensure instances directory exists."""
    INSTANCES_DIR.mkdir(exist_ok=True)

def get_local_hashes():
    """Get set of already downloaded instance hashes."""
    if not INSTANCES_DIR.exists():
        return set()
    
    local = set()
    for f in INSTANCES_DIR.glob("*.cnf"):
        # Extract hash from filename (everything before .cnf)
        local.add(f.stem)
    return local

def query_instances(n_instances, max_vars):
    """
    Query database for random instances with known results from families likely to have small instances.
    
    Returns list of (hash, result) tuples for instances not yet downloaded.
    Variable count is checked after download.
    """
    conn = sqlite3.connect(DB_PATH)
    cursor = conn.cursor()
    
    # Families more likely to have small instances
    small_families = [
        'tseitin-formulas',
        'algebra',
        'uniform-random',
        'random-planted-solution',
        'random',
        'pigeonhole',
        'binary-pigeon-hole',
        'clique-formulas',
        'coloring',
        'sudoku'
    ]
    
    # Build placeholders for SQL query
    placeholders = ','.join('?' * len(small_families))
    
    # Get instances from small families first
    cursor.execute(f"""
        SELECT hash, result
        FROM features 
        WHERE result IN ('sat', 'unsat')
        AND family IN ({placeholders})
        ORDER BY RANDOM()
        LIMIT ?
    """, small_families + [n_instances * 3])
    
    results = []
    local_hashes = get_local_hashes()
    
    for row in cursor.fetchall():
        hash_val, result = row
        
        # Skip if already downloaded
        if hash_val in local_hashes:
            continue
        
        results.append((hash_val, result))
        
        if len(results) >= n_instances:
            break
    
    # If we still need more, get from any family
    if len(results) < n_instances:
        cursor.execute("""
            SELECT hash, result
            FROM features 
            WHERE result IN ('sat', 'unsat')
            AND family NOT IN ({})
            ORDER BY RANDOM()
            LIMIT ?
        """.format(placeholders), small_families + [n_instances * 5 - len(results) * 3])
        
        for row in cursor.fetchall():
            if len(results) >= n_instances:
                break
                
            hash_val, result = row
            
            if hash_val in local_hashes:
                continue
            
            results.append((hash_val, result))
    
    conn.close()
    return results

def download_instance(hash_val):
    """
    Download and decompress a single instance from GBD.
    
    Returns the path to the downloaded CNF file, or None if failed.
    """
    url = f"{GBD_BASE_URL}{hash_val}"
    cnf_path = INSTANCES_DIR / f"{hash_val}.cnf"
    
    # Skip if already exists
    if cnf_path.exists():
        print(f"  Already exists: {cnf_path.name}")
        return cnf_path
    
    print(f"  Downloading from {url}...")
    
    try:
        response = requests.get(url, timeout=30)
        response.raise_for_status()
        
        # Check if it's xz-compressed
        if response.content[:6] == b'\xfd7zXZ\x00':
            print(f"  Decompressing xz...")
            decompressed = lzma.decompress(response.content)
            cnf_path.write_bytes(decompressed)
        else:
            # Not compressed, save as-is
            cnf_path.write_bytes(response.content)
        
        print(f"  Saved to: {cnf_path.name}")
        return cnf_path
        
    except requests.exceptions.RequestException as e:
        print(f"  ERROR: Failed to download: {e}")
        return None
    except Exception as e:
        print(f"  ERROR: Failed to process: {e}")
        return None

def count_vars(cnf_path):
    """Count variables in a CNF file."""
    with open(cnf_path, 'r', errors='ignore') as f:
        for line in f:
            if line.startswith('p cnf'):
                parts = line.split()
                if len(parts) >= 3:
                    return int(parts[2])
    return None

def main():
    n_instances = int(sys.argv[1]) if len(sys.argv) > 1 else 20
    max_vars = int(sys.argv[2]) if len(sys.argv) > 2 else 200
    
    print("=" * 60)
    print(f"Downloading {n_instances} GBD instances (max {max_vars} variables)")
    print("=" * 60)
    print()
    
    ensure_dirs()
    
    # Query for instances to download
    instances = query_instances(n_instances, max_vars)
    
    if not instances:
        print("No instances found to download!")
        print("Make sure meta.db has instances with result='sat' or 'unsat'")
        return
    
    print(f"Found {len(instances)} candidate instances")
    print()
    
    # Download instances
    downloaded = 0
    skipped = 0
    failed = 0
    
    for hash_val, expected_result in instances:
        if downloaded >= n_instances:
            break
        
        print(f"[{downloaded + 1}] {hash_val} (expected: {expected_result})")
        
        cnf_path = download_instance(hash_val)
        
        if cnf_path is None:
            failed += 1
            print()
            continue
        
        # Check number of variables
        n_vars = count_vars(cnf_path)
        
        if n_vars is None:
            print(f"  WARNING: Could not determine variable count")
            # Keep it anyway
        
        if n_vars and n_vars > max_vars:
            print(f"  SKIPPED: Too many variables ({n_vars} > {max_vars})")
            # Remove the file
            cnf_path.unlink()
            skipped += 1
            print()
            continue
        
        if n_vars:
            print(f"  Variables: {n_vars}")
        
        downloaded += 1
        print()
    
    print("=" * 60)
    print("SUMMARY")
    print("=" * 60)
    print(f"Downloaded: {downloaded}")
    print(f"Skipped (too large): {skipped}")
    print(f"Failed: {failed}")
    print()
    
    if downloaded > 0:
        print(f"Successfully downloaded {downloaded} instances to {INSTANCES_DIR}")
    else:
        print("No instances downloaded!")

if __name__ == "__main__":
    main()
