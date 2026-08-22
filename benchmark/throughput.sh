#!/usr/bin/env bash
# throughput.sh — engine-speed measurement rail.
#
# The fixed 72-instance PAR2 suite is trajectory-hypersensitive: it rejects even
# sound, results-identical engine changes (they reorder internal propagation
# events and flip the fragile instances to timeout). This harness instead
# measures steady-state WATCHED-LITERAL THROUGHPUT (propagations/sec), which is
# the true engine metric and is robust to event reordering.
#
# Usage:
#   BINARY=... ./throughput.sh            # satience props/sec over broadened set
#   BINARY=... ./throughput.sh --minisat  # reference minisat props/sec (same set)
#
# Env:
#   SAMPLE_DIR   dir of .cnf instances (default ../benchmark/gbd_instances)
#   MIN_VARS     lower var bound for sampled instances (default 10000)
#   MAX_VARS     upper var bound (default 400000)
#   N            max instances to sample (default 12)
#   TIMEOUT      per-instance timeout s (default 25)
#   STATS        periodic stats interval (default 200000 conflicts)
#
# Output: "<props/sec> <instance>" per row + throughput-column summary.

set -u
BINARY="${BINARY:-../satience_bench}"
SAMPLE_DIR="${SAMPLE_DIR:-../gbd_instances}"
MIN_VARS="${MIN_VARS:-10000}"
MAX_VARS="${MAX_VARS:-400000}"
N="${N:-12}"
TIMEOUT="${TIMEOUT:-25}"
STATS="${STATS:-200000}"
MODE_MINISAT="${1:-}"

[ -x "$BINARY" ] || BINARY=$(command -v "$BINARY" || echo "$BINARY")
[ -d "$SAMPLE_DIR" ] || { echo "no dir $SAMPLE_DIR" >&2; exit 1; }

# Collect a broadened mid/large instance sample by header var count.
tmp=$(mktemp)
n=0
for f in "$SAMPLE_DIR"/*.cnf; do
    read -r p c nv nc < "$f" 2>/dev/null
    [ "$p" = "p" ] && [ "$c" = "cnf" ] || continue
    if [ "$nv" -ge "$MIN_VARS" ] && [ "$nv" -le "$MAX_VARS" ]; then
        printf "%s %s\n" "$nv" "$(basename "$f")" >> "$tmp"
    fi
done
sort -rn "$tmp" | head -n "$N" | while read -r nv name; do
    file="$SAMPLE_DIR/$name"
    if [ -n "$MODE_MINISAT" ]; then
        # minisat prints its own summary: pick propagations + CPU time.
        out=$(timeout "$TIMEOUT" minisat "$file" 2>&1)
        props=$(printf '%s\n' "$out" | rg -o 'propagations *: *[0-9]+' | tail -1 | rg -o '[0-9]+')
        cpu=$(printf '%s\n' "$out" | rg -o 'CPU time *: *[0-9.]+' | tail -1 | rg -o '[0-9.]+')
        if [ -z "$props" ] || [ -z "$cpu" ] || [ "$(printf '%s\n' "$out" | rg -c 'UNSATISFIABLE|SATISFIABLE')" -eq 0 ]; then
            echo "TMO ${name}(${nv}v)"
            continue
        fi
        pps=$(awk -v p="$props" -v c="$cpu" 'BEGIN{printf "%.0f", p/c}')
        echo "${pps} ${name}(${nv}v)"
    else
        out=$(timeout "$TIMEOUT" "$BINARY" -stats="$STATS" "$file" 2>&1 | rg -o 't=[0-9.]+s [^|]* props=[0-9]+' | tail -1)
        [ -n "$out" ] || { echo "TMO ${name}(${nv}v)"; continue; }
        t=$(printf '%s\n' "$out" | rg -o 't=[0-9.]+s' | rg -o '[0-9.]+')
        props=$(printf '%s\n' "$out" | rg -o 'props=[0-9]+' | rg -o '[0-9]+')
        pps=$(awk -v p="$props" -v t="$t" 'BEGIN{printf "%.0f", p/t}')
        echo "${pps} ${name}(${nv}v)"
    fi
done
rm -f "$tmp"
