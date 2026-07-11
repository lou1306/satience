#!/bin/bash
# Cross-check satience verdicts against minisat on the MiniSat Fast Suite.
#
# Usage: bash benchmark/cross_check_minisat.sh [results_file]
#
# If results_file is not given, runs satience on all instances first.
# Otherwise, parses verdicts from the given results file and only runs
# minisat on instances that satience solved (not timeouts).
#
# This catches false UNSAT (satience=UNSAT, minisat=SAT) and false SAT
# (satience=SAT, minisat=UNSAT) that the fuzzer cannot detect (the fuzzer
# only verifies SAT models, not UNSAT correctness).

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
INSTANCES_DIR="$REPO_ROOT/benchmark/gbd_instances"
INSTANCES_LIST="$REPO_ROOT/benchmark/minisat_fast_suite/instances.txt"
TIMEOUT=30
MAX_PARALLEL=4

if [ ! -f "$REPO_ROOT/satience_bench" ]; then
    echo "Building satience_bench..."
    (cd "$REPO_ROOT" && GOAMD64=v3 go build -o satience_bench ./cmd/satience)
fi

# Read instances into array
INSTANCE_LIST=()
while IFS= read -r instance; do
    [ -z "$instance" ] && continue
    INSTANCE_LIST+=("$instance")
done < "$INSTANCES_LIST"

TOTAL=${#INSTANCE_LIST[@]}
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

echo "=== Minisat Cross-Check ==="
echo "Running satience + minisat on all instances (${TIMEOUT}s timeout, ${MAX_PARALLEL} parallel)"
echo ""

# run_instance writes result to $TMP_DIR/$num in the format:
#   instance|sat_ec|mini_ec
run_instance() {
    local num="$1"
    local instance="$2"
    local instance_path="$INSTANCES_DIR/$instance"
    [ ! -f "$instance_path" ] && { echo "NOTFOUND|0|0" > "$TMP_DIR/$num"; return; }

    timeout $TIMEOUT "$REPO_ROOT/satience_bench" "$instance_path" > /dev/null 2>&1 || sat_ec=$?
    sat_ec=${sat_ec:-0}

    timeout $TIMEOUT minisat "$instance_path" /dev/null 2>&1 > /dev/null || mini_ec=$?
    mini_ec=${mini_ec:-0}

    echo "${instance}|${sat_ec}|${mini_ec}" > "$TMP_DIR/$num"
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
match=0
mismatch=0
timeout_count=0
false_unsat=0
false_sat=0

for i in "${!INSTANCE_LIST[@]}"; do
    num=$((i + 1))
    instance="${INSTANCE_LIST[$i]}"
    result=$(cat "$TMP_DIR/$num" 2>/dev/null)
    base="${instance%.cnf}"

    if [ "$result" = "NOTFOUND" ]; then
        echo "? $base - FILE NOT FOUND"
        continue
    fi

    IFS='|' read -r inst sat_ec mini_ec <<< "$result"

    if [ "$sat_ec" -eq 10 ]; then sat_st="SAT"; elif [ "$sat_ec" -eq 20 ]; then sat_st="UNSAT"; else sat_st="TIMEOUT"; fi
    if [ "$mini_ec" -eq 10 ]; then mini_st="SAT"; elif [ "$mini_ec" -eq 20 ]; then mini_st="UNSAT"; else mini_st="TIMEOUT"; fi

    if [ "$sat_st" = "TIMEOUT" ] || [ "$mini_st" = "TIMEOUT" ]; then
        timeout_count=$((timeout_count + 1))
        echo "? $base satience=$sat_st minisat=$mini_st (timeout)"
        continue
    fi

    if [ "$sat_st" = "$mini_st" ]; then
        match=$((match + 1))
    else
        mismatch=$((mismatch + 1))
        if [ "$sat_st" = "UNSAT" ] && [ "$mini_st" = "SAT" ]; then
            false_unsat=$((false_unsat + 1))
            echo "FALSE UNSAT! $base satience=$sat_st minisat=$mini_st"
        elif [ "$sat_st" = "SAT" ] && [ "$mini_st" = "UNSAT" ]; then
            false_sat=$((false_sat + 1))
            echo "FALSE SAT!  $base satience=$sat_st minisat=$mini_st"
        else
            echo "MISMATCH  $base satience=$sat_st minisat=$mini_st"
        fi
    fi
done

echo ""
echo "=== Summary ==="
echo "Matched:        $match"
echo "Mismatched:      $mismatch"
echo "  False UNSAT:   $false_unsat"
echo "  False SAT:     $false_sat"
echo "Timeouts:        $timeout_count"
if [ "$mismatch" -eq 0 ]; then
    echo "SOUND: All verdicts match minisat."
else
    echo "UNSOUND: $mismatch mismatched verdicts!"
fi
