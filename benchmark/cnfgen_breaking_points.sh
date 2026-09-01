#!/bin/bash
# cnfgen_breaking_points.sh — scale up divergent families to find breaking points
# where satience times out (30s) and minisat solves fast (<2s).
#
# For each instance: generate CNF, run satience (30s). If satience TMO, run minisat
# (30s). Report breaking points where satience=TMO AND minisat<2s.
#
# Usage: bash cnfgen_breaking_points.sh [JOBS]
# Output: stdout + /tmp/opencode/breaking_points_results.txt

set -u
JOBS="${1:-4}"
SATIENCE="${SATIENCE:-../satience_bench}"
TIMEOUT_SEC=30
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
RESULTS_FILE="/tmp/opencode/breaking_points_results.txt"

# Family scaling probes. Each line: <label> <cnfgen args...>
# Built from Phase 1 divergences, scaling up toward the 30s crossing.
INSTANCES=(
    # rphp: 10_8_8 had satience 28.9s (nearly TMO), minisat 0.43s. Scale up slightly.
    "rphp_11_8_8       rphp 11 8 8"
    "rphp_10_9_9       rphp 10 9 9"
    "rphp_11_9_9       rphp 11 9 9"
    "rphp_10_8_9       rphp 10 8 9"

    # php: 10_9 was TMO@10s, minisat 3.56s. Check at 30s, then scale up.
    "php_10_9          php 10 9"
    "php_11_10         php 11 10"
    "php_12_11         php 12 11"
    "php_13_12         php 13 12"
    "php_onto_10_9     php --onto 10 9"
    "php_onto_11_10    php --onto 11 10"
    "php_func_10_9     php --functional 10 9"
    "php_func_11_10    php --functional 11 10"

    # rand3sat at density 4.26 (phase transition, below D2 gate of 4.5).
    "rand3sat_300_1275  randkcnf 3 300 1275"
    "rand3sat_350_1488  randkcnf 3 350 1488"
    "rand3sat_400_1700  randkcnf 3 400 1700"
    "rand3sat_250_1065  randkcnf 3 250 1065"

    # stone: 3_tree10 had satience 5.26s, minisat 0.35s. Scale up tree size.
    "stone_3_tree12    stone 3 tree 12"
    "stone_3_tree14    stone 3 tree 14"
    "stone_4_pyramid6  stone 4 pyramid 6"
    "stone_4_pyramid7  stone 4 pyramid 7"
    "stone_4_pyramid8  stone 4 pyramid 8"

    # rand4sat: 75_735 had satience 3.65s, minisat 0.42s. Scale up N at density 9.8.
    "rand4sat_100_980  randkcnf 4 100 980"
    "rand4sat_125_1225 randkcnf 4 125 1225"
    "rand4sat_150_1470 randkcnf 4 150 1470"

    # op: 18 had satience 2.74s, minisat 0.49s. Scale up.
    "op_20             op 20"
    "op_22             op 22"
    "op_25             op 25"

    # parity: 13 had satience 1.79s, minisat 0.42s. Scale up (odd = UNSAT).
    "parity_15         parity 15"
    "parity_17         parity 17"
    "parity_19         parity 19"
)

TOTAL=${#INSTANCES[@]}

run_instance() {
    local num="$1"
    local label="$2"
    shift 2
    local args=("$@")
    local cnf_file="$TMP_DIR/${label}.cnf"

    if ! cnfgen -S 1 --output "$cnf_file" "${args[@]}" > /dev/null 2>&1; then
        echo "GENFAIL|$label|0|0|0|0.00|0|0.00" > "$TMP_DIR/$num"
        return
    fi

    local vars clauses
    vars=$(head -20 "$cnf_file" | grep "^p " | awk '{print $3}')
    clauses=$(head -20 "$cnf_file" | grep "^p " | awk '{print $4}')

    # Run satience first
    local s_start s_end s_dur s_code
    s_start=$(date +%s.%N)
    timeout "${TIMEOUT_SEC}s" $SATIENCE "$cnf_file" > /dev/null 2>&1
    s_code=$?
    s_end=$(date +%s.%N)
    s_dur=$(echo "$s_end - $s_start" | bc)

    # If satience timed out, run minisat
    local m_code=0 m_dur=0.00
    if [ "$s_code" -eq 124 ]; then
        local m_start m_end
        m_start=$(date +%s.%N)
        timeout "${TIMEOUT_SEC}s" minisat "$cnf_file" > /dev/null 2>&1
        m_code=$?
        m_end=$(date +%s.%N)
        m_dur=$(echo "$m_end - $m_start" | bc)
    fi

    echo "${s_code}|${m_code}|${vars}|${clauses}|${s_dur}|${m_dur}|${label}" > "$TMP_DIR/$num"
}

echo "=== CNFgen Breaking Points Sweep ===" | tee "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT_SEC}s, JOBS: $JOBS, Instances: $TOTAL" | tee -a "$RESULTS_FILE"
echo "Breaking point = satience TMO AND minisat < 2s" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
printf "%-22s %6s %7s  %8s %8s  %8s %8s  %s\n" "LABEL" "VARS" "CLAUSES" "SAT" "SAT_S" "MINI" "MINI_S" "BREAK?" | tee -a "$RESULTS_FILE"
printf '%0.s-' {1..90} | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Launch with concurrency limit
running=0
for i in "${!INSTANCES[@]}"; do
    entry="${INSTANCES[$i]}"
    IFS=' ' read -r label args <<< "$entry"
    run_instance "$((i + 1))" "$label" $args &
    running=$((running + 1))
    if [ $running -ge $JOBS ]; then
        wait -n
        running=$((running - 1))
    fi
done
wait

# Collect and print results
breaking_count=0
for i in "${!INSTANCES[@]}"; do
    num=$((i + 1))
    result=$(cat "$TMP_DIR/$num" 2>/dev/null)
    if [ -z "$result" ] || [ "$result" = "GENFAIL|0|0|0|0.00|0|0.00" ]; then
        label=$(echo "${INSTANCES[$i]}" | awk '{print $1}')
        echo "[$num/$TOTAL] $label - GENFAIL" | tee -a "$RESULTS_FILE"
        continue
    fi

    IFS='|' read -r s_code m_code vars clauses s_dur m_dur label <<< "$result"

    s_verdict="?"
    case $s_code in
        10) s_verdict="SAT" ;;
        20) s_verdict="UNSAT" ;;
        124) s_verdict="TMO" ;;
        *) s_verdict="ERR($s_code)" ;;
    esac

    m_verdict="—"
    if [ "$s_code" -eq 124 ]; then
        case $m_code in
            10) m_verdict="SAT" ;;
            20) m_verdict="UNSAT" ;;
            124) m_verdict="TMO" ;;
            *) m_verdict="ERR($m_code)" ;;
        esac
    fi

    is_breaking=""
    if [ "$s_code" -eq 124 ] && [ "$m_code" -ne 124 ]; then
        # satience timed out, minisat solved — check minisat time
        is_below_2=$(echo "$m_dur < 2.0" | bc)
        if [ "$is_below_2" -eq 1 ]; then
            is_breaking="*** BREAKING"
            breaking_count=$((breaking_count + 1))
        else
            is_breaking="(minisat slow)"
        fi
    fi

    printf "%-22s %6s %7s  %8s %8.2f  %8s %8s  %s\n" \
        "$label" "$vars" "$clauses" "$s_verdict" "$s_dur" "$m_verdict" "$m_dur" "$is_breaking" | tee -a "$RESULTS_FILE"
done

echo "" | tee -a "$RESULTS_FILE"
echo "=== Breaking points (satience TMO @ ${TIMEOUT_SEC}s AND minisat < 2s): $breaking_count ===" | tee -a "$RESULTS_FILE"
