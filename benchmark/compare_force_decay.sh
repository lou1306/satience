#!/bin/bash
# Phase 3: Compare default vs aggressive decay across the 72-instance suite.
# Captures prod= from [final] line to test if restart productivity separates
# the two decay-preference groups.

BINARY="${BINARY:-/tmp/opencode-sat-new/satience_bench}"
TIMEOUT_SEC=30
MAX_PARALLEL=4
INSTANCE_FILE="minisat_fast_suite/instances.txt"
RESULTS_FILE="force_decay_comparison_$(date +%Y%m%d_%H%M%S).txt"
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

# Read instances
INSTANCE_LIST=()
while IFS= read -r instance; do
    INSTANCE_LIST+=("$instance")
done < "$INSTANCE_FILE"
TOTAL=${#INSTANCE_LIST[@]}

run_instance() {
    local num="$1"
    local instance="$2"
    local mode="$3"
    local FILE="gbd_instances/$instance"

    if [ ! -f "$FILE" ]; then
        echo "NOTFOUND" > "$TMP_DIR/${num}_${mode}"
        return
    fi

    local VARS CLAUSES EXIT_CODE START END DURATION
    VARS=$(head -20 "$FILE" | grep "^p " | awk '{print $3}')
    CLAUSES=$(head -20 "$FILE" | grep "^p " | awk '{print $4}')

    local OUT
    START=$(date +%s.%N)
    OUT=$(timeout "${TIMEOUT_SEC}s" $BINARY -force-decay "$mode" -stats 1000 "$FILE" 2>&1)
    EXIT_CODE=$?
    END=$(date +%s.%N)
    DURATION=$(echo "$END - $START" | bc)

    local PROD RESTARTS
    PROD=$(echo "$OUT" | grep "\[final\]" | grep -oP 'prod=\K[0-9.]+')
    RESTARTS=$(echo "$OUT" | grep "\[final\]" | grep -oP 'restarts=\K[0-9]+')
    local CLAUSEDATA
    CLAUSEDATA=$(echo "$OUT" | grep "\[final\]" | grep -oP 'conflicts=\K[0-9]+')
    local DECISIONS
    DECISIONS=$(echo "$OUT" | grep "\[final\]" | grep -oP 'decisions=\K[0-9]+')

    echo "${VARS}|${CLAUSES}|${EXIT_CODE}|${DURATION}|${PROD:-NA}|${RESTARTS:-NA}|${CLAUSEDATA:-NA}|${DECISIONS:-NA}" > "$TMP_DIR/${num}_${mode}"
}

echo "=== Force-Decay Comparison (default vs aggressive) ===" | tee "$RESULTS_FILE"
echo "Binary: $BINARY" | tee -a "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT_SEC}s, Parallel: ${MAX_PARALLEL}" | tee -a "$RESULTS_FILE"
echo "Date: $(date)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Run both modes for all instances
running=0
for i in "${!INSTANCE_LIST[@]}"; do
    for mode in default aggressive; do
        run_instance "$((i + 1))" "${INSTANCE_LIST[$i]}" "$mode" &
        running=$((running + 1))
        if [ $running -ge $MAX_PARALLEL ]; then
            wait -n
            running=$((running - 1))
        fi
    done
done
wait

# Collect and compare
echo "Instance | Vars | Clauses | Default(sec/exit/prod/restarts/conflicts) | Aggressive(sec/exit/prod/restarts/conflicts) | Delta_sec" | tee -a "$RESULTS_FILE"
echo "---|---|---|---|---|---" | tee -a "$RESULTS_FILE"

DEFAULT_PAR2=0
AGGR_PAR2=0
DEFAULT_SOLVED=0
AGGR_SOLVED=0

for i in "${!INSTANCE_LIST[@]}"; do
    num=$((i + 1))
    instance="${INSTANCE_LIST[$i]}"

    d_result=$(cat "$TMP_DIR/${num}_default" 2>/dev/null)
    a_result=$(cat "$TMP_DIR/${num}_aggressive" 2>/dev/null)

    if [ "$d_result" = "NOTFOUND" ] || [ "$a_result" = "NOTFOUND" ]; then
        echo "$instance | FILE NOT FOUND" | tee -a "$RESULTS_FILE"
        continue
    fi

    IFS='|' read -r DV DC DEXIT DDUR DPROD DREST DCONF DDEC <<< "$d_result"
    IFS='|' read -r AV AC AEXIT ADUR APROD AREST ACONF ADEC <<< "$a_result"

    # PAR2 scoring
    if [ "$DEXIT" -eq 10 ] || [ "$DEXIT" -eq 20 ]; then
        DEFAULT_PAR2=$(echo "$DEFAULT_PAR2 + $DDUR" | bc)
        DEFAULT_SOLVED=$((DEFAULT_SOLVED + 1))
    else
        DEFAULT_PAR2=$(echo "$DEFAULT_PAR2 + 2 * $TIMEOUT_SEC" | bc)
    fi

    if [ "$AEXIT" -eq 10 ] || [ "$AEXIT" -eq 20 ]; then
        AGGR_PAR2=$(echo "$AGGR_PAR2 + $ADUR" | bc)
        AGGR_SOLVED=$((AGGR_SOLVED + 1))
    else
        AGGR_PAR2=$(echo "$AGGR_PAR2 + 2 * $TIMEOUT_SEC" | bc)
    fi

    DELTA=$(echo "$ADUR - $DDUR" | bc)

    echo "$instance | ${DV}v ${DC}c | ${DDUR}s exit=$DEXIT prod=${DPROD} r=${DREST} conf=${DCONF} | ${ADUR}s exit=$AEXIT prod=${APROD} r=${AREST} conf=${ACONF} | ${DELTA}" | tee -a "$RESULTS_FILE"
done

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
echo "Default:     $DEFAULT_SOLVED/$TOTAL solved, PAR2=$(echo "scale=2; $DEFAULT_PAR2 / $TOTAL" | bc)s" | tee -a "$RESULTS_FILE"
echo "Aggressive:  $AGGR_SOLVED/$TOTAL solved, PAR2=$(echo "scale=2; $AGGR_PAR2 / $TOTAL" | bc)s" | tee -a "$RESULTS_FILE"
