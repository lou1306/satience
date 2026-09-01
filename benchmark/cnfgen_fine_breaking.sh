#!/bin/bash
# cnfgen_fine_breaking.sh — fine-grained probe around the breaking boundary.
# Focus: rphp (widest gap), stone (tree sizes), rand3sat (density edge).
#
# For each instance: run satience 3x (check variance), then minisat once.
# Reports breaking points where satience TMO in any run AND minisat < 5s.

set -u
SATIENCE="${SATIENCE:-../satience_bench}"
TIMEOUT_SEC=30
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
RESULTS_FILE="/tmp/opencode/fine_breaking_results.txt"

INSTANCES=(
    # rphp fine probe around 10_8_8 (satience 28.9s) / 10_9_9 (satience TMO)
    "rphp_10_8_8       rphp 10 8 8"
    "rphp_10_8_10      rphp 10 8 10"
    "rphp_10_9_8       rphp 10 9 8"
    "rphp_10_9_9       rphp 10 9 9"
    "rphp_10_9_10      rphp 10 9 10"
    "rphp_10_10_10     rphp 10 10 10"
    "rphp_11_8_8       rphp 11 8 8"
    "rphp_11_9_8       rphp 11 9 8"
    "rphp_11_8_9       rphp 11 8 9"

    # stone tree fine probe: tree10=5.26s, tree12=TMO. Find the crossing.
    "stone_3_tree10    stone 3 tree 10"
    "stone_3_tree11    stone 3 tree 11"
    "stone_3_tree12    stone 3 tree 12"

    # rand3sat density edge: 250_1065 (TMO, minisat 4.7s). Probe nearby densities.
    "rand3sat_250_1065 randkcnf 3 250 1065"
    "rand3sat_250_1100 randkcnf 3 250 1100"
    "rand3sat_250_1150 randkcnf 3 250 1150"
)

TOTAL=${#INSTANCES[@]}

run_one() {
    local label="$1"
    shift
    local args=("$@")
    local cnf_file="$TMP_DIR/${label}.cnf"

    if ! cnfgen -S 1 --output "$cnf_file" "${args[@]}" > /dev/null 2>&1; then
        echo "0|0|GENFAIL|0.00|0.00|0.00|0|0.00"
        return
    fi

    local vars clauses
    vars=$(head -20 "$cnf_file" | grep "^p " | awk '{print $3}')
    clauses=$(head -20 "$cnf_file" | grep "^p " | awk '{print $4}')

    # Run satience 3 times to check variance
    local s_code s_dur s_start s_end
    local codes="" durs=""
    for run in 1 2 3; do
        s_start=$(date +%s.%N)
        timeout "${TIMEOUT_SEC}s" $SATIENCE "$cnf_file" > /dev/null 2>&1
        s_code=$?
        s_end=$(date +%s.%N)
        s_dur=$(echo "$s_end - $s_start" | bc)
        codes="$codes $s_code"
        durs="$durs $s_dur"
    done

    # Run minisat once
    local m_code m_dur m_start m_end
    m_start=$(date +%s.%N)
    timeout "${TIMEOUT_SEC}s" minisat "$cnf_file" > /dev/null 2>&1
    m_code=$?
    m_end=$(date +%s.%N)
    m_dur=$(echo "$m_end - $m_start" | bc)

    echo "${vars}|${clauses}|$codes|$durs|$m_code|$m_dur"
}

echo "=== Fine-Grained Breaking Point Probe ===" | tee "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT_SEC}s, 3 satience runs per instance" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
printf "%-22s %6s %7s  %-18s %-24s  %8s %8s  %s\n" "LABEL" "VARS" "CLAUSES" "SAT_CODES" "SAT_TIMES" "MINI" "MINI_S" "BREAK?" | tee -a "$RESULTS_FILE"
printf '%0.s-' {1..110} | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

for i in "${!INSTANCES[@]}"; do
    entry="${INSTANCES[$i]}"
    IFS=' ' read -r label args <<< "$entry"
    result=$(run_one "$label" $args)

    IFS='|' read -r vars clauses codes durs m_code m_dur <<< "$result"

    if [ "$vars" = "0" ]; then
        printf "%-22s %6s %7s  %s\n" "$label" "-" "-" "GENFAIL" | tee -a "$RESULTS_FILE"
        continue
    fi

    m_verdict="—"
    case $m_code in
        10) m_verdict="SAT" ;;
        20) m_verdict="UNSAT" ;;
        124) m_verdict="TMO" ;;
        *) m_verdict="E$m_code" ;;
    esac

    # Check if any satience run timed out
    any_tmo=0
    for c in $codes; do
        if [ "$c" -eq 124 ]; then any_tmo=1; fi
    done

    is_breaking=""
    if [ "$any_tmo" -eq 1 ] && [ "$m_code" -ne 124 ]; then
        is_below_5=$(echo "$m_dur < 5.0" | bc)
        if [ "$is_below_5" -eq 1 ]; then
            is_breaking="*** BREAKING (<5s)"
        else
            is_breaking="(minisat $m_dur s)"
        fi
    fi

    # Decode satience codes for display
    s_display=""
    for c in $codes; do
        case $c in
            10) s_display="$s_display S" ;;
            20) s_display="$s_display U" ;;
            124) s_display="$s_display T" ;;
            *) s_display="$s_display E" ;;
        esac
    done

    printf "%-22s %6s %7s  [%-16s] %-24s  %8s %8.2f  %s\n" \
        "$label" "$vars" "$clauses" "$s_display" "$durs" "$m_verdict" "$m_dur" "$is_breaking" | tee -a "$RESULTS_FILE"
done

echo "" | tee -a "$RESULTS_FILE"
echo "S=SAT, U=UNSAT, T=TMO, E=ERROR" | tee -a "$RESULTS_FILE"
