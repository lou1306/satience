#!/usr/bin/env bash
# Deterministic divergence study: does satience lose on UNSAT vs SAT vs minisat?
# Captures time + conflicts/decisions/props from BOTH solvers so we can separate
# search coverage (conflict count) from per-step overhead (time/conflict).
#
# Usage: bash benchmark/divergence_unsat.sh [per-solver-timeout] [JOBS]
#   emits a TSV on stdout: label family verdict_sat verdict_mini sat_s mini_s
#                            sat_conf mini_conf sat_dec mini_dec sat_prop mini_prop

set -u
TIMEOUT_SEC=${1:-15}
JOBS=${JOBS:-6}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"
MINISAT="minisat"
SEED=42

if [ ! -x "$SATIENCE" ]; then
    (cd "$REPO_ROOT" && GOAMD64=v3 go build -o satience_bench ./cmd/satience) || { echo "build fail"; exit 1; }
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# --- Balanced instance set: known verdicts, sized so both solve within timeout.
# format: label|fam|<args...>
INSTANCES=(
    # UNSAT proof-complexity families
    "php87|php|php 8 7"
    "php98|php|php 9 8"
    "bphp65|php|bphp 6 5"
    "bphp87|php|bphp 8 7"
    "ts66|tseitin|tseitin randomodd grid 6 6"
    "ts77|tseitin|tseitin randomodd grid 7 7"
    "pa9|parity|parity 9"
    "pa11|parity|parity 11"
    "op8|op|op 8"
    "op10|op|op 10"
    "op12|op|op 12"
    "cnt10|count|count 10 3"
    "cnt13|count|count 13 3"
    "st35|stone|stone 3 pyramid 5"
    "st36|stone|stone 3 pyramid 6"
    "kcl5|kclique|kclique 5 gnp 30 0.3"
    "kcl6|kclique|kclique 6 gnp 30 0.3"
    "pb8|peb|peb pyramid 8"
    "pb10|peb|peb pyramid 10"
    "pb30|peb|peb path 30"
    # SAT families
    "r3lo100|rand|randkcnf 3 100 350"
    "r3lo150|rand|randkcnf 3 150 525"
    "r3lo200|rand|randkcnf 3 200 700"
    "kc66|kcolor|kcolor 3 grid 6 6"
    "kc77|kcolor|kcolor 3 grid 7 7"
    "pa20|parity|parity 20"
    "alx20|algebrax|algebra_xor_20"
    "alx40|algebrax|algebra_xor_40"
)

run_solver() {
    # $1=which(cnfgen-ish args) -> generates file, times both
    local label="$1" fam="$2" cnfargs="$3"
    local cnf="$WORK/$label.cnf"
    # shellcheck disable=SC2086
    if [ "$fam" = "algebrax" ]; then
        cnf="$SCRIPT_DIR/gbd_instances/algebra_xor_${label#alx}_sat.cnf"
    else
        cnfgen -S $SEED $cnfargs > "$cnf" 2>/dev/null || { echo "$label|$fam|GENFAIL"; return; }
    fi

    # --- satience ---
    local s_start s_end s_dur s_ec s_line s_conf s_dec s_prop s_res
    s_start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$SATIENCE" -stats=1000000000 "$cnf" > "$WORK/s$label.out" 2>&1
    s_ec=$?
    s_end=$(date +%s.%N)
    s_dur=$(awk "BEGIN{printf \"%.4f\", $s_end-$s_start}")
    case $s_ec in
        10) s_res=SAT ;; 20) s_res=UNSAT ;; 124) s_res=TMO ;; *) s_res="EC$s_ec" ;;
    esac
    s_line=$(grep -oE "c \[final\] [0-9.]+s t=.*" "$WORK/s$label.out" 2>/dev/null | tail -1)
    s_line=$(grep -oE "c \[final\] .*" "$WORK/s$label.out" | tail -1)
    s_conf=$(echo "$s_line" | grep -oE "conflicts=[0-9]+" | grep -oE "[0-9]+")
    s_dec=$(echo "$s_line" | grep -oE "decisions=[0-9]+" | grep -oE "[0-9]+")
    s_prop=$(echo "$s_line" | grep -oE "props=[0-9]+" | grep -oE "[0-9]+")
    [ -z "$s_conf" ] && s_conf=0; [ -z "$s_dec" ] && s_dec=0; [ -z "$s_prop" ] && s_prop=0

    # --- minisat ---
    local m_start m_end m_dur m_ec m_conf m_dec m_prop m_res mout
    m_start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" minisat -verb=2 "$cnf" /dev/null > "$WORK/m$label.out" 2>&1
    m_ec=$?
    m_end=$(date +%s.%N)
    m_dur=$(awk "BEGIN{printf \"%.4f\", $m_end-$m_start}")
    case $m_ec in
        10) m_res=SAT ;; 20) m_res=UNSAT ;; 124) m_res=TMO ;; *) m_res="EC$m_ec" ;;
    esac
    mout=$(cat "$WORK/m$label.out")
    m_conf=$(echo "$mout" | grep -oE "conflicts *: *[0-9]+" | grep -oE "[0-9]+" | head -1)
    m_dec=$(echo "$mout" | grep -oE "decisions *: *[0-9]+" | grep -oE "[0-9]+" | head -1)
    m_prop=$(echo "$mout" | grep -oE "propagations *: *[0-9]+" | grep -oE "[0-9]+" | head -1)
    [ -z "$m_conf" ] && m_conf=0; [ -z "$m_dec" ] && m_dec=0; [ -z "$m_prop" ] && m_prop=0

    echo "$label|$fam|$s_res|$m_res|$s_dur|$m_dur|$s_conf|$m_conf|$s_dec|$m_dec|$s_prop|$m_prop"
}

# --- worker pool ---
fifo="$WORK/sem"; mkfifo "$fifo"; exec 3<>"$fifo"; rm "$fifo"
for ((i=0;i<JOBS;i++)); do echo >&3; done

declare -a LABELS
for entry in "${INSTANCES[@]}"; do
    read -u 3
    label="${entry%%|*}"; rest="${entry#*|}"
    fam="${rest%%|*}"; args="${rest#*|}"
    LABELS+=("$label")
    ( run_solver "$label" "$fam" "$args" > "$WORK/res_$label"; echo >&3 ) &
done
wait
exec 3>&-

for label in "${LABELS[@]}"; do
    cat "$WORK/res_$label" 2>/dev/null || true
done
