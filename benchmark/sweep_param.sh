#!/bin/bash
# Sweep a single CLI flag across multiple values, report PAR2 + solve rate.
# Usage: ./sweep_param.sh <flag> <val1> <val2> ... <valN>
# Optionally set SWEEP_LABEL for nicer output.
BINARY="${BINARY:-./satience_t1}"
TIMEOUT_SEC=30
MAX_PARALLEL=4
INSTANCE_FILE="minisat_fast_suite/instances.txt"
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

INSTANCE_LIST=()
while IFS= read -r instance; do
    INSTANCE_LIST+=("$instance")
done < "$INSTANCE_FILE"
TOTAL=${#INSTANCE_LIST[@]}

run_one_value() {
    local flag="$1"
    local val="$2"
    local outdir="$3"

    run_instance() {
        local num="$1"
        local instance="$2"
        local FILE="gbd_instances/$instance"
        if [ ! -f "$FILE" ]; then
            echo "NOTFOUND" > "$outdir/$num"
            return
        fi
        local VARS CLAUSES EXIT_CODE START END DURATION
        VARS=$(head -20 "$FILE" | grep "^p " | awk '{print $3}')
        CLAUSES=$(head -20 "$FILE" | grep "^p " | awk '{print $4}')
        START=$(date +%s.%N)
        if [ -z "$val" ]; then
            timeout "${TIMEOUT_SEC}s" $BINARY "$FILE" > /dev/null 2>&1
        else
            timeout "${TIMEOUT_SEC}s" $BINARY "-$flag=$val" "$FILE" > /dev/null 2>&1
        fi
        EXIT_CODE=$?
        END=$(date +%s.%N)
        DURATION=$(echo "$END - $START" | bc)
        echo "${VARS}|${CLAUSES}|${EXIT_CODE}|${DURATION}" > "$outdir/$num"
    }

    running=0
    for i in "${!INSTANCE_LIST[@]}"; do
        run_instance "$((i + 1))" "${INSTANCE_LIST[$i]}" &
        running=$((running + 1))
        if [ $running -ge $MAX_PARALLEL ]; then
            wait -n
            running=$((running - 1))
        fi
    done
    wait

    local SAT=0 UNSAT=0 TIMEOUT_COUNT=0 PAR2_SUM=0
    for i in "${!INSTANCE_LIST[@]}"; do
        num=$((i + 1))
        result=$(cat "$outdir/$num" 2>/dev/null)
        if [ "$result" = "NOTFOUND" ]; then continue; fi
        IFS='|' read -r VARS CLAUSES EXIT_CODE DURATION <<< "$result"
        if [ "$EXIT_CODE" -eq 10 ]; then
            SAT=$((SAT + 1)); PAR2_SUM=$(echo "$PAR2_SUM + $DURATION" | bc)
        elif [ "$EXIT_CODE" -eq 20 ]; then
            UNSAT=$((UNSAT + 1)); PAR2_SUM=$(echo "$PAR2_SUM + $DURATION" | bc)
        elif [ "$EXIT_CODE" -eq 124 ]; then
            TIMEOUT_COUNT=$((TIMEOUT_COUNT + 1)); PAR2_SUM=$(echo "$PAR2_SUM + 2 * $TIMEOUT_SEC" | bc)
        else
            PAR2_SUM=$(echo "$PAR2_SUM + 2 * $TIMEOUT_SEC" | bc)
        fi
    done
    echo "$val|$SAT|$UNSAT|$TIMEOUT_COUNT|$(echo "scale=2; $PAR2_SUM / $TOTAL" | bc)"
}

FLAG="$1"
shift
LABEL="${SWEEP_LABEL:-$FLAG}"
printf "%-20s %8s %6s %6s %8s %8s\n" "PARAM" "VALUE" "SAT" "UNSAT" "TIMEOUT" "PAR2"
for val in "$@"; do
    outdir="$TMP_DIR/$val"
    mkdir -p "$outdir"
    line=$(run_one_value "$FLAG" "$val" "$outdir")
    IFS='|' read -r V SAT UNSAT TO PAR2 <<< "$line"
    printf "%-20s %8s %6d %6d %8d %8s\n" "$LABEL" "$V" "$SAT" "$UNSAT" "$TO" "$PAR2"
done
