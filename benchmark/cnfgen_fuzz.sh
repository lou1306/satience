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
# Usage: bash benchmark/cnfgen_fuzz.sh [timeout_sec]
#   timeout_sec  per-instance timeout in seconds (default 10)
#
# Requires: cnfgen (pipx install cnfgen), GOAMD64=v3 go build for satience.

set -u

TIMEOUT_SEC=${1:-10}
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

TMP_FILE=$(mktemp --suffix=.cnf)
trap 'rm -f "$TMP_FILE"' EXIT

# Instance suite: "expected_verdict|cnfgen args..."
# Expected verdict is SAT or UNSAT (mathematically guaranteed for these params).
# The -T shuffle transform randomizes variable/polarity/clause order while
# preserving satisfiability, adding run-to-run variety.
INSTANCES=(
    # UNSAT — pigeonhole principle (pigeons > holes)
    "UNSAT|php 6 5"
    "UNSAT|php 8 7"
    "UNSAT|php --functional 6 5"
    # UNSAT — parity principle (odd count cannot pair up)
    "UNSAT|parity 7"
    # UNSAT — counting principle (N not divisible by d)
    "UNSAT|count 10 4"
    # UNSAT — Tseitin (odd charge on regular/grid graphs)
    "UNSAT|tseitin 10"
    "UNSAT|tseitin 20"
    "UNSAT|tseitin randomodd grid 5 5"
    "UNSAT|tseitin randomodd grid 8 8"
    # UNSAT — ordering principle (finite linear order has a min element)
    "UNSAT|op 5"
    "UNSAT|op 10"
    # UNSAT — pebbling (pyramid DAG contradiction)
    "UNSAT|peb pyramid 5"
    "UNSAT|peb pyramid 8"
    # SAT — parity principle (even count pairs up)
    "SAT|parity 8"
    "SAT|parity 10"
    # SAT — counting principle (N divisible by d)
    "SAT|count 9 3"
    "SAT|count 12 4"
    # SAT — pigeonhole (pigeons <= holes)
    "SAT|php 5 6"
    "SAT|php 4 5"
    # UNSAT — shuffled variants (variable/polarity/clause order randomized)
    "UNSAT|php 6 5 -T shuffle"
    "UNSAT|tseitin 10 -T shuffle"
    "UNSAT|op 8 -T shuffle"
)

TOTAL=${#INSTANCES[@]}
PASS=0
FALSE_SAT=0       # satience=SAT, expected UNSAT
FALSE_UNSAT=0     # satience=UNSAT, expected SAT
TIMEOUT_COUNT=0
VERIFY_FAIL=0     # satience=SAT but model does not satisfy clauses

echo "=== CNFgen Soundness Suite ==="
echo "Satience: $SATIENCE"
echo "Timeout:  ${TIMEOUT_SEC}s per instance"
echo "Instances: $TOTAL"
echo ""

for entry in "${INSTANCES[@]}"; do
    expected="${entry%%|*}"
    cnf_args="${entry#*|}"

    if ! cnfgen $cnf_args > "$TMP_FILE" 2>/dev/null; then
        echo "FAIL  [gen]  $cnf_args  (cnfgen failed)"
        FALSE_SAT=$((FALSE_SAT + 1))
        continue
    fi

    n_vars=$(grep "^p cnf" "$TMP_FILE" | awk '{print $3}')
    n_clauses=$(grep "^p cnf" "$TMP_FILE" | awk '{print $4}')

    timeout "$TIMEOUT_SEC" "$SATIENCE" -verify "$TMP_FILE" > /dev/null 2>&1
    ec=$?

    case $ec in
        10)  result="SAT" ;;
        20)  result="UNSAT" ;;
        124) result="TIMEOUT" ;;
        1)   result="SAT_BAD_MODEL" ;;   # -verify returned non-zero (bad model)
        *)   result="EC=$ec" ;;
    esac

    if [ "$result" = "TIMEOUT" ]; then
        symbol="?"
        TIMEOUT_COUNT=$((TIMEOUT_COUNT + 1))
        status="timeout"
    elif [ "$result" = "SAT_BAD_MODEL" ]; then
        symbol="!"
        VERIFY_FAIL=$((VERIFY_FAIL + 1))
        status="BAD MODEL (SAT but model does not satisfy clauses)"
    elif [ "$result" = "$expected" ]; then
        symbol="."
        PASS=$((PASS + 1))
        status="ok"
    else
        symbol="!"
        if [ "$expected" = "UNSAT" ]; then
            FALSE_SAT=$((FALSE_SAT + 1))
            status="FALSE SAT (expected UNSAT, got SAT)"
        else
            FALSE_UNSAT=$((FALSE_UNSAT + 1))
            status="FALSE UNSAT (expected SAT, got UNSAT)"
        fi
    fi

    printf "%s  %-40s %5sv %7sc  %-7s exp=%-5s  %s\n" \
        "$symbol" "$cnf_args" "$n_vars" "$n_clauses" "$result" "$expected" "$status"
done

echo ""
echo "=== Summary ==="
echo "Total:     $TOTAL"
echo "Passed:    $PASS"
echo "Timeouts:  $TIMEOUT_COUNT"
echo "False SAT:    $FALSE_SAT"
echo "False UNSAT:  $FALSE_UNSAT"
echo "Bad models:   $VERIFY_FAIL"

if [ $FALSE_SAT -gt 0 ] || [ $FALSE_UNSAT -gt 0 ] || [ $VERIFY_FAIL -gt 0 ]; then
    echo ""
    echo "SOUNDNESS FAILURE: satience produced a wrong verdict or bad model."
    exit 1
fi

echo ""
echo "SOUND: All non-timeout verdicts match the expected answer; all SAT models verified."
exit 0
