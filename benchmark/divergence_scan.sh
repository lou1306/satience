#!/bin/bash
# Broad catastrophic-divergence scan: Satience (current HEAD) vs MiniSat.
#
# Sweeps ~25 CNFgen families across a size dimension with fresh deterministic
# draws (-S <seed>). Flags "catastrophic" divergences:
#   (sat_time >= 1s AND mini_time < 0.5s)   or   (sat=TMO AND mini solved)
# and also captures satience conflict/decision counts so a flagged cell's
# search-quality gap (high conflict ratio) vs per-step overhead (high time/conf)
# can be attributed immediately.
#
# Usage: bash benchmark/divergence_scan.sh [timeout_sec]
#   timeout_sec  per-instance per-solver timeout (default 15)
#   JOBS env var  parallelism (default 6)
#   SEED env var  cnfgen seed base (default 7)
#

set -u
TIMEOUT_SEC=${1:-15}
JOBS=${JOBS:-6}
SEED=${SEED:-7}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"
MINISAT="minisat"

if ! command -v cnfgen >/dev/null 2>&1; then echo "ERROR: cnfgen missing"; exit 1; fi
if ! command -v "$MINISAT" >/dev/null 2>&1; then echo "ERROR: minisat missing"; exit 1; fi
if [ ! -x "$SATIENCE" ]; then echo "ERROR: build satience_bench first"; exit 1; fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# label|cnfgen args (options after -S seed; seed appended by runner)
INSTANCES=(
    # === Random 3-SAT, ratios across the transition ===
    "r3_50_215|randkcnf 3 50 215"
    "r3_100_426|randkcnf 3 100 426"
    "r3_150_639|randkcnf 3 150 639"
    "r3_200_852|randkcnf 3 200 852"
    "r3_250_1065|randkcnf 3 250 1065"
    "r3_hi_100_480|randkcnf 3 100 480"
    "r3_hi_150_720|randkcnf 3 150 720"
    "r3_hi_200_960|randkcnf 3 200 960"
    "r3_lo_150_525|randkcnf 3 150 525"
    # === Random 4-SAT ===
    "r4_75_735|randkcnf 4 75 735"
    "r4_100_980|randkcnf 4 100 980"
    # === Pigeonhole family ===
    "php_8|php 8 7"
    "php_10|php 10 9"
    "php_12|php 12 11"
    "php_func_10|php --functional 10 9"
    "php_onto_10|php --onto 10 9"
    "bphp_8|bphp 8 7"
    "bphp_10|bphp 10 9"
    "rphp_5_4_4|rphp 5 4 4"
    "rphp_7_6_6|rphp 7 6 6"
    "rphp_10_8_8|rphp 10 8 8"
    # === Tseitin ===
    "tseitin_grid_6|tseitin randomodd grid 6 6"
    "tseitin_grid_7|tseitin randomodd grid 7 7"
    "tseitin_grid_8|tseitin randomodd grid 8 8"
    "tseitin_reg_50_6|tseitin randomodd gnd 50 6"
    # === Ordering principle ===
    "op_10|op 10"
    "op_15|op 15"
    "op_20|op 20"
    # === Counting ===
    "count_13_3|count 13 3"
    "count_16_3|count 16 3"
    "count_20_3|count 20 3"
    # === Parity ===
    "parity_11|parity 11"
    "parity_15|parity 15"
    "parity_20|parity 20"
    # === Pebbling ===
    "peb_pyr_10|peb pyramid 10"
    "peb_tree_12|peb tree 12"
    "peb_path_40|peb path 40"
    # === Stone ===
    "stone_3_pyr5|stone 3 pyramid 5"
    "stone_3_pyr6|stone 3 pyramid 6"
    "stone_4_pyr5|stone 4 pyramid 5"
    "stone_3_tree10|stone 3 tree 10"
    # === Search-structure families ===
    "kcolor3_g20|kcolor 3 gnp 20 0.5"
    "kcolor3_g30|kcolor 3 gnp 30 0.5"
    "kcolor3_g40|kcolor 3 gnp 40 0.5"
    "kclique5_g20|kclique 5 gnp 20 0.3"
    "kclique5_g30|kclique 5 gnp 30 0.3"
    "kclique6_g30|kclique 6 gnp 30 0.3"
    "ram_3_3_8|ram 3 3 8"
    "ramlb_5_5|ramlb 5 5 gnp 30 0.7"
    "ptn_40|ptn 40"
    "ptn_60|ptn 60"
    "subsetcard_20|subsetcard 20"
    "subsetcard_40|subsetcard 40"
    "vdw_10_3_3|vdw 10 3 3"
    "cpls_2_4_4|cpls 2 4 4"
    "cpls_3_4_4|cpls 3 4 4"
    "pitfall_45|pitfall 45 4 30 5 8"
    "cliquecoloring_5_5|cliquecoloring 5 5 gnp 30 0.5"
    "kcliquebin_6|kcliquebin 6 gnp 30 0.7"
    "domset_4_30|domset 4 gnp 30 0.5"
    "ec_gnd14_4|ec gnd 14 4"
    "matching_gnd16_4|matching gnd 16 4"
)

TOTAL=${#INSTANCES[@]}

run_instance() {
    local idx="$1" entry="$2" seed="$3"
    local label="${entry%%|*}" cnf_args="${entry#*|}"
    local cnf="$WORK/i_$idx.cnf" res="$WORK/r_$idx"

    if ! cnfgen -S "$seed" $cnf_args > "$cnf" 2>/dev/null; then
        printf 'GENFAIL|%s\n' "$label" > "$res"; return
    fi
    local nv nc
    nv=$(grep "^p cnf" "$cnf" | awk '{print $3}')
    nc=$(grep "^p cnf" "$cnf" | awk '{print $4}')

    # satience (+ parse conflicts/decisions)
    local ss se sec sd sres sconf sdec
    ss=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$SATIENCE" "$cnf" > "$WORK/s.out" 2>&1
    sec=$?; se=$(date +%s.%N)
    sd=$(awk "BEGIN{printf \"%.4f\", $se-$ss}")
    case $sec in 10) sres=SAT;; 20) sres=UNSAT;; 124) sres=TMO;; *) sres="EC$sec";; esac
    sconf=$(grep -oE "conflicts=[0-9]+" "$WORK/s.out" | grep -oE "[0-9]+" | head -1)
    sdec=$(grep -oE "decisions=[0-9]+" "$WORK/s.out" | grep -oE "[0-9]+" | head -1)
    [ -z "$sconf" ] && sconf="-"; [ -z "$sdec" ] && sdec="-"

    # minisat
    local ms me mec md mres
    ms=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$MINISAT" "$cnf" >/dev/null 2>&1
    mec=$?; me=$(date +%s.%N)
    md=$(awk "BEGIN{printf \"%.4f\", $me-$ms}")
    case $mec in 10) mres=SAT;; 20) mres=UNSAT;; 124) mres=TMO;; *) mres="EC$mec";; esac

    printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "$label" "$nv" "$nc" "$sres" "$sd" "$mres" "$md" "$sconf" "$sdec" "$seed" "$cnf_args" > "$res"
}

fifo="$WORK/sem"; mkfifo "$fifo"; exec 3<>"$fifo"; rm "$fifo"
for ((i=0;i<JOBS;i++)); do echo >&3; done

echo "=== Broad Divergence Scan ===" >&2
echo "Satience (HEAD): $SATIENCE" >&2
echo "Timeout: ${TIMEOUT_SEC}s  Workers: $JOBS  Seed offset: $SEED  Instances: $TOTAL" >&2

wall_s=$(date +%s.%N)
for i in "${!INSTANCES[@]}"; do
    read -u 3
    ( run_instance "$i" "${INSTANCES[$i]}" "$((SEED + i))"; echo >&3 ) &
done
wait
exec 3>&-
wall_e=$(date +%s.%N)
wall=$(awk "BEGIN{printf \"%.1f\", ($wall_e-$wall_s)}")

# sortable rows: ratio,label,nv,nc,sres,sd,mres,md,sconf,sdec
declare -a rows
for ((i=0;i<TOTAL;i++)); do
    line=$(cat "$WORK/r_$i" 2>/dev/null || echo "MISSING|0|0||||||||")
    IFS='|' read -r label nv nc sres sd mres md sconf sdec seed cnf_args <<< "$line"
    [ "$label" = "GENFAIL" ] && continue
    st=$sd; mt=$md
    [ "$sres" = "TMO" ] && st=$(awk "BEGIN{printf \"%.2f\", 2*$TIMEOUT_SEC}")
    [ "$mres" = "TMO" ] && mt=$(awk "BEGIN{printf \"%.2f\", 2*$TIMEOUT_SEC}")
    ratio=$(awk "BEGIN{m=$mt; if(m<0.001)m=0.001; printf \"%.2f\", $st/m}")
    rows+=("$ratio|$label|$nv|$nc|$sres|$sd|$mres|$md|$sconf|$sdec")
done
IFS=$'\n' sorted=($(printf '%s\n' "${rows[@]}" | sort -t'|' -k1 -rn)); unset IFS

echo ""
echo "=== Full results (sorted by satience/minisat time ratio, worst first) ==="
printf "%-6s %-22s %6s %7s  %-6s %9s  %-6s %9s  %9s %9s\n" \
    "RATIO" "LABEL" "VARS" "CLS" "S_RES" "S_TIME" "M_RES" "M_TIME" "S_CONF" "S_DEC"
echo "-----------------------------------------------------------------------------"
for r in "${sorted[@]}"; do
    IFS='|' read -r ratio label nv nc sres sd mres md sconf sdec <<< "$r"
    printf "%6s %-22s %6s %7s  %-6s %9s  %-6s %9s  %9s %9s\n" \
        "$ratio" "$label" "$nv" "$nc" "$sres" "$sd" "$mres" "$md" "$sconf" "$sdec"
done

# catastrophics
CATASTROPHIC=()
MISMATCH=()
for r in "${sorted[@]}"; do
    IFS='|' read -r ratio label nv nc sres sd mres md sconf sdec <<< "$r"
    flag=0
    if [ "$sres" = "TMO" ] && { [ "$mres" = "SAT" ] || [ "$mres" = "UNSAT" ]; }; then
        flag=1
    elif [ "$sres" != "TMO" ] && [ "$mres" != "TMO" ]; then
        if awk "BEGIN{exit !($sd >= 1.0)}" && awk "BEGIN{exit !($md < 0.5)}"; then flag=1; fi
    fi
    [ $flag -eq 1 ] && CATASTROPHIC+=("$ratio|$label|$nv|$nc|$sres|$sd|$mres|$md|$sconf|$sdec")
    # soundness
    if [ "$sres" != "TMO" ] && [ "$mres" != "TMO" ] && [ "$sres" != "$mres" ]; then
        MISMATCH+=("$label|$sres|$mres")
    fi
done

echo ""
echo "=== CATASTROPHIC divergences (sat>=1s & mini<0.5s, or sat=TMO & mini solved) ==="
printf "%-6s %-22s %6s %7s  %-6s %9s  %-6s %9s  %9s %9s\n" \
    "RATIO" "LABEL" "VARS" "CLS" "S_RES" "S_TIME" "M_RES" "M_TIME" "S_CONF" "S_DEC"
echo "-----------------------------------------------------------------------------"
if [ ${#CATASTROPHIC[@]} -eq 0 ]; then echo "  (none)"; fi
for r in "${CATASTROPHIC[@]}"; do
    IFS='|' read -r ratio label nv nc sres sd mres md sconf sdec <<< "$r"
    printf "%6s %-22s %6s %7s  %-6s %9s  %-6s %9s  %9s %9s\n" \
        "$ratio" "$label" "$nv" "$nc" "$sres" "$sd" "$mres" "$md" "$sconf" "$sdec"
done

echo ""
echo "=== Soundness mismatches (verdict disagreement, non-TMO) ==="
if [ ${#MISMATCH[@]} -eq 0 ]; then echo "  (none)"; fi
for m in "${MISMATCH[@]}"; do echo "  $m"; done

echo ""
echo "=== Summary ==="
echo "Total instances: $TOTAL   Catastrophic: ${#CATASTROPHIC[@]}   Wall: ${wall}s ($JOBS workers)"
