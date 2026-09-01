#!/bin/bash
# minisat_simponly.sh — find cnfgen families that MiniSat solves THROUGH
# SIMPLIFICATION/PREPROCESSING ONLY (variable elimination / subsumption), by
# A/B-ing minisat with pre ON (default) vs OFF (-no-pre), plus satience.
#
# Signals of "simplification-only":
#   * Mpre solves but MnoPre is TMO / much slower  (dependency on simplification)
#   * Mpre solves with ~0 search conflicts (whole verdict reached in preprocess)
#   * minisat actually eliminated variables (elimination-left decreases)
#
# Usage: bash benchmark/minisat_simponly.sh [timeout]   (JOBS env, default 4)

set -u
TIMEOUT_SEC=${1:-15}
JOBS=${JOBS:-4}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"
SEED=${SEED:-7}

INSTANCES=(
    # substitution / cardinality / XOR families (classic preprocessing targets)
    "subsetcard_10|subsetcard 10"
    "subsetcard_12|subsetcard 12"
    "subsetcard_15|subsetcard 15"
    "rxor_3_80_100|randkxor 3 80 100"
    "rxor_3_120_150|randkxor 3 120 150"
    "rxor_3_160_200|randkxor 3 160 200"
    "rxor_p_3_80_100|randkxor -p 3 80 100"
    "rxor_p_3_120_150|randkxor -p 3 120 150"
    "rxor_p_4_80_100|randkxor -p 4 80 100"
    # under-constrained SAT (preprocess/unit-prop finds model)
    "r3_200_600|randkcnf 3 200 600"
    "r3_250_750|randkcnf 3 250 750"
    "r3_300_900|randkcnf 3 300 900"
    # baseline structured families (should NOT be preprocess-dependent)
    "op_15|op 15"
    "op_20|op 20"
    "php_10|php 10 9"
    "rphp_10_8_8|rphp 10 8 8"
    "count_13_3|count 13 3"
    "tseitin_gnd_50_6|tseitin randomodd gnd 50 6"
    "kcolor3_g30|kcolor 3 gnp 30 0.5"
    "r3_150_639|randkcnf 3 150 639"
    "r4_100_980|randkcnf 4 100 980"
)

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

run_one(){
    local idx="$1" entry="$2"
    local label="${entry%%|*}" args="${entry#*|}"
    local cnf="$WORK/i_$idx.cnf" res="$WORK/r_$idx"
    cnfgen -S $((SEED+idx)) $args > "$cnf" 2>/dev/null || { printf '%s|GENFAIL|0|0||||||\n' "$label" > "$res"; return; }
    local nv nc; nv=$(grep '^p cnf' "$cnf"|awk '{print $3}'); nc=$(grep '^p cnf' "$cnf"|awk '{print $4}')

    local s e d ec
    local ar at ac alp
    s=$(date +%s.%N); timeout "$TIMEOUT_SEC" minisat "$cnf" >"$WORK/ap" 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\",$e-$s}"); case $ec in 10)ar=SAT;;20)ar=UNSAT;;124)ar=TMO;;*)ar="EC$ec";;esac; at=$d
    ac=$(grep -oE 'conflicts[ ]*:[ ]*[0-9]+' "$WORK/ap"|grep -oE '[0-9]+'|head -1); [ -z "$ac" ] && ac=-
    # vars eliminated = drop between consecutive "elimination left:" values
    local elim=0 prev=-1
    while read -r v; do [ "$v" -ge 0 ] 2>/dev/null || continue; [ $prev -ge 0 ] && [ "$v" -lt "$prev" ] && elim=$((elim + prev - v)); prev=$v; done \
      < <(grep -oE 'elimination left:[ ]*[0-9]+' "$WORK/ap"|grep -oE '[0-9]+')
    alp=$elim

    local br bt bc
    s=$(date +%s.%N); timeout "$TIMEOUT_SEC" minisat -no-pre "$cnf" >"$WORK/bp" 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\",$e-$s}"); case $ec in 10)br=SAT;;20)br=UNSAT;;124)br=TMO;;*)br="EC$ec";;esac; bt=$d
    bc=$(grep -oE 'conflicts[ ]*:[ ]*[0-9]+' "$WORK/bp"|grep -oE '[0-9]+'|head -1); [ -z "$bc" ] && bc=-

    local cr ct
    s=$(date +%s.%N); timeout "$TIMEOUT_SEC" "$SATIENCE" "$cnf" >/dev/null 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\",$e-$s}"); case $ec in 10)cr=SAT;;20)cr=UNSAT;;124)cr=TMO;;*)cr="EC$ec";;esac; ct=$d

    printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "$label" "$nv" "$nc" "$ar" "$at" "$ac" "$br" "$bt" "$bc" "$cr" "$ct" "$alp" > "$res"
}

fifo="$WORK/sem"; mkfifo "$fifo"; exec 3<>"$fifo"; rm "$fifo"
for ((i=0;i<JOBS;i++)); do echo >&3; done

echo "=== MiniSat simplification-only divergence (pre on/off) vs satience ==="
echo "col: label | Mp(t,conf) | Mnopre(t,conf) | sat(t) | varsEliminated"; echo ""
for i in "${!INSTANCES[@]}"; do read -u 3; ( run_one "$i" "${INSTANCES[$i]}"; echo >&3 ) & done
wait; exec 3>&-

printf "%-18s %6s %6s | %-5s %7s %7s | %-5s %7s %7s | %-5s %7s | %6s\n" \
    "label" "vars" "cls" "Mpre" "t" "conf" "Mno" "t" "conf" "sat" "t" "elim"
echo "--------------------------------------------------------------------------------------------------"
declare -a cats=()
for ((i=0;i<${#INSTANCES[@]};i++)); do
    IFS='|' read -r label nv nc ar at ac br bt bc cr ct alp < "$WORK/r_$i"
    printf "%-18s %6s %6s | %-5s %7s %7s | %-5s %7s %7s | %-5s %7s | %6s\n" \
        "$label" "$nv" "$nc" "$ar" "$at" "$ac" "$br" "$bt" "$bc" "$cr" "$ct" "$alp"
    if [ "$ar" != TMO ] && [ "$br" != TMO ] && [ "$ar" != EC* ] && [ "$br" != EC* ]; then
        dep=""
        awk "BEGIN{exit !( $at < 0.30 && $bt > 3*$at && $bt > 0.3)}" && dep=$dep"TIME"
        #[ "$ac" != - ] && [ "$bc" != - ] && [ "$ac" -le 1 ] && [ "$bc" -gt 20 ] && dep=$dep"CONF0"
        [ -n "$dep" ] && cats+=("$label|TIME")
    fi
done
echo ""
echo "=== Preprocessing-DEPENDENT (minisat -no-pre much slower than pre) ==="
[ ${#cats[@]} -eq 0 ] && echo "  (none)"
printf '%s\n' "${cats[@]:-}" | sed 's/^/  /'
