#!/usr/bin/env python3
"""Test GBD database query."""

import os
from gbd_core.api import GBD

gbd_db_path = os.path.abspath("meta.db")

print(f"Querying GBD database: {gbd_db_path}\n")

with GBD([gbd_db_path]) as gbd:
    df = gbd.query(
        "result=sat",
        resolve=["result", "family"]
    )
    
    print(f"Found {len(df)} satisfiable SAT instances")
    print("\nFirst 10 instances:")
    print(df.head(10))
