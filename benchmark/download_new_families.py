#!/usr/bin/env python3
"""Download new benchmark instances from diverse families for performance analysis."""

import subprocess
import sys
import sqlite3
from pathlib import Path

DB_PATH = Path('/home/luca/git/opencode-sat-new/benchmark/meta.db')
OUTPUT_DIR = Path('/home/luca/git/opencode-sat-new/benchmark/gbd_instances')
BASE_URL = 'https://benchmark-database.de/file'

def get_instances(family, count=5, max_vars=500):
    """Get instances from a specific family."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    
    # Get instances with known results
    c.execute("""
        SELECT hash, result FROM features 
        WHERE family=? AND result IN ('sat', 'unsat')
        LIMIT ?
    """, (family, count))
    
    instances = c.fetchall()
    conn.close()
    return instances

def download(hash_val):
    """Download instance from GBD."""
    url = f"{BASE_URL}/{hash_val}"
    output = OUTPUT_DIR / f"{hash_val}.cnf.xz"
    
    if output.exists():
        print(f"  Skipping {hash_val[:16]}... (already exists)")
        return True
    
    try:
        subprocess.run(['wget', '-q', '-O', str(output), url], timeout=30, check=True)
        subprocess.run(['unxz', '-f', str(output)], timeout=30, check=True, capture_output=True)
        print(f"  Downloaded {hash_val[:16]}...")
        return True
    except Exception as e:
        print(f"  Failed {hash_val[:16]}...: {e}")
        if output.exists():
            output.unlink()
        return False

def main():
    # Families to explore
    families = [
        'hardware-verification',
        'cryptography',
        'planning',
        'subgraph-isomorphism',
        'bitvector',
        'coloring',
        'quasigroup-completion',
        'antibandwidth',
        'diagnosis',
        'scheduling',
    ]
    
    print("Downloading new benchmark instances from diverse families...")
    print("="*70)
    
    downloaded = 0
    for family in families:
        print(f"\nFamily: {family}")
        instances = get_instances(family, count=5)
        
        for hash_val, result in instances:
            if download(hash_val):
                downloaded += 1
    
    print("\n" + "="*70)
    print(f"Downloaded {downloaded} new instances")

if __name__ == '__main__':
    main()
