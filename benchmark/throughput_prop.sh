#!/bin/bash
# throughput_prop.sh — Per-propagation throughput ceiling harness.
#
# Measures props/sec for satience vs a reference CDCL on the SAME CNFs, then
# reports the per-propagation cost ratio. Used to separate (A) a Go/per-prop
# representation ceiling from (B) algorithmic excess (doing more/heavier slow
# path work per solve). On a pure binary implication chain both solvers do
# IDENTICAL propagation work, so the props/sec ratio there is the cleanest
# raw per-prop throughput comparison.
#
# USAGE: CONTROL-BINARY is satience under test; MINISAT is the reference.
#   CONTROL_BINARY=/path/to/satience MINISAT=/path/to/minisat ./throughput_prop.sh <cnf>...
#
# Env:
#   CONTROL_BINARY   satience binary (default ./satience_bench)
#   MINISAT          reference CDCL (default /home/luca/bin/minisat)
#   TIMEOUT          per-solver seconds (default 60)
# It appends a line:  <cnf>  sat_props  sat_time  sat_pps  msat_props  msat_time  msat_pps  ratio
# where ratio = sat_pps / msat_pps  (>1 satience faster per propagation).
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTROL_BINARY="${CONTROL_BINARY:-$ROOT/satience_bench}"
MINISAT="${MINISAT:-/home/luca/bin/minisat}"
TIMEOUT="${TIMEOUT:-60}"

# satience: parse [final] line for props= and t=
sat_pps() { # $1 cnf
    local out
    out=$(timeout "$TIMEOUT" "$CONTROL_BINARY" -stats 1000000000 -no-preprocess -parity=false -no-classify "$1" 2>&1 >/dev/null)
    local props time
    props=$(echo "$out" | grep -oE 'props=[0-9]+' | tail -1 | cut -d= -f2)
    time=$(echo "$out" | grep -oE '\[final\] t=[0-9.]+' | tail -1 | sed 's/.*t=//')
    [ -z "$props" ] && [ -z "$time" ] && { echo "0 0 0"; return; }
    [ -z "$props" ] && props=0
    [ -z "$time" ] && time=0
    local pps=0
    if [ "$time" != "0" ] && awk "BEGIN{exit !($time>0)}"; then
        pps=$(awk -v p="$props" -v t="$time" 'BEGIN{printf "%.0f", p/t}')
    fi
    echo "$props $time $pps"
}

# minisat: parse "propagations : N (X /sec)" and "CPU time : T"
msat_pps() { # $1 cnf
    local out
    out=$(timeout "$TIMEOUT" "$MINISAT" "$1" 2>&1)
    local props=0 time=0
    local l
    l=$(echo "$out" | grep -iE 'propagations' | head -1)
    props=$(echo "$l" | awk '{print $3}')
    l=$(echo "$out" | grep -iE 'CPU time' | head -1)
    time=$(echo "$l" | awk '{print $4}')
    local pps=0
    if awk "BEGIN{exit !($time>0)}"; then
        pps=$(awk -v p="${props:-0}" -v t="$time" 'BEGIN{printf "%.0f", p/t}')
    fi
    echo "${props:-0} $time $pps"
}

echo "sat_props sat_time sat_pps msat_props msat_time msat_pps  sat/msat_props_per_sec"
for f in "$@"; do
    read -r sp st spp <<< "$(sat_pps "$f")"
    read -r mp mt mpp <<< "$(msat_pps "$f")"
    ratio=$(awk -v a="$spp" -v b="$mpp" 'BEGIN{ if(b>0 && a>0) printf "%.3f", a/b; else print "-" }')
    printf "%-24s %9s %7.3f %10s %9s %7.3f %10s   %s\n" \
        "$(basename "$f")" "$sp" "$st" "$spp" "$mp" "$mt" "$mpp" "$ratio"
done
