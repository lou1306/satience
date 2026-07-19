#!/bin/bash
# CNFgen divergence probe: find families where minisat is fast but satience is slow.
#
# Generates instances from ~15 CNFgen families (2 sizes each), runs both
# minisat (default flags) and satience_bench (default flags) on each, and
# reports per-instance time ratios. Filters divergences by:
#   ratio > 10x AND minisat < 2s
#
# This catches SEARCH pathologies (bad branching/restart/learning on a
# particular structure) rather than just slower C code — if minisat solves
# in 0.1s and satience takes 10s, the gap is in search quality, not
# propagation speed.
#
# Soundness cross-check: if minisat and satience disagree on any verdict,
# the script flags it prominently and exits non-zero.
#
# Usage: bash benchmark/cnfgen_divergence.sh [timeout_sec]
#   timeout_sec  per-instance timeout in seconds (default 30)
#   JOBS env var controls parallelism (default 4)
#
# Requires: cnfgen (pipx install cnfgen), minisat, GOAMD64=v3 go build for satience.

set -u

TIMEOUT_SEC=${1:-30}
JOBS=${JOBS:-4}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SATIENCE="$REPO_ROOT/satience_bench"
MINISAT="${MINISAT:-minisat}"

if ! command -v cnfgen >/dev/null 2>&1; then
    echo "ERROR: cnfgen not found. Install with: pipx install cnfgen" >&2
    exit 1
fi
if ! command -v "$MINISAT" >/dev/null 2>&1; then
    echo "ERROR: minisat not found on PATH." >&2
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

# Instance suite: "label|cnfgen args..."
# -S 1 fixes the seed so graph-based families (gnp/gnd) are reproducible.
# Argument order: cnfgen -S 1 <formula> <args> (options before formula).
# Two sizes per family: a small and a medium instance.
INSTANCES=(
    # === Binary pigeonhole (compact PHP encoding) ===
    "bphp_8_7|-S 1 bphp 8 7"
    "bphp_12_11|-S 1 bphp 12 11"
    # === Relativized pigeonhole ===
    "rphp_5_4_4|-S 1 rphp 5 4 4"
    "rphp_7_6_6|-S 1 rphp 7 6 6"
    # === k-clique (SAT: planted clique in random graph) ===
    "kclique_5_gnp20_0.7|-S 1 kclique 5 gnp 20 0.7"
    "kclique_6_gnp30_0.7|-S 1 kclique 6 gnp 30 0.7"
    # === k-colorability ===
    "kcolor_3_gnp20_0.5|-S 1 kcolor 3 gnp 20 0.5"
    "kcolor_3_gnp30_0.4|-S 1 kcolor 3 gnp 30 0.4"
    # === Dominating set ===
    "domset_3_gnp20_0.5|-S 1 domset 3 gnp 20 0.5"
    "domset_4_gnp30_0.5|-S 1 domset 4 gnp 30 0.5"
    # === Even coloring ===
    "ec_gnd10_4|-S 1 ec gnd 10 4"
    "ec_gnd14_4|-S 1 ec gnd 14 4"
    # === Perfect matching ===
    "matching_gnd12_3|-S 1 matching gnd 12 3"
    "matching_gnd16_4|-S 1 matching gnd 16 4"
    # === Pitfall (designed to be hard for CDCL with VSIDS, Vinyals AAAI 2020) ===
    "pitfall_paper|-S 1 pitfall 45 4 30 5 8"
    "pitfall_small|-S 1 pitfall 30 4 20 5 6"
    # === Pythagorean triples bicoloring ===
    "ptn_30|-S 1 ptn 30"
    "ptn_50|-S 1 ptn 50"
    # === Ramsey principle (UNSAT above threshold) ===
    "ram_3_3_8|-S 1 ram 3 3 8"
    "ram_4_4_18|-S 1 ram 4 4 18"
    # === Ramsey lower bound (SAT: planted clique or stable set) ===
    "ramlb_4_4_gnp20_0.7|-S 1 ramlb 4 4 gnp 20 0.7"
    "ramlb_5_5_gnp30_0.7|-S 1 ramlb 5 5 gnp 30 0.7"
    # === Stone formula ===
    "stone_3_pyramid5|-S 1 stone 3 pyramid 5"
    "stone_4_pyramid6|-S 1 stone 4 pyramid 6"
    # === Subset cardinality ===
    "subsetcard_20|-S 1 subsetcard 20"
    "subsetcard_40|-S 1 subsetcard 40"
    # === van der Waerden ===
    "vdw_10_3_3|-S 1 vdw 10 3 3"
    "vdw_15_3_4|-S 1 vdw 15 3 4"
    # === Thapen's CPLS (resolution-hard) ===
    "cpls_2_4_4|-S 1 cpls 2 4 4"
    "cpls_3_4_4|-S 1 cpls 3 4 4"

    # === Scaling probe: families that showed modest divergence at small sizes ===
    # ramlb scaling (29x at 30v — does it widen?)
    "ramlb_6_6_gnp50_0.7|-S 1 ramlb 6 6 gnp 50 0.7"
    "ramlb_7_7_gnp80_0.7|-S 1 ramlb 7 7 gnp 80 0.7"
    # rphp scaling (4.3x at 84v)
    "rphp_10_8_8|-S 1 rphp 10 8 8"
    "rphp_12_10_10|-S 1 rphp 12 10 10"
    # bphp scaling (2.8x at 24v, but satience WINS at 48v — interesting crossover)
    "bphp_16_15|-S 1 bphp 16 15"
    "bphp_20_19|-S 1 bphp 20 19"

    # === Untested families ===
    "cliquecoloring_4_4_gnp20_0.5|-S 1 cliquecoloring 4 4 gnp 20 0.5"
    "cliquecoloring_5_5_gnp30_0.5|-S 1 cliquecoloring 5 5 gnp 30 0.5"
    "kcliquebin_5_gnp20_0.7|-S 1 kcliquebin 5 gnp 20 0.7"
    "kcliquebin_6_gnp30_0.7|-S 1 kcliquebin 6 gnp 30 0.7"
    # Random 3-SAT at phase transition (4.2-4.3 ratio) — our known weak spot
    "randkcnf_3_200_850|-S 1 randkcnf 3 200 850"
    "randkcnf_3_300_1275|-S 1 randkcnf 3 300 1275"
)

TOTAL=${#INSTANCES[@]}

# --- Worker: generate CNF, run minisat, run satience, write result file ---
run_instance() {
    local idx="$1" entry="$2"
    local label="${entry%%|*}"
    local cnf_args="${entry#*|}"
    local cnf_file="$WORK_DIR/inst_$idx.cnf"
    local res_file="$WORK_DIR/res_$idx"

    if ! cnfgen $cnf_args > "$cnf_file" 2>/dev/null; then
        printf 'GENFAIL|%s|0|0|0|0|0|0\n' "$label" > "$res_file"
        return
    fi

    local n_vars n_clauses
    n_vars=$(grep "^p cnf" "$cnf_file" | awk '{print $3}')
    n_clauses=$(grep "^p cnf" "$cnf_file" | awk '{print $4}')

    # Phase 1: minisat
    local m_start m_end m_dur m_ec m_result
    m_start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$MINISAT" "$cnf_file" > /dev/null 2>&1
    m_ec=$?
    m_end=$(date +%s.%N)
    m_dur=$(awk "BEGIN{printf \"%.4f\", $m_end-$m_start}")
    case $m_ec in
        10)  m_result="SAT" ;;
        20)  m_result="UNSAT" ;;
        124) m_result="TIMEOUT" ;;
        *)   m_result="EC=$m_ec" ;;
    esac

    # Phase 2: satience
    local s_start s_end s_dur s_ec s_result
    s_start=$(date +%s.%N)
    timeout "$TIMEOUT_SEC" "$SATIENCE" "$cnf_file" > /dev/null 2>&1
    s_ec=$?
    s_end=$(date +%s.%N)
    s_dur=$(awk "BEGIN{printf \"%.4f\", $s_end-$s_start}")
    case $s_ec in
        10)  s_result="SAT" ;;
        20)  s_result="UNSAT" ;;
        124) s_result="TIMEOUT" ;;
        *)   s_result="EC=$s_ec" ;;
    esac

    printf '%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "$label" "$cnf_args" "$n_vars" "$n_clauses" \
        "$m_result" "$m_dur" "$s_result" "$s_dur" "$idx" > "$res_file"
}

# --- Launch workers with a FIFO semaphore ---
fifo="$WORK_DIR/sem"
mkfifo "$fifo"
exec 3<>"$fifo"
rm "$fifo"
for ((i = 0; i < JOBS; i++)); do echo >&3; done

echo "=== CNFgen Divergence Probe ===" >&2
echo "Satience:  $SATIENCE" >&2
echo "Minisat:   $(command -v $MINISAT)" >&2
echo "Timeout:   ${TIMEOUT_SEC}s per instance" >&2
echo "Workers:   $JOBS" >&2
echo "Instances: $TOTAL" >&2
echo "" >&2

wall_start=$(date +%s.%N)
for i in "${!INSTANCES[@]}"; do
    read -u 3
    (
        run_instance "$i" "${INSTANCES[$i]}"
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
echo ""
echo "=== Per-instance results (sorted by satience/minisat ratio, worst first) ==="
echo ""

# Build a sortable table. Columns: label, n_vars, n_clauses, minisat_verdict,
# minisat_time, satience_verdict, satience_time, ratio, divergent_flag.
declare -a rows
for ((i = 0; i < TOTAL; i++)); do
    res_file="$WORK_DIR/res_$i"
    line=$(cat "$res_file" 2>/dev/null || echo "MISSING|||||0|0|0")
    IFS='|' read -r label cnf_args n_vars n_clauses m_res m_dur s_res s_dur idx <<< "$line"

    # Skip GENFAIL for the table, but count them
    if [ "$label" = "GENFAIL" ] || [ -z "$label" ]; then
        continue
    fi

    # Compute ratio (timeout = 2x timeout for PAR-2 style)
    m_t=$m_dur
    s_t=$s_dur
    if [ "$m_res" = "TIMEOUT" ]; then m_t=$(awk "BEGIN{printf \"%.4f\", 2*$TIMEOUT_SEC}"); fi
    if [ "$s_res" = "TIMEOUT" ]; then s_t=$(awk "BEGIN{printf \"%.4f\", 2*$TIMEOUT_SEC}"); fi

    # ratio = satience / minisat (guard div by zero)
    ratio=$(awk "BEGIN{
        m=$m_t; s=$s_t;
        if (m < 0.001) m=0.001;
        printf \"%.2f\", s/m
    }")

    # Divergent flag: ratio > 10 AND minisat < 2s AND minisat not TIMEOUT
    divergent=""
    m_lt_2=$(awk "BEGIN{print ($m_dur < 2.0) ? 1 : 0}")
    ratio_gt_10=$(awk "BEGIN{print ($ratio > 10.0) ? 1 : 0}")
    if [ "$m_lt_2" = "1" ] && [ "$ratio_gt_10" = "1" ] && [ "$m_res" != "TIMEOUT" ]; then
        divergent="*"
    fi

    rows+=("$divergent|$ratio|$label|$n_vars|$n_clauses|$m_res|$m_dur|$s_res|$s_dur")
done

# Sort by ratio descending
IFS=$'\n' sorted=($(printf '%s\n' "${rows[@]}" | sort -t'|' -k2 -rn))
unset IFS

# Print header
printf "%-3s %-7s %-26s %7s %8s  %-8s %8s  %-8s %8s  %7s\n" \
    "" "RATIO" "INSTANCE" "VARS" "CLAUSES" "MINISAT" "TIME" "SATIENCE" "TIME" "VERDICT"
printf '%.0s-' {1..100}
echo ""

# Track soundness mismatches and per-family divergence counts
mismatch_count=0
gen_fail=0
declare -A fam_divergent fam_total

for row in "${sorted[@]}"; do
    IFS='|' read -r divergent ratio label n_vars n_clauses m_res m_dur s_res s_dur <<< "$row"

    # Derive family name (everything before first underscore-digit, or the label prefix)
    fam="${label%%_*}"
    case "$fam" in
        bphp|rphp|php) fam="pigeonhole" ;;
        peb)           fam="pebbling" ;;
        op)            fam="ordering" ;;
        count)         fam="counting" ;;
        tseitin)       fam="tseitin" ;;
        parity)        fam="parity" ;;
    esac
    fam_total[$fam]=$(( ${fam_total[$fam]:-0} + 1 ))
    if [ -n "$divergent" ]; then
        fam_divergent[$fam]=$(( ${fam_divergent[$fam]:-0} + 1 ))
    fi

    # Soundness mismatch check (ignore TIMEOUTs — they aren't verdicts)
    verdict_match="OK"
    if [ "$m_res" != "TIMEOUT" ] && [ "$s_res" != "TIMEOUT" ]; then
        if [ "$m_res" != "$s_res" ]; then
            verdict_match="MISMATCH"
            mismatch_count=$((mismatch_count + 1))
        fi
    fi

    printf "%-3s %-7s %-26s %7s %8s  %-8s %8s  %-8s %8s  %s\n" \
        "$divergent" "$ratio" "$label" "$n_vars" "$n_clauses" \
        "$m_res" "$m_dur" "$s_res" "$s_dur" "$verdict_match"
done

echo ""
echo "=== Per-family divergence count (ratio > 10x AND minisat < 2s) ==="
echo ""
printf "  %-15s %12s %12s\n" "FAMILY" "DIVERGENT" "TOTAL"
printf "  %-15s %12s %12s\n" "-------" "---------" "-----"
# Sort families by divergence count descending
fam_list=$(for k in "${!fam_total[@]}"; do
    echo "${fam_divergent[$k]:-0} $k ${fam_total[$k]}"
done | sort -rn)
while read -r div name tot; do
    printf "  %-15s %12s %12s\n" "$name" "$div" "$tot"
done <<< "$fam_list"

echo ""
echo "=== Divergent families (>=1 divergent instance) ==="
echo ""
div_fams=$(while read -r div name tot; do
    [ "${div:-0}" -gt 0 ] && echo "$div $name $tot"
done <<< "$fam_list")
if [ -z "$div_fams" ]; then
    echo "  (none)"
else
    while read -r div name tot; do
        printf "  %-15s %d/%d instances divergent\n" "$name" "$div" "$tot"
    done <<< "$div_fams"
fi

echo ""
echo "=== Summary ==="
echo "Total instances:   $TOTAL"
echo "Generation fails:  $gen_fail"
echo "Soundness mismatches: $mismatch_count"
echo "Wall time:         ${wall_dur}s  ($JOBS workers)"
echo ""
echo "Filter: ratio > 10x AND minisat < 2s"
echo "(* = divergent in the per-instance table above)"
echo ""

if [ $mismatch_count -gt 0 ]; then
    echo "!!! SOUNDNESS FAILURE: $mismatch_count verdict mismatches between minisat and satience."
    exit 1
fi

exit 0
