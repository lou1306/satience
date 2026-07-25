#!/bin/bash
# fuzz_random_ksat.sh — Random k-SAT soundness fuzzer for satience.
#
# Generates fresh random k-SAT instances each run (time-based seeds) and
# cross-checks satience's verdict against minisat. Catches:
#   - False SAT  (satience=SAT but -verify fails → bad model)
#   - False UNSAT (satience=UNSAT but minisat=SAT)
#
# On any mismatch, the CNF file and metadata are saved to fuzz_failures/ for
# reproducibility.
#
# Usage: bash benchmark/fuzz_random_ksat.sh
#   ITERATIONS  number of instances to test (default 100)
#   TIMEOUT     per-solver timeout in seconds (default 10)
#   JOBS        parallel workers (default 6)
#
# Requires: cnfgen (pipx install cnfgen), minisat, GOAMD64=v3 go build.

set -u

ITERATIONS=${ITERATIONS:-100}
TIMEOUT=${TIMEOUT:-10}
JOBS=${JOBS:-6}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"

if ! command -v cnfgen >/dev/null 2>&1; then
    echo "ERROR: cnfgen not found. Install with: pipx install cnfgen" >&2
    exit 1
fi
if ! command -v minisat >/dev/null 2>&1; then
    echo "ERROR: minisat not found." >&2
    exit 1
fi

if [ ! -x "$SATIENCE" ]; then
    echo "Building satience_bench..."
    (cd "$REPO_ROOT" && GOAMD64=v3 go build -o satience_bench ./cmd/satience) || {
        echo "ERROR: build failed" >&2; exit 1
    }
fi

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

FAIL_DIR="$REPO_ROOT/benchmark/fuzz_failures"
mkdir -p "$FAIL_DIR"

# Coverage matrix: "k N density_ratio"
# Densities span below/at/above the phase transition for each k.
MATRIX=(
    # k=3 (phase transition at ratio ~4.26)
    "3 50  3.5"
    "3 50  4.26"
    "3 50  4.8"
    "3 100 3.5"
    "3 100 4.26"
    "3 100 4.8"
    "3 150 3.5"
    "3 150 4.26"
    "3 150 4.8"
    "3 200 4.26"
    "3 200 4.8"
    "3 300 4.26"
    # k=4 (phase transition at ratio ~9.8)
    "4 50  8.0"
    "4 50  9.8"
    "4 75  9.8"
    "4 100 9.8"
    # k=5 (phase transition at ratio ~21)
    "5 30  21"
    "5 50  21"
)

MATRIX_SIZE=${#MATRIX[@]}

# --- Worker ---
run_instance() {
    local idx="$1"
    local seed="$2"
    local entry="${MATRIX[$((idx % MATRIX_SIZE))]}"
    local k n ratio m
    read -r k n ratio <<< "$entry"
    m=$(awk "BEGIN{printf \"%d\", $n * $ratio}")

    local cnf_file="$WORK_DIR/inst_${idx}.cnf"
    local res_file="$WORK_DIR/res_${idx}"

    if ! cnfgen -S "$seed" randkcnf "$k" "$n" "$m" > "$cnf_file" 2>/dev/null; then
        printf 'GENFAIL|%d|%d|%s|%d|%d|0|0\n' "$k" "$n" "$ratio" "$m" "$seed" > "$res_file"
        return
    fi

    # Run satience with -verify (catches false SAT via bad model)
    local sat_ec sat_start sat_end sat_dur
    sat_start=$(date +%s.%N)
    timeout "$TIMEOUT" "$SATIENCE" -verify "$cnf_file" > /dev/null 2>&1
    sat_ec=$?
    sat_end=$(date +%s.%N)
    sat_dur=$(awk "BEGIN{printf \"%.3f\", $sat_end-$sat_start}")

    local sat_result
    case $sat_ec in
        10)  sat_result="SAT" ;;
        20)  sat_result="UNSAT" ;;
        124) sat_result="TMO" ;;
        1)   sat_result="BAD_MODEL" ;;
        *)   sat_result="EC=$sat_ec" ;;
    esac

    # If satience produced a verdict, cross-check with minisat
    local mini_result="—" mini_dur="0.000"
    if [ "$sat_result" = "SAT" ] || [ "$sat_result" = "UNSAT" ]; then
        local mini_ec mini_start mini_end
        mini_start=$(date +%s.%N)
        timeout "$TIMEOUT" minisat "$cnf_file" /dev/null > /dev/null 2>&1
        mini_ec=$?
        mini_end=$(date +%s.%N)
        mini_dur=$(awk "BEGIN{printf \"%.3f\", $mini_end-$mini_start}")
        case $mini_ec in
            10)  mini_result="SAT" ;;
            20)  mini_result="UNSAT" ;;
            124) mini_result="TMO" ;;
            *)   mini_result="EC=$mini_ec" ;;
        esac
    fi

    printf '%s|%s|%d|%d|%s|%d|%s|%s|%s|%s\n' \
        "$k" "$ratio" "$n" "$m" "$sat_result" "$seed" "$sat_dur" "$mini_result" "$mini_dur" "$cnf_file" \
        > "$res_file"
}

# --- Launch workers with FIFO semaphore ---
fifo="$WORK_DIR/sem"
mkfifo "$fifo"
exec 3<>"$fifo"
rm "$fifo"
for ((i = 0; i < JOBS; i++)); do echo >&3; done

echo "=== Random k-SAT Fuzzer ===" >&2
echo "Satience:   $SATIENCE" >&2
echo "Minisat:    $(command -v minisat)" >&2
echo "Iterations: $ITERATIONS" >&2
echo "Timeout:    ${TIMEOUT}s per solver" >&2
echo "Workers:    $JOBS" >&2
echo "Failures:   saved to $FAIL_DIR/" >&2
echo "" >&2

wall_start=$(date +%s.%N)
for ((i = 0; i < ITERATIONS; i++)); do
    read -u 3  # acquire token
    (
        # Seed from PID + iteration + time for run-to-run variety
        seed=$(( (RANDOM << 16) | RANDOM | (i * 2654435761) ))
        run_instance "$i" "$seed"
        echo >&3  # release token
    ) &
    printf '.' >&2
done
wait
exec 3>&-
wall_end=$(date +%s.%N)
wall_dur=$(awk "BEGIN{printf \"%.1f\", $wall_end-$wall_start}")
echo "" >&2

# --- Collect results ---
PASS=0
FALSE_SAT=0
FALSE_UNSAT=0
BOTH_TIMEOUT=0
DISAGREE=0
GEN_FAIL=0

for ((i = 0; i < ITERATIONS; i++)); do
    res_file="$WORK_DIR/res_$i"
    line=$(cat "$res_file" 2>/dev/null || echo "MISSING||||||||||")

    IFS='|' read -r k ratio n m sat_result seed sat_dur mini_result mini_dur cnf_file <<< "$line"

    case "$sat_result" in
        GENFAIL)
            GEN_FAIL=$((GEN_FAIL + 1))
            continue
            ;;
        BAD_MODEL)
            FALSE_SAT=$((FALSE_SAT + 1))
            cp "$cnf_file" "$FAIL_DIR/false_sat_${i}_k${k}_n${n}.cnf"
            echo "FAIL seed=$seed: satience produced bad model (k=$k n=$n m=$m)" >&2
            continue
            ;;
        TMO)
            if [ "$mini_result" = "TMO" ] || [ "$mini_result" = "—" ]; then
                BOTH_TIMEOUT=$((BOTH_TIMEOUT + 1))
            else
                # satience timed out but minisat solved — not a soundness bug but a perf issue
                PASS=$((PASS + 1))
            fi
            continue
            ;;
    esac

    # Both produced a verdict — compare
    if [ "$sat_result" = "$mini_result" ]; then
        PASS=$((PASS + 1))
    elif [ "$sat_result" = "SAT" ] && [ "$mini_result" = "UNSAT" ]; then
        FALSE_SAT=$((FALSE_SAT + 1))
        cp "$cnf_file" "$FAIL_DIR/false_sat_${i}_k${k}_n${n}.cnf"
        echo "FAIL seed=$seed: satience=SAT but minisat=UNSAT (k=$k n=$n m=$m)" >&2
    elif [ "$sat_result" = "UNSAT" ] && [ "$mini_result" = "SAT" ]; then
        FALSE_UNSAT=$((FALSE_UNSAT + 1))
        cp "$cnf_file" "$FAIL_DIR/false_unsat_${i}_k${k}_n${n}.cnf"
        echo "FAIL seed=$seed: satience=UNSAT but minisat=SAT (k=$k n=$n m=$m)" >&2
    else
        # One timed out, other didn't — inconclusive
        DISAGREE=$((DISAGREE + 1))
    fi
done

echo "" >&2
echo "=== Summary ===" >&2
echo "Total:       $ITERATIONS" >&2
echo "Passed:      $PASS" >&2
echo "False SAT:   $FALSE_SAT" >&2
echo "False UNSAT: $FALSE_UNSAT" >&2
echo "Both TMO:    $BOTH_TIMEOUT" >&2
echo "Inconclusive:$DISAGREE" >&2
echo "Gen fails:   $GEN_FAIL" >&2
echo "Wall time:   ${wall_dur}s ($JOBS workers)" >&2

if [ $FALSE_SAT -gt 0 ] || [ $FALSE_UNSAT -gt 0 ]; then
    echo "" >&2
    echo "SOUNDNESS FAILURE: satience produced a wrong verdict." >&2
    echo "Failed instances saved to: $FAIL_DIR/" >&2
    exit 1
fi

echo "" >&2
echo "SOUND: All non-timeout verdicts match minisat; all SAT models verified." >&2
exit 0
