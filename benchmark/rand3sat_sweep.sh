#!/bin/bash
# Focused parameter sweep on randkcnf 3 to find the cleanest breaking point.
#
# Axes:
#   - Size at density 4.26 (phase transition): N = 200, 225, 250, 275
#   - Density at N=250: 4.0, 4.2, 4.26, 4.4, 4.5 (D2 gate), 4.8, 5.0
#   - Seeds at N=250, density 4.26: 1, 2, 3, 42, 100 (reproducibility)
#
# Usage: bash benchmark/rand3sat_sweep.sh [timeout_sec]

set -u
TIMEOUT_SEC=${1:-30}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"
JOBS=${JOBS:-6}

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# label|seed|N|clauses
INSTANCES=(
    # === Size axis at density 4.26 (seed 42) ===
    "size_n200_d4.26|42|200|852"
    "size_n225_d4.26|42|225|958"
    "size_n250_d4.26|42|250|1065"
    "size_n275_d4.26|42|275|1171"
    # === Density axis at N=250 (seed 42) ===
    "dens_n250_d4.0|42|250|1000"
    "dens_n250_d4.2|42|250|1050"
    "dens_n250_d4.4|42|250|1100"
    "dens_n250_d4.5|42|250|1125"
    "dens_n250_d4.8|42|250|1200"
    "dens_n250_d5.0|42|250|1250"
    # === Seed axis at N=250, density 4.26 ===
    "seed1_n250_d4.26|1|250|1065"
    "seed2_n250_d4.26|2|250|1065"
    "seed3_n250_d4.26|3|250|1065"
    "seed100_n250_d4.26|100|250|1065"
)

run_one() {
    local label=$1 seed=$2 n=$3 clauses=$4
    local cnf_file="$WORK_DIR/${label}.cnf"
    cnfgen -S "$seed" randkcnf 3 "$n" "$clauses" > "$cnf_file" 2>/dev/null
    if [ ! -s "$cnf_file" ]; then
        echo "$label|GEN_FAIL|$seed|$n|$clauses||||"
        return
    fi

    local t0 t1 sat_ec sat_dur mini_ec mini_dur
    t0=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$SATIENCE" "$cnf_file" > /dev/null 2>&1
    sat_ec=$?
    t1=$(date +%s.%N)
    sat_dur=$(echo "$t1 - $t0" | bc -l)

    t0=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" minisat "$cnf_file" /dev/null > /dev/null 2>&1
    mini_ec=$?
    t1=$(date +%s.%N)
    mini_dur=$(echo "$t1 - $t0" | bc -l)

    echo "$label|$seed|$n|$clauses|$sat_ec|$sat_dur|$mini_ec|$mini_dur"
}

export -f run_one
export TIMEOUT_SEC SATIENCE WORK_DIR

# Run in parallel
printf '%s\n' "${INSTANCES[@]}" | xargs -P "$JOBS" -I {} bash -c '
    IFS="|" read -r label seed n clauses <<< "{}"
    run_one "$label" "$seed" "$n" "$clauses"
' > "$WORK_DIR/results.txt" 2>/dev/null

# Sort and display
echo "=== randkcnf 3 parameter sweep (timeout ${TIMEOUT_SEC}s) ==="
echo ""
printf "%-26s %5s %5s %7s %7s %7s %7s %7s\n" "label" "seed" "N" "clauses" "sat_ec" "sat_s" "mini_ec" "mini_s"
echo "--------------------------------------------------------------------------------------------"

sort -t'|' -k1 "$WORK_DIR/results.txt" | while IFS='|' read -r label seed n clauses sat_ec sat_dur mini_ec mini_dur; do
    printf "%-26s %5s %5s %7s %7s %7s %7s %7s\n" "$label" "$seed" "$n" "$clauses" "$sat_ec" "$(printf '%.3f' "$sat_dur")" "$mini_ec" "$(printf '%.3f' "$mini_dur")"
done

echo ""
echo "Exit codes: 10=SAT, 20=UNSAT, 124=TMO"
echo ""

# Summary
echo "=== Breaking points (satience=TMO AND minisat<5s) ==="
while IFS='|' read -r label seed n clauses sat_ec sat_dur mini_ec mini_dur; do
    if [ "$sat_ec" = "124" ] && [ "$mini_ec" != "124" ]; then
        mini_s=$(printf '%.3f' "$mini_dur")
        echo "  $label (N=$n, clauses=$clauses, seed=$seed): sat TMO, mini solved in ${mini_s}s"
    fi
done < "$WORK_DIR/results.txt"
