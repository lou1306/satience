#!/bin/bash
# fuzz_structured.sh — Structured random soundness fuzzer for satience.
#
# Generates fresh structured CNF instances (random graphs: Tseitin, k-coloring,
# k-clique) with varying seeds each run. Two check modes:
#
#   Known-answer families (tseitin randomodd): check satience's verdict
#   directly against the mathematically guaranteed answer (UNSAT).
#
#   Unknown-answer families (kcolor, kclique): cross-check satience against
#   minisat. Catches false SAT (bad model via -verify) and false UNSAT.
#
# On any mismatch, the CNF file and metadata are saved to fuzz_failures/.
#
# Usage: bash benchmark/fuzz_structured.sh
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

# Instance matrix: "expected|cnfgen args..."
# expected = UNSAT for known-answer families, "ORACLE" for minisat cross-check.
MATRIX=(
    # === Tseitin odd-charge on random grid graphs (known UNSAT) ===
    "UNSAT|tseitin randomodd grid 5 5"
    "UNSAT|tseitin randomodd grid 6 6"
    "UNSAT|tseitin randomodd grid 7 7"
    "UNSAT|tseitin randomodd grid 8 8"
    "UNSAT|tseitin randomodd grid 9 9"
    # === Tseitin odd-charge on random regular graphs (known UNSAT) ===
    "UNSAT|tseitin randomodd gnd 50 6"
    "UNSAT|tseitin randomodd gnd 80 6"
    "UNSAT|tseitin randomodd gnd 100 8"
    "UNSAT|tseitin randomodd gnd 150 8"
    # === Tseitin shuffled variants ===
    "UNSAT|tseitin randomodd grid 6 6 -T shuffle"
    "UNSAT|tseitin randomodd grid 8 8 -T shuffle"
    "UNSAT|tseitin randomodd gnd 100 8 -T shuffle"
    # === k-coloring (answer depends on graph — cross-check with minisat) ===
    "ORACLE|kcolor 3 gnp 20 0.5"
    "ORACLE|kcolor 3 gnp 30 0.5"
    "ORACLE|kcolor 3 gnp 40 0.3"
    "ORACLE|kcolor 3 gnp 50 0.3"
    "ORACLE|kcolor 3 grid 6 6"
    "ORACLE|kcolor 3 grid 7 7"
    "ORACLE|kcolor 3 grid 8 8"
    "ORACLE|kcolor 4 gnp 30 0.5"
    "ORACLE|kcolor 4 gnp 50 0.4"
    # === k-clique (answer depends on graph — cross-check with minisat) ===
    "ORACLE|kclique 5 gnp 30 0.3"
    "ORACLE|kclique 5 gnp 40 0.2"
    "ORACLE|kclique 6 gnp 40 0.3"
    "ORACLE|kclique 6 gnp 50 0.25"
)

MATRIX_SIZE=${#MATRIX[@]}

# --- Worker ---
run_instance() {
    local idx="$1"
    local seed="$2"
    local entry="${MATRIX[$((idx % MATRIX_SIZE))]}"
    local expected="${entry%%|*}"
    local cnf_args="${entry#*|}"
    local cnf_file="$WORK_DIR/inst_${idx}.cnf"
    local res_file="$WORK_DIR/res_${idx}"

    if ! cnfgen -S "$seed" $cnf_args > "$cnf_file" 2>/dev/null; then
        printf 'GENFAIL|%s|%s|%d\n' "$expected" "$cnf_args" "$seed" > "$res_file"
        return
    fi

    # Run satience with -verify (catches false SAT via bad model)
    local sat_ec sat_start sat_end
    sat_start=$(date +%s.%N)
    timeout "$TIMEOUT" "$SATIENCE" -verify "$cnf_file" > /dev/null 2>&1
    sat_ec=$?
    sat_end=$(date +%s.%N)
    local sat_dur
    sat_dur=$(awk "BEGIN{printf \"%.3f\", $sat_end-$sat_start}")

    local sat_result
    case $sat_ec in
        10)  sat_result="SAT" ;;
        20)  sat_result="UNSAT" ;;
        124) sat_result="TMO" ;;
        1)   sat_result="BAD_MODEL" ;;
        *)   sat_result="EC=$sat_ec" ;;
    esac

    # For ORACLE instances, run minisat regardless of satience's verdict.
    # For known-answer instances, only run minisat if satience disagrees
    # (to confirm the mismatch).
    local mini_result="—" mini_dur="0.000"
    if [ "$expected" = "ORACLE" ] && { [ "$sat_result" = "SAT" ] || [ "$sat_result" = "UNSAT" ]; }; then
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

    printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "$expected" "$cnf_args" "$sat_result" "$sat_dur" "$mini_result" "$mini_dur" "$seed" "$cnf_file" \
        > "$res_file"
}

# --- Launch workers with FIFO semaphore ---
fifo="$WORK_DIR/sem"
mkfifo "$fifo"
exec 3<>"$fifo"
rm "$fifo"
for ((i = 0; i < JOBS; i++)); do echo >&3; done

echo "=== Structured Random Fuzzer ===" >&2
echo "Satience:   $SATIENCE" >&2
echo "Minisat:    $(command -v minisat)" >&2
echo "Iterations: $ITERATIONS" >&2
echo "Timeout:    ${TIMEOUT}s per solver" >&2
echo "Workers:    $JOBS" >&2
echo "Failures:   saved to $FAIL_DIR/" >&2
echo "" >&2

wall_start=$(date +%s.%N)
for ((i = 0; i < ITERATIONS; i++)); do
    read -u 3
    (
        seed=$(( (RANDOM << 16) | RANDOM | (i * 2654435761) ))
        run_instance "$i" "$seed"
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
PASS=0
FALSE_SAT=0
FALSE_UNSAT=0
BAD_MODEL=0
BOTH_TIMEOUT=0
GEN_FAIL=0

for ((i = 0; i < ITERATIONS; i++)); do
    res_file="$WORK_DIR/res_$i"
    line=$(cat "$res_file" 2>/dev/null || echo "MISSING|||||||")

    IFS='|' read -r expected cnf_args sat_result sat_dur mini_result mini_dur seed cnf_file <<< "$line"

    case "$sat_result" in
        GENFAIL)
            GEN_FAIL=$((GEN_FAIL + 1))
            continue
            ;;
        BAD_MODEL)
            BAD_MODEL=$((BAD_MODEL + 1))
            cp "$cnf_file" "$FAIL_DIR/bad_model_${i}.cnf"
            echo "FAIL seed=$seed: satience produced bad model ($cnf_args)" >&2
            continue
            ;;
        TMO)
            BOTH_TIMEOUT=$((BOTH_TIMEOUT + 1))
            continue
            ;;
    esac

    if [ "$expected" = "ORACLE" ]; then
        # Cross-check with minisat
        if [ "$mini_result" = "TMO" ] || [ "$mini_result" = "—" ]; then
            BOTH_TIMEOUT=$((BOTH_TIMEOUT + 1))
        elif [ "$sat_result" = "$mini_result" ]; then
            PASS=$((PASS + 1))
        elif [ "$sat_result" = "SAT" ] && [ "$mini_result" = "UNSAT" ]; then
            FALSE_SAT=$((FALSE_SAT + 1))
            cp "$cnf_file" "$FAIL_DIR/false_sat_${i}.cnf"
            echo "FAIL seed=$seed: satience=SAT but minisat=UNSAT ($cnf_args)" >&2
        elif [ "$sat_result" = "UNSAT" ] && [ "$mini_result" = "SAT" ]; then
            FALSE_UNSAT=$((FALSE_UNSAT + 1))
            cp "$cnf_file" "$FAIL_DIR/false_unsat_${i}.cnf"
            echo "FAIL seed=$seed: satience=UNSAT but minisat=SAT ($cnf_args)" >&2
        else
            BOTH_TIMEOUT=$((BOTH_TIMEOUT + 1))
        fi
    else
        # Known-answer family
        if [ "$sat_result" = "$expected" ]; then
            PASS=$((PASS + 1))
        elif [ "$sat_result" = "SAT" ] && [ "$expected" = "UNSAT" ]; then
            FALSE_SAT=$((FALSE_SAT + 1))
            cp "$cnf_file" "$FAIL_DIR/false_sat_${i}.cnf"
            echo "FAIL seed=$seed: satience=SAT but expected=UNSAT ($cnf_args)" >&2
        elif [ "$sat_result" = "UNSAT" ] && [ "$expected" = "SAT" ]; then
            FALSE_UNSAT=$((FALSE_UNSAT + 1))
            cp "$cnf_file" "$FAIL_DIR/false_unsat_${i}.cnf"
            echo "FAIL seed=$seed: satience=UNSAT but expected=SAT ($cnf_args)" >&2
        fi
    fi
done

echo "" >&2
echo "=== Summary ===" >&2
echo "Total:       $ITERATIONS" >&2
echo "Passed:      $PASS" >&2
echo "False SAT:   $FALSE_SAT" >&2
echo "False UNSAT: $FALSE_UNSAT" >&2
echo "Bad models:  $BAD_MODEL" >&2
echo "Both TMO:    $BOTH_TIMEOUT" >&2
echo "Gen fails:   $GEN_FAIL" >&2
echo "Wall time:   ${wall_dur}s ($JOBS workers)" >&2

if [ $FALSE_SAT -gt 0 ] || [ $FALSE_UNSAT -gt 0 ] || [ $BAD_MODEL -gt 0 ]; then
    echo "" >&2
    echo "SOUNDNESS FAILURE: satience produced a wrong verdict." >&2
    echo "Failed instances saved to: $FAIL_DIR/" >&2
    exit 1
fi

echo "" >&2
echo "SOUND: All non-timeout verdicts correct; all SAT models verified." >&2
exit 0
