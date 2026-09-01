#!/bin/bash
#
# sweep_vivify.sh — Compare vivify ON vs OFF per instance.
#
# For each instance, runs twice (ON then OFF) sequentially to avoid CPU
# contention between the two runs. Captures: time, exit code, conflicts,
# vivify rounds. Produces a sorted table highlighting where vivify
# helps/hurts.
#
# Usage: BINARY=./satience_b4 ./sweep_vivify.sh

BINARY="${BINARY:-../satience_bench}"
TIMEOUT_SEC=30
MAX_PARALLEL=4
INSTANCE_FILE="minisat_fast_suite/instances.txt"
RESULTS_FILE="vivify_sweep_$(date +%Y%m%d_%H%M%S).txt"
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

echo "=== Vivify Sweep (ON vs OFF) ===" | tee "$RESULTS_FILE"
echo "Binary: $BINARY" | tee -a "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT_SEC}s per instance" | tee -a "$RESULTS_FILE"
echo "Parallel: $MAX_PARALLEL concurrent instances (ON+OFF sequential per instance)" | tee -a "$RESULTS_FILE"
echo "Date: $(date)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

INSTANCE_LIST=()
while IFS= read -r instance; do
    INSTANCE_LIST+=("$instance")
done < "$INSTANCE_FILE"

TOTAL=${#INSTANCE_LIST[@]}

# run_pair: runs one instance ON then OFF, writes result to $TMP_DIR/$num
# Format: instance|on_time|on_exit|on_conflicts|on_vrounds|off_time|off_exit|off_conflicts|off_vrounds
run_pair() {
    local num="$1"
    local instance="$2"
    local FILE="gbd_instances/$instance"
    local out="$TMP_DIR/$num"

    if [ ! -f "$FILE" ]; then
        echo "NOTFOUND" > "$out"
        return
    fi

    local on_log off_log
    on_log=$(mktemp /tmp/viv_on_XXXXXX.txt)
    off_log=$(mktemp /tmp/viv_off_XXXXXX.txt)

    # --- ON (default vivify) ---
    local on_start on_end on_dur on_exit on_final
    on_start=$(date +%s.%N)
    timeout "${TIMEOUT_SEC}s" $BINARY -stats 100000000 "$FILE" 2>"$on_log"
    on_exit=$?
    on_end=$(date +%s.%N)
    on_dur=$(echo "$on_end - $on_start" | bc)
    on_final=$(grep "\[final\]" "$on_log" 2>/dev/null | tail -1)
    local on_conflicts on_vrounds
    on_conflicts=$(echo "$on_final" | grep -oP 'conflicts=\K\d+' || echo "0")
    on_vrounds=$(echo "$on_final" | grep -oP 'vivify: rounds=\K\d+' || echo "0")

    # --- OFF (vivify disabled) ---
    local off_start off_end off_dur off_exit off_final
    off_start=$(date +%s.%N)
    timeout "${TIMEOUT_SEC}s" $BINARY -stats 100000000 -vivify-period 0 "$FILE" 2>"$off_log"
    off_exit=$?
    off_end=$(date +%s.%N)
    off_dur=$(echo "$off_end - $off_start" | bc)
    off_final=$(grep "\[final\]" "$off_log" 2>/dev/null | tail -1)
    local off_conflicts off_vrounds
    off_conflicts=$(echo "$off_final" | grep -oP 'conflicts=\K\d+' || echo "0")
    off_vrounds=$(echo "$off_final" | grep -oP 'vivify: rounds=\K\d+' || echo "0")

    rm -f "$on_log" "$off_log"

    echo "${instance}|${on_dur}|${on_exit}|${on_conflicts}|${on_vrounds}|${off_dur}|${off_exit}|${off_conflicts}|${off_vrounds}" > "$out"
}

# Launch instance-pairs with concurrency limit
running=0
for i in "${!INSTANCE_LIST[@]}"; do
    run_pair "$((i + 1))" "${INSTANCE_LIST[$i]}" &
    running=$((running + 1))

    if [ $running -ge $MAX_PARALLEL ]; then
        wait -n
        running=$((running - 1))
    fi
done
wait

# Collect and analyze results
echo "--- Per-instance results ---" | tee -a "$RESULTS_FILE"
printf "%-40s %8s %8s %8s %6s %8s %8s %8s %6s %s\n" \
    "Instance" "ON_time" "OFF_time" "delta" "vrounds" "ON_conf" "OFF_conf" "conf_d" "verdict" "" | tee -a "$RESULTS_FILE"

helps=0
hurts=0
neutral=0
nvr=0
flips=0
on_par2=0
off_par2=0

for i in "${!INSTANCE_LIST[@]}"; do
    num=$((i + 1))
    instance="${INSTANCE_LIST[$i]}"
    result=$(cat "$TMP_DIR/$num" 2>/dev/null)

    if [ "$result" = "NOTFOUND" ]; then
        printf "%-40s FILE NOT FOUND\n" "$instance" | tee -a "$RESULTS_FILE"
        continue
    fi

    IFS='|' read -r inst on_dur on_exit on_conflicts on_vrounds off_dur off_exit off_conflicts off_vrounds <<< "$result"

    # Determine verdict
    verdict=""
    on_solved=0; off_solved=0
    if [ "$on_exit" -eq 10 ] || [ "$on_exit" -eq 20 ]; then on_solved=1; fi
    if [ "$off_exit" -eq 10 ] || [ "$off_exit" -eq 20 ]; then off_solved=1; fi

    if [ "$on_solved" -ne 1 ] && [ "$off_solved" -ne 1 ]; then
        verdict="BOTH_TMO"
        on_par2=$(echo "$on_par2 + 2 * $TIMEOUT_SEC" | bc)
        off_par2=$(echo "$off_par2 + 2 * $TIMEOUT_SEC" | bc)
        flips=$((flips + 1))
    elif [ "$on_solved" -eq 1 ] && [ "$off_solved" -ne 1 ]; then
        verdict="VIVIFY_WINS"
        on_par2=$(echo "$on_par2 + $on_dur" | bc)
        off_par2=$(echo "$off_par2 + 2 * $TIMEOUT_SEC" | bc)
        helps=$((helps + 1))
        flips=$((flips + 1))
    elif [ "$on_solved" -ne 1 ] && [ "$off_solved" -eq 1 ]; then
        verdict="NO_VIVIFY_WINS"
        on_par2=$(echo "$on_par2 + 2 * $TIMEOUT_SEC" | bc)
        off_par2=$(echo "$off_par2 + $off_dur" | bc)
        hurts=$((hurts + 1))
        flips=$((flips + 1))
    else
        # Both solved — compare times
        delta=$(echo "$on_dur - $off_dur" | bc)
        abs_delta=$(echo "$delta" | sed 's/-//')
        pct=$(echo "scale=1; ($on_dur - $off_dur) * 100 / $off_dur" | bc 2>/dev/null)
        if [ "$on_vrounds" -eq 0 ]; then
            verdict="NEVER_FIRED"
            nvr=$((nvr + 1))
        elif [ "$(echo "$delta > 0" | bc)" -eq 1 ]; then
            verdict="HURTS(${pct}%)"
            hurts=$((hurts + 1))
        elif [ "$(echo "$delta < 0" | bc)" -eq 1 ]; then
            verdict="HELPS(${pct}%)"
            helps=$((helps + 1))
        else
            verdict="TIE"
            neutral=$((neutral + 1))
        fi
        on_par2=$(echo "$on_par2 + $on_dur" | bc)
        off_par2=$(echo "$off_par2 + $off_dur" | bc)
    fi

    # Compute conflict delta
    conf_delta=$(echo "$on_conflicts - $off_conflicts" | bc)

    printf "%-40s %8s %8s %8s %6s %8s %8s %8s %s\n" \
        "$instance" "$on_dur" "$off_dur" "$delta" "$on_vrounds" \
        "$on_conflicts" "$off_conflicts" "$conf_delta" "$verdict" | tee -a "$RESULTS_FILE"
done

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
echo "Total instances: $TOTAL" | tee -a "$RESULTS_FILE"
echo "Vivify helps:    $helps" | tee -a "$RESULTS_FILE"
echo "Vivify hurts:     $hurts" | tee -a "$RESULTS_FILE"
echo "Never fired:      $nvr" | tee -a "$RESULTS_FILE"
echo "Neutral/tie:      $neutral" | tee -a "$RESULTS_FILE"
echo "Flips (solvability changed): $flips" | tee -a "$RESULTS_FILE"
echo "PAR2 (vivify ON):  $(echo "scale=2; $on_par2 / $TOTAL" | bc)s" | tee -a "$RESULTS_FILE"
echo "PAR2 (vivify OFF): $(echo "scale=2; $off_par2 / $TOTAL" | bc)s" | tee -a "$RESULTS_FILE"
