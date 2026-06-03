#!/bin/bash
# Run satience benchmark
# Usage: ./run-bench.sh [path/to/satience] [path/to/meta.db]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOLVER="${1:-$SCRIPT_DIR/../satience}"
GBD_DB="${2:-$SCRIPT_DIR/meta.db}"

export SATIENCE_SOLVER="$SOLVER"
export GBD_DB="$GBD_DB"

cd "$SCRIPT_DIR"
uv run python benchmark.py
