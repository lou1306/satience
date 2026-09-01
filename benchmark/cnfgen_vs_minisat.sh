#!/bin/bash
# CNFgen vs minisat comparison harness for satience.
#
# Generates CNF instances across many formula families and sizes, times both
# satience and minisat on each, and highlights instances where satience is
# slow but minisat is fast. Used to find search-quality weaknesses.
#
# Usage: bash benchmark/cnfgen_vs_minisat.sh [timeout_sec]
#   timeout_sec  per-instance per-solver timeout (default 10)
#   JOBS env var controls parallelism (default 6)
#
# Requires: cnfgen, minisat, GOAMD64=v3 go build for satience.

set -u

TIMEOUT_SEC=${1:-10}
JOBS=${JOBS:-6}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"

if ! command -v cnfgen >/dev/null 2>&1; then
    echo "ERROR: cnfgen not found. Install with: pipx install cnfgen" >&2
    exit 1
fi
if ! command -v minisat >/dev/null 2>&1; then
    echo "ERROR: minisat not found." >&2; exit 1
fi

if [ ! -x "$SATIENCE" ]; then
    echo "Building satience_bench..."
    (cd "$REPO_ROOT" && GOAMD64=v3 go build -o satience_bench ./cmd/satience) || {
        echo "ERROR: build failed" >&2; exit 1
    }
fi

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# Instance suite: "label|cnfgen args..."
# Uses for reproducibility. Sizes go larger than the soundness suite
# to actually find hardness. Focus on families where CDCL search quality matters.
INSTANCES=(
    # === Random 3-SAT at phase transition (ratio ~4.26) ===
    "rand3sat_50|randkcnf 3 50 213"
    "rand3sat_75|randkcnf 3 75 320"
    "rand3sat_100|randkcnf 3 100 426"
    "rand3sat_125|randkcnf 3 125 533"
    "rand3sat_150|randkcnf 3 150 639"
    "rand3sat_175|randkcnf 3 175 746"
    "rand3sat_200|randkcnf 3 200 852"
    "rand3sat_250|randkcnf 3 250 1065"
    "rand3sat_300|randkcnf 3 300 1278"

    # === Random 3-SAT below threshold (SAT, ratio ~3.5) ===
    "rand3sat_lo100|randkcnf 3 100 350"
    "rand3sat_lo150|randkcnf 3 150 525"
    "rand3sat_lo200|randkcnf 3 200 700"

    # === Random 3-SAT above threshold (UNSAT, ratio ~4.8) ===
    "rand3sat_hi100|randkcnf 3 100 480"
    "rand3sat_hi150|randkcnf 3 150 720"
    "rand3sat_hi200|randkcnf 3 200 960"

    # === Random 4-SAT at threshold (ratio ~9.8) ===
    "rand4sat_50|randkcnf 4 50 490"
    "rand4sat_75|randkcnf 4 75 735"
    "rand4sat_100|randkcnf 4 100 980"

    # === Pigeonhole (UNSAT) ===
    "php_8_7|php 8 7"
    "php_10_9|php 10 9"
    "php_12_11|php 12 11"
    "php_15_14|php 15 14"
    "php_func_8_7|php --functional 8 7"
    "php_func_10_9|php --functional 10 9"
    "php_onto_8_7|php --onto 8 7"
    "php_onto_10_9|php --onto 10 9"

    # === Binary pigeonhole (UNSAT) ===
    "bphp_6_5|bphp 6 5"
    "bphp_8_7|bphp 8 7"
    "bphp_10_9|bphp 10 9"
    "bphp_12_11|bphp 12 11"

    # === Tseitin (UNSAT) ===
    "tseitin_30|tseitin 30"
    "tseitin_50|tseitin 50"
    "tseitin_80|tseitin 80"
    "tseitin_100|tseitin 100"
    "tseitin_grid_6x6|tseitin randomodd grid 6 6"
    "tseitin_grid_7x7|tseitin randomodd grid 7 7"
    "tseitin_grid_8x8|tseitin randomodd grid 8 8"
    "tseitin_grid_10x10|tseitin randomodd grid 10 10"
    "tseitin_6reg_50|tseitin randomodd gnd 50 6"

    # === Ordering principle (UNSAT) ===
    "op_8|op 8"
    "op_10|op 10"
    "op_12|op 12"
    "op_15|op 15"
    "op_18|op 18"
    "op_20|op 20"

    # === Counting principle (UNSAT) ===
    "count_10_3|count 10 3"
    "count_13_3|count 13 3"
    "count_16_3|count 16 3"
    "count_20_3|count 20 3"
    "count_14_4|count 14 4"
    "count_17_4|count 17 4"

    # === Parity (UNSAT odd / SAT even) ===
    "parity_9|parity 9"
    "parity_11|parity 11"
    "parity_13|parity 13"
    "parity_15|parity 15"
    "parity_17|parity 17"
    "parity_20|parity 20"

    # === Pebbling (UNSAT) ===
    "peb_pyr_6|peb pyramid 6"
    "peb_pyr_8|peb pyramid 8"
    "peb_pyr_10|peb pyramid 10"
    "peb_tree_8|peb tree 8"
    "peb_tree_10|peb tree 10"
    "peb_tree_12|peb tree 12"
    "peb_path_20|peb path 20"
    "peb_path_30|peb path 30"
    "peb_path_40|peb path 40"

    # === Stone formula (UNSAT) ===
    "stone_3_pyr5|stone 3 pyramid 5"
    "stone_3_pyr6|stone 3 pyramid 6"
    "stone_4_pyr5|stone 4 pyramid 5"
    "stone_3_tree8|stone 3 tree 8"
    "stone_3_tree10|stone 3 tree 10"

    # === k-coloring ===
    "kcolor3_gnp20|kcolor 3 gnp 20 0.5"
    "kcolor3_gnp30|kcolor 3 gnp 30 0.5"
    "kcolor3_grid6x6|kcolor 3 grid 6 6"
    "kcolor3_grid7x7|kcolor 3 grid 7 7"

    # === k-clique ===
    "kclique5_gnp20|kclique 5 gnp 20 0.3"
    "kclique5_gnp30|kclique 5 gnp 30 0.3"
    "kclique6_gnp30|kclique 6 gnp 30 0.3"
)

TOTAL=${#INSTANCES[@]}

# --- Worker ---
run_instance() {
    local idx="$1" entry="$2"
    local label="${entry%%|*}"
    local cnf_args="${entry#*|}"
    local cnf_file="$WORK_DIR/inst_$idx.cnf"
    local res_file="$WORK_DIR/res_$idx"

    if ! cnfgen -S 42 $cnf_args > "$cnf_file" 2>/dev/null; then
        printf 'GENFAIL|%s|0|0|0|0|0|0\n' "$label" > "$res_file"
        return
    fi

    local n_vars n_clauses
    n_vars=$(grep "^p cnf" "$cnf_file" | awk '{print $3}')
    n_clauses=$(grep "^p cnf" "$cnf_file" | awk '{print $4}')

    # Run satience
    local sat_start sat_end sat_dur sat_ec sat_result
    sat_start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$SATIENCE" "$cnf_file" > /dev/null 2>&1
    sat_ec=$?
    sat_end=$(date +%s.%N)
    sat_dur=$(awk "BEGIN{printf \"%.3f\", $sat_end-$sat_start}")
    case $sat_ec in
        10)  sat_result="SAT" ;;
        20)  sat_result="UNSAT" ;;
        124) sat_result="TMO" ;;
        *)   sat_result="EC=$sat_ec" ;;
    esac

    # Run minisat
    local mini_start mini_end mini_dur mini_ec mini_result
    mini_start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" minisat "$cnf_file" /dev/null > /dev/null 2>&1
    mini_ec=$?
    mini_end=$(date +%s.%N)
    mini_dur=$(awk "BEGIN{printf \"%.3f\", $mini_end-$mini_start}")
    case $mini_ec in
        10)  mini_result="SAT" ;;
        20)  mini_result="UNSAT" ;;
        124) mini_result="TMO" ;;
        *)   mini_result="EC=$mini_ec" ;;
    esac

    printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "$label" "$n_vars" "$n_clauses" "$sat_result" "$sat_dur" "$mini_result" "$mini_dur" "$cnf_args" \
        > "$res_file"
}

# --- Launch workers with FIFO semaphore ---
fifo="$WORK_DIR/sem"
mkfifo "$fifo"
exec 3<>"$fifo"
rm "$fifo"
for ((i = 0; i < JOBS; i++)); do echo >&3; done

echo "=== CNFgen vs Minisat Comparison ===" >&2
echo "Satience:  $SATIENCE" >&2
echo "Minisat:   $(command -v minisat)" >&2
echo "Timeout:   ${TIMEOUT_SEC}s per solver per instance" >&2
echo "Workers:   $JOBS" >&2
echo "Instances: $TOTAL" >&2
echo "" >&2

wall_start=$(date +%s.%N)
for i in "${!INSTANCES[@]}"; do
    read -u 3
    (
        run_instance "$i" "${INSTANCES[$i]}"
        echo >&3
    ) &
    printf '.' >&2
done
wait
exec 3>&-
wall_end=$(date +%s.%N)
wall_dur=$(awk "BEGIN{printf \"%.1f\", $wall_end-$wall_start}")
echo "" >&2

# --- Collect results ---
declare -a RESULTS
for ((i = 0; i < TOTAL; i++)); do
    res_file="$WORK_DIR/res_$i"
    RESULTS[$i]=$(cat "$res_file" 2>/dev/null || echo "MISSING||||||||")
done

# Print full table sorted by satience_time descending (slowest satience first)
echo "=== Full Results (sorted by satience time desc) ==="
echo ""
printf "%-22s %7s %7s  %-7s %8s  %-7s %8s  %6s\n" \
    "label" "vars" "clauses" "sat" "sat_s" "mini" "mini_s" "ratio"
echo "-------------------------------------------------------------------------------------------"

# Store for sorting: "sat_dur|label|n_vars|n_clauses|sat_result|sat_dur|mini_result|mini_dur|cnf_args"
declare -a SORT_LINES
for ((i = 0; i < TOTAL; i++)); do
    IFS='|' read -r label n_vars n_clauses sat_result sat_dur mini_result mini_dur cnf_args <<< "${RESULTS[$i]}"
    SORT_LINES[$i]="$sat_dur|$label|$n_vars|$n_clauses|$sat_result|$sat_dur|$mini_result|$mini_dur|$cnf_args"
done

# Sort by sat_dur descending
IFS=$'\n' sorted=($(sort -t'|' -k1 -rn <<< "${SORT_LINES[*]}")); unset IFS

for line in "${sorted[@]}"; do
    IFS='|' read -r _ label n_vars n_clauses sat_result sat_dur mini_result mini_dur cnf_args <<< "$line"
    local_ratio="---"
    if [ "$sat_result" = "TMO" ] || [ "$mini_result" = "TMO" ]; then
        local_ratio="---"
    elif awk "BEGIN{exit !($sat_dur > 0.01)}"; then
        local_ratio=$(awk "BEGIN{printf \"%.1f\", $sat_dur/$mini_dur}")
    fi
    printf "%-22s %7s %7s  %-7s %8s  %-7s %8s  %6s\n" \
        "$label" "$n_vars" "$n_clauses" "$sat_result" "$sat_dur" "$mini_result" "$mini_dur" "$local_ratio"
done

echo ""
echo "=== Hard for satience, easy for minisat ==="
echo "(satience >= 1s AND minisat < 0.5s, or satience=TMO AND minisat solved)"
echo ""
printf "%-22s %7s %7s  %-7s %8s  %-7s %8s  %6s  %s\n" \
    "label" "vars" "clauses" "sat" "sat_s" "mini" "mini_s" "ratio" "cnf_args"
echo "-------------------------------------------------------------------------------------------"

hard_count=0
for line in "${sorted[@]}"; do
    IFS='|' read -r _ label n_vars n_clauses sat_result sat_dur mini_result mini_dur cnf_args <<< "$line"

    is_hard=0
    if [ "$sat_result" = "TMO" ] && [ "$mini_result" = "SAT" -o "$mini_result" = "UNSAT" ]; then
        is_hard=1
    elif [ "$sat_result" != "TMO" ] && [ "$mini_result" != "TMO" ]; then
        if awk "BEGIN{exit !($sat_dur >= 1.0)}" && awk "BEGIN{exit !($mini_dur < 0.5)}"; then
            is_hard=1
        fi
    fi

    if [ $is_hard -eq 1 ]; then
        hard_count=$((hard_count + 1))
        local_ratio="---"
        if [ "$sat_result" != "TMO" ] && [ "$mini_result" != "TMO" ]; then
            local_ratio=$(awk "BEGIN{printf \"%.1f\", $sat_dur/$mini_dur}")
        fi
        printf "%-22s %7s %7s  %-7s %8s  %-7s %8s  %6s  %s\n" \
            "$label" "$n_vars" "$n_clauses" "$sat_result" "$sat_dur" "$mini_result" "$mini_dur" "$local_ratio" "$cnf_args"
    fi
done

echo ""
echo "=== Summary ==="
echo "Total instances: $TOTAL"
echo "Hard-for-satience: $hard_count"
echo "Wall time: ${wall_dur}s ($JOBS workers)"

# Count by family
echo ""
echo "=== By family (satience avg time vs minisat avg time) ==="
declare -A fam_count fam_sat_sum fam_mini_sum fam_sat_solved fam_mini_solved
for line in "${sorted[@]}"; do
    IFS='|' read -r _ label n_vars n_clauses sat_result sat_dur mini_result mini_dur cnf_args <<< "$line"
    fam="${label%%_*}"
    fam="${label%%[0-9]*}"
    # Extract family from cnf_args
    fam="${cnf_args%% *}"
    fam_count[$fam]=$(( ${fam_count[$fam]:-0} + 1 ))
    if [ "$sat_result" != "TMO" ]; then
        fam_sat_sum[$fam]=$(awk "BEGIN{printf \"%.3f\", ${fam_sat_sum[$fam]:-0}+$sat_dur}")
        fam_sat_solved[$fam]=$(( ${fam_sat_solved[$fam]:-0} + 1 ))
    fi
    if [ "$mini_result" != "TMO" ]; then
        fam_mini_sum[$fam]=$(awk "BEGIN{printf \"%.3f\", ${fam_mini_sum[$fam]:-0}+$mini_dur}")
        fam_mini_solved[$fam]=$(( ${fam_mini_solved[$fam]:-0} + 1 ))
    fi
done

printf "%-15s %5s %5s %5s  %8s %8s\n" "family" "count" "sat" "mini" "sat_avg" "mini_avg"
echo "----------------------------------------------------------"
for fam in $(echo "${!fam_count[@]}" | tr ' ' '\n' | sort); do
    c=${fam_count[$fam]}
    s=${fam_sat_solved[$fam]:-0}
    m=${fam_mini_solved[$fam]:-0}
    sat_avg=$(awk "BEGIN{printf \"%.3f\", ${fam_sat_sum[$fam]:-0}/$c}")
    mini_avg=$(awk "BEGIN{printf \"%.3f\", ${fam_mini_sum[$fam]:-0}/$c}")
    printf "%-15s %5d %5d %5d  %8s %8s\n" "$fam" "$c" "$s" "$m" "$sat_avg" "$mini_avg"
done
