#!/bin/bash

BINARY="../satience_bench"
EXTRA_FLAGS="${EXTRA_FLAGS:-}"
TIMEOUT_SEC=30
MAX_PARALLEL=4
INSTANCE_FILE="minisat_fast_suite/instances.txt"
RESULTS_FILE="satience_fast_suite_results_$(date +%Y%m%d_%H%M%S).txt"
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

echo "=== Satience Fast Suite Benchmark ===" | tee "$RESULTS_FILE"
echo "Binary: $BINARY" | tee -a "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT_SEC}s per instance" | tee -a "$RESULTS_FILE"
echo "Parallel: ${MAX_PARALLEL} concurrent" | tee -a "$RESULTS_FILE"
echo "Date: $(date)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Read instances into array
INSTANCE_LIST=()
while IFS= read -r instance; do
    INSTANCE_LIST+=("$instance")
done < "$INSTANCE_FILE"

TOTAL=${#INSTANCE_LIST[@]}

run_instance() {
    local num="$1"
    local instance="$2"
    local FILE="gbd_instances/$instance"

    if [ ! -f "$FILE" ]; then
        echo "NOTFOUND" > "$TMP_DIR/$num"
        return
    fi

    local VARS CLAUSES EXIT_CODE START END DURATION
    VARS=$(head -20 "$FILE" | grep "^p " | awk '{print $3}')
    CLAUSES=$(head -20 "$FILE" | grep "^p " | awk '{print $4}')

    START=$(date +%s.%N)
    timeout "${TIMEOUT_SEC}s" $BINARY $EXTRA_FLAGS "$FILE" > /dev/null 2>&1
    EXIT_CODE=$?
    END=$(date +%s.%N)
    DURATION=$(echo "$END - $START" | bc)

    echo "${VARS}|${CLAUSES}|${EXIT_CODE}|${DURATION}" > "$TMP_DIR/$num"
}

# Launch instances with concurrency limit
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

# Collect results in order
SAT=0
UNSAT=0
TIMEOUT_COUNT=0
PAR2_SUM=0

for i in "${!INSTANCE_LIST[@]}"; do
    num=$((i + 1))
    instance="${INSTANCE_LIST[$i]}"
    result=$(cat "$TMP_DIR/$num" 2>/dev/null)

    if [ "$result" = "NOTFOUND" ]; then
        echo "[$num/$TOTAL] ? $instance - FILE NOT FOUND" | tee -a "$RESULTS_FILE"
        continue
    fi

    IFS='|' read -r VARS CLAUSES EXIT_CODE DURATION <<< "$result"

    if [ "$EXIT_CODE" -eq 10 ]; then
        VERDICT="SAT"; SAT=$((SAT + 1)); SYMBOL="✓"
        PAR2_SUM=$(echo "$PAR2_SUM + $DURATION" | bc)
    elif [ "$EXIT_CODE" -eq 20 ]; then
        VERDICT="UNSAT"; UNSAT=$((UNSAT + 1)); SYMBOL="✗"
        PAR2_SUM=$(echo "$PAR2_SUM + $DURATION" | bc)
    elif [ "$EXIT_CODE" -eq 124 ]; then
        VERDICT="TIMEOUT"; TIMEOUT_COUNT=$((TIMEOUT_COUNT + 1)); SYMBOL="⊠"
        PAR2_SUM=$(echo "$PAR2_SUM + 2 * $TIMEOUT_SEC" | bc)
    else
        VERDICT="ERROR (exit=$EXIT_CODE)"; SYMBOL="?"
        PAR2_SUM=$(echo "$PAR2_SUM + 2 * $TIMEOUT_SEC" | bc)
    fi

    echo "[$num/$TOTAL] $SYMBOL $instance (${VARS}v, ${CLAUSES}c) - $VERDICT (${DURATION}s)" | tee -a "$RESULTS_FILE"
done

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
echo "Total: $TOTAL" | tee -a "$RESULTS_FILE"
echo "SAT: $SAT" | tee -a "$RESULTS_FILE"
echo "UNSAT: $UNSAT" | tee -a "$RESULTS_FILE"
echo "TIMEOUT: $TIMEOUT_COUNT" | tee -a "$RESULTS_FILE"
echo "Solve Rate: $(echo "scale=1; ($SAT + $UNSAT) * 100 / $TOTAL" | bc)%" | tee -a "$RESULTS_FILE"
echo "PAR2: $(echo "scale=2; $PAR2_SUM / $TOTAL" | bc)s" | tee -a "$RESULTS_FILE"
