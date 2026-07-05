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

if [ ! -f "$REPO_ROOT/satience_bench" ]; then
    echo "Building satience_bench..."
    (cd "$REPO_ROOT" && GOAMD64=v3 go build -o satience_bench ./cmd/satience)
fi

echo "=== Minisat Cross-Check ==="
echo "Running minisat on all instances (30s timeout)..."
echo ""

match=0
mismatch=0
timeout_count=0
false_unsat=0
false_sat=0

while IFS= read -r instance; do
    [ -z "$instance" ] && continue
    instance_path="$INSTANCES_DIR/$instance"
    [ ! -f "$instance_path" ] && continue
    base="${instance%.cnf}"

    # Run satience
    sat_out=$(timeout $TIMEOUT "$REPO_ROOT/satience_bench" "$instance_path" 2>&1)
    sat_ec=$?
    if [ $sat_ec -eq 10 ]; then sat_st="SAT"; elif [ $sat_ec -eq 20 ]; then sat_st="UNSAT"; else sat_st="TIMEOUT"; fi

    # Run minisat
    mini_out=$(timeout $TIMEOUT minisat "$instance_path" /dev/null 2>&1)
    mini_ec=$?
    if [ $mini_ec -eq 10 ]; then mini_st="SAT"; elif [ $mini_ec -eq 20 ]; then mini_st="UNSAT"; else mini_st="TIMEOUT"; fi

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
done < "$INSTANCES_LIST"

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
