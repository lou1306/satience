#!/bin/bash
# CNFgen-based soundness harness for satience.
#
# Generates known-answer CNF instances using CNFgen (proof-complexity formula
# families: pigeonhole, Tseitin, ordering principle, counting, parity,
# pebbling) and checks satience's verdict against the mathematically
# guaranteed answer. SAT models are verified against the DIMACS clauses
# via `satience -verify`.
#
# This catches both false SAT (satience=SAT on an UNSAT instance) and false
# UNSAT (satience=UNSAT on a SAT instance) inline, with no minisat dependency.
# The minisat cross-check (benchmark/cross_check_minisat.sh) complements this
# on real GBD instances.
#
# Instances run in parallel (default 6 workers) to keep wall time low despite
# the ~115-instance suite. Each family has >=5 instances spanning multiple sizes
# and SAT/UNSAT verdicts where applicable.
#
# Usage: bash benchmark/cnfgen_fuzz.sh [timeout_sec]
#   timeout_sec  per-instance timeout in seconds (default 30)
#   JOBS env var controls parallelism (default 6)
#
# Requires: cnfgen (pipx install cnfgen), GOAMD64=v3 go build for satience.

set -u

TIMEOUT_SEC=${1:-30}
JOBS=${JOBS:-6}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"

if ! command -v cnfgen >/dev/null 2>&1; then
    echo "ERROR: cnfgen not found. Install with: pipx install cnfgen" >&2
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

# Instance suite: "expected_verdict|cnfgen args..."
# Expected verdict is SAT or UNSAT (mathematically guaranteed for these params).
# The -T shuffle transform randomizes variable/polarity/clause order while
# preserving satisfiability, adding run-to-run variety.
#
# Families (each >=5 instances):
#   pigeonhole (php) — SAT + UNSAT, basic/functional/onto variants
#   tseitin       — random regular + grid graphs (UNSAT)
#   ordering (op) — UNSAT
#   counting      — SAT + UNSAT
#   parity        — SAT (even) + UNSAT (odd)
#   pebbling (peb)— pyramid + tree + path DAGs (UNSAT)
INSTANCES=(
    # === Pigeonhole principle (php) ===
    # UNSAT: more pigeons than holes
    "UNSAT|php 6 5"
    "UNSAT|php 7 6"
    "UNSAT|php 8 7"
    "UNSAT|php 9 8"
    "UNSAT|php --functional 6 5"
    "UNSAT|php --functional 7 6"
    "UNSAT|php --functional 8 7"
    "UNSAT|php --onto 6 5"
    "UNSAT|php --onto 7 6"
    "UNSAT|php --functional --onto 6 5"
    "UNSAT|php --functional --onto 7 6"
    # SAT: pigeons <= holes
    "SAT|php 3 4"
    "SAT|php 4 5"
    "SAT|php 5 6"
    "SAT|php 5 7"
    "SAT|php 6 7"
    "SAT|php 3 5"
    "SAT|php 4 6"
    "SAT|php 5 8"

    # === Tseitin (odd charge) ===
    "UNSAT|tseitin 10"
    "UNSAT|tseitin 12"
    "UNSAT|tseitin 15"
    "UNSAT|tseitin 20"
    "UNSAT|tseitin 25"
    "UNSAT|tseitin randomodd grid 4 4"
    "UNSAT|tseitin randomodd grid 5 5"
    "UNSAT|tseitin randomodd grid 6 6"
    "UNSAT|tseitin randomodd grid 7 7"
    "UNSAT|tseitin randomodd grid 8 8"
    "UNSAT|tseitin randomodd grid 9 9"

    # === Ordering principle (op) ===
    "UNSAT|op 5"
    "UNSAT|op 6"
    "UNSAT|op 7"
    "UNSAT|op 8"
    "UNSAT|op 9"
    "UNSAT|op 10"
    "UNSAT|op 12"
    "UNSAT|op 15"

    # === Counting principle (count) ===
    # UNSAT: N not divisible by d
    "UNSAT|count 7 3"
    "UNSAT|count 8 3"
    "UNSAT|count 10 3"
    "UNSAT|count 11 3"
    "UNSAT|count 10 4"
    "UNSAT|count 11 4"
    # SAT: N divisible by d
    "SAT|count 6 2"
    "SAT|count 6 3"
    "SAT|count 8 2"
    "SAT|count 8 4"
    "SAT|count 9 3"
    "SAT|count 10 2"
    "SAT|count 12 4"
    "SAT|count 14 2"
    "SAT|count 15 3"

    # === Parity principle (parity) ===
    # UNSAT: odd
    "UNSAT|parity 3"
    "UNSAT|parity 5"
    "UNSAT|parity 7"
    "UNSAT|parity 9"
    "UNSAT|parity 11"
    "UNSAT|parity 13"
    # SAT: even
    "SAT|parity 4"
    "SAT|parity 6"
    "SAT|parity 8"
    "SAT|parity 10"
    "SAT|parity 12"
    "SAT|parity 14"

    # === Pebbling (peb) ===
    # pyramid
    "UNSAT|peb pyramid 3"
    "UNSAT|peb pyramid 4"
    "UNSAT|peb pyramid 5"
    "UNSAT|peb pyramid 6"
    "UNSAT|peb pyramid 7"
    "UNSAT|peb pyramid 8"
    "UNSAT|peb pyramid 10"
    # tree
    "UNSAT|peb tree 3"
    "UNSAT|peb tree 4"
    "UNSAT|peb tree 5"
    "UNSAT|peb tree 6"
    "UNSAT|peb tree 7"
    "UNSAT|peb tree 8"
    "UNSAT|peb tree 10"
    # path
    "UNSAT|peb path 5"
    "UNSAT|peb path 8"
    "UNSAT|peb path 10"
    "UNSAT|peb path 15"
    "UNSAT|peb path 20"
    "UNSAT|peb path 25"
    "UNSAT|peb path 30"

    # === Shuffled variants (-T shuffle: randomized var/polarity/clause order) ===
    "UNSAT|php 6 5 -T shuffle"
    "UNSAT|php 8 7 -T shuffle"
    "UNSAT|tseitin 10 -T shuffle"
    "UNSAT|tseitin 20 -T shuffle"
    "UNSAT|tseitin randomodd grid 6 6 -T shuffle"
    "UNSAT|op 8 -T shuffle"
    "UNSAT|op 10 -T shuffle"
    "UNSAT|op 12 -T shuffle"
    "UNSAT|parity 7 -T shuffle"
    "UNSAT|parity 11 -T shuffle"
    "SAT|parity 8 -T shuffle"
    "SAT|parity 10 -T shuffle"
    "UNSAT|peb pyramid 5 -T shuffle"
    "UNSAT|peb pyramid 8 -T shuffle"
    "UNSAT|peb tree 6 -T shuffle"
    "UNSAT|count 10 4 -T shuffle"
    "UNSAT|count 11 3 -T shuffle"
    "SAT|count 9 3 -T shuffle"
    "SAT|count 8 4 -T shuffle"
    "UNSAT|php 7 6 -T shuffle"
    "UNSAT|peb path 20 -T shuffle"
    "UNSAT|parity 13 -T shuffle"
    "UNSAT|count 8 3 -T shuffle"
    "UNSAT|peb tree 8 -T shuffle"
)

TOTAL=${#INSTANCES[@]}

# --- Worker: generate + solve one instance, write result to $WORK_DIR/res_$idx ---
run_instance() {
    local idx="$1" entry="$2"
    local expected="${entry%%|*}"
    local cnf_args="${entry#*|}"
    local cnf_file="$WORK_DIR/inst_$idx.cnf"
    local res_file="$WORK_DIR/res_$idx"

    if ! cnfgen $cnf_args > "$cnf_file" 2>/dev/null; then
        printf 'GENFAIL|%s|0|0|0\n' "$cnf_args" > "$res_file"
        return
    fi

    local n_vars n_clauses
    n_vars=$(grep "^p cnf" "$cnf_file" | awk '{print $3}')
    n_clauses=$(grep "^p cnf" "$cnf_file" | awk '{print $4}')

    local start end dur ec result
    start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$SATIENCE" -verify "$cnf_file" > /dev/null 2>&1
    ec=$?
    end=$(date +%s.%N)
    dur=$(awk "BEGIN{printf \"%.2f\", $end-$start}")

    case $ec in
        10)  result="SAT" ;;
        20)  result="UNSAT" ;;
        124) result="TIMEOUT" ;;
        1)   result="SAT_BAD_MODEL" ;;
        *)   result="EC=$ec" ;;
    esac

    printf '%s|%s|%s|%s|%s|%s\n' \
        "$expected" "$cnf_args" "$n_vars" "$n_clauses" "$result" "$dur" > "$res_file"
}

# --- Launch workers with a FIFO semaphore limiting to $JOBS concurrent ---
fifo="$WORK_DIR/sem"
mkfifo "$fifo"
exec 3<>"$fifo"
rm "$fifo"
for ((i = 0; i < JOBS; i++)); do echo >&3; done

echo "=== CNFgen Soundness Suite ===" >&2
echo "Satience:  $SATIENCE" >&2
echo "Timeout:   ${TIMEOUT_SEC}s per instance" >&2
echo "Workers:   $JOBS" >&2
echo "Instances: $TOTAL" >&2
echo "" >&2

wall_start=$(date +%s.%N)
for i in "${!INSTANCES[@]}"; do
    read -u 3  # acquire a token
    (
        run_instance "$i" "${INSTANCES[$i]}"
        echo >&3  # release token
    ) &
    printf '.' >&2
done
wait
exec 3>&-
wall_end=$(date +%s.%N)
wall_dur=$(awk "BEGIN{printf \"%.1f\", $wall_end-$wall_start}")
echo "" >&2

# --- Collect results in table order ---
PASS=0
FALSE_SAT=0
FALSE_UNSAT=0
TIMEOUT_COUNT=0
VERIFY_FAIL=0
GEN_FAIL=0

# family|count|pass  accumulators (associative)
declare -A fam_total fam_pass

for ((i = 0; i < TOTAL; i++)); do
    res_file="$WORK_DIR/res_$i"
    line=$(cat "$res_file" 2>/dev/null || echo "MISSING|||||")

    IFS='|' read -r expected cnf_args n_vars n_clauses result dur <<< "$line"

    # derive family name from cnf_args first token
    fam="${cnf_args%% *}"
    case "$fam" in
        php)     fam="pigeonhole" ;;
        peb)     fam="pebbling" ;;
        op)      fam="ordering" ;;
        count)   fam="counting" ;;
        tseitin) fam="tseitin" ;;
        parity)  fam="parity" ;;
        *)       fam="$fam" ;;
    esac
    fam_total[$fam]=$(( ${fam_total[$fam]:-0} + 1 ))

    symbol="."
    status="ok"
    case "$result" in
        GENFAIL)
            symbol="G"
            status="gen failed"
            GEN_FAIL=$((GEN_FAIL + 1))
            ;;
        TIMEOUT)
            symbol="?"
            status="timeout"
            TIMEOUT_COUNT=$((TIMEOUT_COUNT + 1))
            ;;
        SAT_BAD_MODEL)
            symbol="!"
            status="BAD MODEL (SAT but model does not satisfy clauses)"
            VERIFY_FAIL=$((VERIFY_FAIL + 1))
            ;;
        "$expected")
            symbol="."
            PASS=$((PASS + 1))
            fam_pass[$fam]=$(( ${fam_pass[$fam]:-0} + 1 ))
            ;;
        *)
            symbol="!"
            if [ "$expected" = "UNSAT" ]; then
                FALSE_SAT=$((FALSE_SAT + 1))
                status="FALSE SAT (expected UNSAT, got $result)"
            else
                FALSE_UNSAT=$((FALSE_UNSAT + 1))
                status="FALSE UNSAT (expected SAT, got $result)"
            fi
            ;;
    esac

    printf "%s  %-44s %5sv %7sc  %-7s exp=%-5s  %s  %ss\n" \
        "$symbol" "$cnf_args" "$n_vars" "$n_clauses" "$result" "$expected" "$status" "$dur"
done

echo ""
echo "=== Per-family ==="
for fam in pigeonhole tseitin ordering counting parity pebbling; do
    t=${fam_total[$fam]:-0}
    p=${fam_pass[$fam]:-0}
    printf "  %-12s %3d instances, %3d passed\n" "$fam" "$t" "$p"
done

echo ""
echo "=== Summary ==="
echo "Total:       $TOTAL"
echo "Passed:      $PASS"
echo "Timeouts:    $TIMEOUT_COUNT"
echo "Gen fails:   $GEN_FAIL"
echo "False SAT:      $FALSE_SAT"
echo "False UNSAT:    $FALSE_UNSAT"
echo "Bad models:     $VERIFY_FAIL"
echo "Wall time:   ${wall_dur}s  ($JOBS workers)"

if [ $FALSE_SAT -gt 0 ] || [ $FALSE_UNSAT -gt 0 ] || [ $VERIFY_FAIL -gt 0 ] || [ $GEN_FAIL -gt 0 ]; then
    echo ""
    echo "SOUNDNESS FAILURE: satience produced a wrong verdict or bad model."
    exit 1
fi

echo ""
echo "SOUND: All non-timeout verdicts match the expected answer; all SAT models verified."
exit 0
