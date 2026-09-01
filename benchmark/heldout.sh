#!/bin/bash
# heldout.sh — Distributional held-out generalization gate for satience.
#
# PURPOSE
# A fixed static benchmark corpus (the 72-instance fast suite, gbd_instances)
# gets re-overfit: a detector/governor can be tuned against the exact fixed
# deterministic trajectories and their dec/conf/LBD signatures. This harness
# instead evaluates over PARAMETRIC CNFgen FAMILIES with fresh random draws,
# so a gate/threshold only survives if it generalizes across the distribution
# rather than keying to specific instance signatures.
#
# COMMITMENT: cnfgen families only. Coverage: combinational (Tseitin, kcolor,
# kclique), unstructured (random k-SAT), plus dense binary encodings (kcliquebin)
# and structured orderings (op), with one cnfgen -T majority-encoded rail
# (tseitin_maj). The encoded rails approximate the translated cardinality/XOR
# circuits of planning/verification encodings, partially closing the
# "encoded/industrial-like" gap in a parametric, reproducible way. True
# industrial instances (from SAT-Competition archives) are still NOT covered;
# a narrow static industrial firewall (no-TMO) would need to be layered
# separately if that coverage is required.
#
# METHOD
#   Paired A/B: run CONTROL_BINARY and VARIANT_BINARY on IDENTICAL draws, then
#   report the per-instance delta (variant - control). Paired comparison
#   cancels per-instance intrinsic variance, which independent draws would
#   otherwise swamp (phase-transition families span trivial <-> TMO).
#
# TWO RAILS (seed partition discipline)
#   - DEV rail (DEV_ITERATIONS/family): small N for parameter tuning. Do NOT
#     accept/reject on this rail.
#   - VALIDATION rail (VALIDATION_ITERATIONS/family, fresh seed offset): the
#     actual accept/reject measurement. A change is accepted only if it passes
#     the acceptance rule on the VALIDATION rail.
#
# ACCEPTANCE RULE (per family, on the VALIDATION rail)
#   Gate passes iff ALL hold:
#     1. No new TMO: every instance control solves, variant solves.
#     2. Max-regress cap: < REGRESS_MAX_FRAC of sampled instances regress by
#        more than REGRESS_MAX_PCT wall-time.
#     3. Median PAR2 does not regress by more than MEDIAN_REGRESS_PCT.
#   A change must pass on ALL sampled families of the validation rail.
#
# Usage: bash benchmark/heldout.sh
# Env:
#   CONTROL_BINARY      reference binary (default $REPO_ROOT/satience_bench)
#   VARIANT_BINARY      binary under test  (default = CONTROL_BINARY; set an
#                       explicit second binary to do a real A/B)
#   DEV_ITERATIONS      draws/family on the dev rail   (default 10)
#   VALIDATION_ITERATIONS draws/family on validation rail (default 50)
#   TIMEOUT             per-solver timeout seconds (default 30)
#   JOBS                parallel workers (default 6)
#   REGRESS_MAX_FRAC    max fraction of sampled instances allowed to regress
#                       by > REGRESS_MAX_PCT (default 0.10)
#   REGRESS_MAX_PCT     wall-time regression threshold, % (default 25)
#   MEDIAN_REGRESS_PCT  max allowed median PAR2 regression, % (default 5)
#   NOISE_FLOOR         control solves below this seconds are exempt from the
#                       %-based regress/median rules (scheduler jitter swamps
#                       sub-second solves; TMO & newTMO still counted);
#                       seconds (default 0.25, ~4x below the ~1s suite PAR2)
#
# Requires: cnfgen (pipx install cnfgen), GOAMD64=v3 go build.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

CONTROL_BINARY="${CONTROL_BINARY:-$REPO_ROOT/satience_bench}"
VARIANT_BINARY="${VARIANT_BINARY:-$CONTROL_BINARY}"
DEV_ITERATIONS=${DEV_ITERATIONS:-10}
VALIDATION_ITERATIONS=${VALIDATION_ITERATIONS:-50}
TIMEOUT=${TIMEOUT:-30}
JOBS=${JOBS:-6}
REGRESS_MAX_FRAC=${REGRESS_MAX_FRAC:-0.10}
REGRESS_MAX_PCT=${REGRESS_MAX_PCT:-25}
MEDIAN_REGRESS_PCT=${MEDIAN_REGRESS_PCT:-5}
NOISE_FLOOR=${NOISE_FLOOR:-0.25}

# Resolve user-provided relative paths against the repo root; absolutes pass through.
case "$CONTROL_BINARY" in /*) ;; *) CONTROL_BINARY="$REPO_ROOT/$CONTROL_BINARY" ;; esac
case "$VARIANT_BINARY" in /*) ;; *) VARIANT_BINARY="$REPO_ROOT/$VARIANT_BINARY" ;; esac

if ! command -v cnfgen >/dev/null 2>&1; then
    echo "ERROR: cnfgen not found. Install with: pipx install cnfgen" >&2
    exit 1
fi
for b in "$CONTROL_BINARY" "$VARIANT_BINARY"; do
    if [ ! -x "$b" ]; then
        echo "ERROR: binary not executable: $b" >&2
        exit 1
    fi
done

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# ---------------------------------------------------------------------------
# Family matrix. Each entry: "name|cnfgen args...".
# Names carry no per-instance identity; draws are randomized within each family
# (see gen_family_seed below) so gates cannot key to a file/signature.
# Families and density rails mirror fuzz_structured.sh / fuzz_random_ksat.sh.
# ---------------------------------------------------------------------------
MATRIX=(
    # === Tseitin odd-charge on random grids (UNSAT, propagation-heavy; the
    #      grid ramp spans easy -> TMO at grid9 so the no-new-TMO rule is
    #      exercised) ===
    "tseitin_grid5|tseitin randomodd grid 5 5"
    "tseitin_grid6|tseitin randomodd grid 6 6"
    "tseitin_grid7|tseitin randomodd grid 7 7"
    "tseitin_grid8|tseitin randomodd grid 8 8"
    "tseitin_grid9|tseitin randomodd grid 9 9"
    # === Tseitin on one random regular graph (second no-new-TMO canary) ===
    "tseitin_gnd50|tseitin randomodd gnd 50 6"
    # === k-coloring (SAT/UNSAT by graph; hard regime rails) ===
    "kcolor3_gnp30|kcolor 3 gnp 30 0.5"
    "kcolor3_gnp50|kcolor 3 gnp 50 0.3"
    "kcolor3_grid7|kcolor 3 grid 7 7"
    "kcolor4_gnp30|kcolor 4 gnp 30 0.5"
    "kcolor4_gnp50|kcolor 4 gnp 50 0.4"
    # === k-clique ===
    "kclique5_gnp30|kclique 5 gnp 30 0.3"
    "kclique5_gnp40|kclique 5 gnp 40 0.2"
    "kclique6_gnp40|kclique 6 gnp 40 0.3"
    "kclique6_gnp50|kclique 6 gnp 50 0.25"
    # === k-clique BINARY encoding (dense "encoded"/industrial-like rails;
    #      UNSAT; calc: ~1.7-4.3s across draws at these densities) ===
    "kcliquebin9_gnp50|kcliquebin 9 gnp 50 0.4"
    "kcliquebin10_gnp60|kcliquebin 10 gnp 60 0.35"
    # === Ordering principle (structured UNSAT, ~1s; the op variant-Ramsey
    #      threshold is intentionally avoided: N just under/over r jumps
    #      easy<->TMO with no tunable density, so it is not a stable rail) ===
    "op18|op 18"
    "op20|op 20"
    # === Majority-ENCODED Tseitin (an actual cnfgen -T encoding of a
    #      combinational base; majority/xor encodings resemble the translated
    #      cardinality circuits of planning/verification encodings, closing
    #      part of the "encoded/industrial-like" coverage gap in a parametric,
    #      reproducible way) ===
    "tseitin_maj_g4|tseitin randomodd grid 4 4 -T maj 3"
    # === Random k-SAT (below/at/above phase transition; cnfgen takes a
    #      clause COUNT, so derive from vars × ratio) ===
    "rand3_50_3.5|randkcnf 3 50 175"
    "rand3_50_4.26|randkcnf 3 50 213"
    "rand3_50_4.8|randkcnf 3 50 240"
    "rand3_100_4.26|randkcnf 3 100 426"
    "rand4_75_8.5|randkcnf 4 75 638"
)
MATRIX_SIZE=${#MATRIX[@]}

# ---------------------------------------------------------------------------
# Family-seeded, reproducible draw generator. The seed rail for draws is
# derived from a fixed (per family) base + draw index, so the SAME instance is
# used for control and variant (paired). The dev and validation rails use
# disjoint seed ranges (offset by 1_000_000) so tuning on dev never touches
# the instances measured on validation.
# ---------------------------------------------------------------------------
gen_family_seed() {
    local rail="$1" idx="$2"
    if [ "$rail" = "validation" ]; then
        echo "$(( 1000000 + idx * 7919 ))"
    else
        echo "$(( idx * 7919 ))"
    fi
}

family_name() {
    local idx="$1"
    local entry="${MATRIX[$((idx % MATRIX_SIZE))]}"
    echo "${entry%%|*}"
}
family_args() {
    local idx="$1"
    local entry="${MATRIX[$((idx % MATRIX_SIZE))]}"
    echo "${entry#*|}"
}

# run_one draws collides with run_satience_fast_suite naming; here we run a
# single instance through a single binary and record "RESULT|DURATION".
# PAR2 convention: a timeout counts as 2*TIMEOUT.
par2_of() { # $1 result, $2 duration
    if [ "$1" = "TMO" ]; then
        echo "$(( 2 * TIMEOUT ))"
    else
        echo "$2"
    fi
}

# ---------------------------------------------------------------------------
# Worker: generate one instance, run control & variant (paired), emit the line.
# Line format:
#   idx|family|control_res|control_par2|variant_res|variant_par2|control_dur|variant_dur
# ---------------------------------------------------------------------------
run_instance() {
    local idx="$1" rail="$2" res_file="$3"
    local fname fargs seed
    fname=$(family_name "$idx")
    fargs=$(family_args "$idx")
    seed=$(gen_family_seed "$rail" "$idx")

    local cnf_file="$WORK_DIR/inst_${rail}_${idx}.cnf"
    local c_res c_dur v_res v_dur
    local s e ec d

    if ! cnfgen -S "$seed" $fargs > "$cnf_file" 2>/dev/null; then
        printf '%d|%s|GENFAIL|0|GENFAIL|0|0|0\n' "$idx" "$fname" > "$res_file"
        return
    fi

    s=$(date +%s.%N)
    timeout "$TIMEOUT" "$CONTROL_BINARY" "$cnf_file" > /dev/null 2>&1
    ec=$?
    e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
    case $ec in
        10)  c_res="SAT" ;;
        20)  c_res="UNSAT" ;;
        124) c_res="TMO" ;;
        *)   c_res="EC=$ec" ;;
    esac
    c_dur=$d

    s=$(date +%s.%N)
    timeout "$TIMEOUT" "$VARIANT_BINARY" "$cnf_file" > /dev/null 2>&1
    ec=$?
    e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
    case $ec in
        10)  v_res="SAT" ;;
        20)  v_res="UNSAT" ;;
        124) v_res="TMO" ;;
        *)   v_res="EC=$ec" ;;
    esac
    v_dur=$d

    local c_par2 v_par2
    c_par2=$(par2_of "$c_res" "$c_dur")
    v_par2=$(par2_of "$v_res" "$v_dur")
    printf '%d|%s|%s|%s|%s|%s|%s|%s\n' \
        "$idx" "$fname" "$c_res" "$c_par2" "$v_res" "$v_par2" "$c_dur" "$v_dur" \
        > "$res_file"
}

# ---------------------------------------------------------------------------
# AWS: run a full rail (all families × N draws) and emit verdict summary.
# ---------------------------------------------------------------------------
run_rail() {
    local rail="$1" n="$2" outdir="$3"
    local total=$(( n * MATRIX_SIZE ))
    echo "== rail=$rail draws/family=$n total=$total ==" >&2

    # FIFO semaphore for parallelism
    local fifo="$outdir/sem_$rail"
    mkfifo "$fifo"
    exec 4<>"$fifo"
    rm "$fifo"
    local i
    for ((i = 0; i < JOBS; i++)); do echo >&4; done

    local k
    for ((k = 0; k < total; k++)); do
        read -u 4
        (
            run_instance "$k" "$rail" "$outdir/res_${rail}_${k}"
            echo >&4
        ) &
    done
    wait
    exec 4>&-
}

# accumulate_one appends idx,fname,c_par2,v_par2 into two per-family arrays.
# We store only PAR2 values (already par2-computed) in per-family temp files.
accumulate_instance() {
    local line="$1" famfile="$2" # famfile is a list of "c_par2 v_par2"
    IFS='|' read -r idx fname c_res c_par2 v_res v_par2 c_dur v_dur <<< "$line"
    if [ "$c_res" = "GENFAIL" ]; then
        return
    fi
    echo "$c_par2 $v_par2" >> "$famfile"
}

# compare evaluates a float comparison; prints 1 (true) or 0 (false).
# The operator is passed as its own arg so the shell never evaluates a float.
# Usage: if [ "$(compare 3.2 '>' 3.1)" = "1" ]; then ...
compare() {
    # NB: cannot write (a op b) — awk treats op-as-variable as concatenation.
    local r
    r=$(awk -v a="$1" -v op="$2" -v b="$3" '
        BEGIN{
            r=0
            if      (op==">")  r=(a>b)
            else if (op==">=") r=(a>=b)
            else if (op=="<")  r=(a<b)
            else if (op=="<=") r=(a<=b)
            else if (op=="==") r=(a==b)
            print r?1:0
        }')
    echo "$r"
}

# median_of reads a list of numeric lines and prints the median (floats).
median_of() {
    sort -n | awk '{v[NR]=$1} END{
        if(NR==0){print 0.000; exit}
        n=NR
        m=n%2 ? v[(n+1)/2] : (v[n/2]+v[n/2+1])/2
        printf "%.3f\n", m
    }'
}

# compute_verdict reads a per-family file (lines: "c_par2 v_par2") and prints
# a single verdict line:
#   fam|cnt|cpar2med|vpar2med|cTMO|vTMO|newTMO|regressFrac|PASS|FAIL...
compute_verdict() {
    local fam="$1" famfile="$2"
    local cnt=0 c_ttmo=0 v_ttmo=0 regress=0
    local c_par2 v_par2 tmo2=$((2 * TIMEOUT))
    local cvals=() vvals=()
    while read -r c_par2 v_par2; do
        [ -z "$c_par2" ] && continue
        cvals+=("$c_par2")
        vvals+=("$v_par2")
        cnt=$((cnt + 1))
        if [ "$(compare "$c_par2" '>=' "$tmo2")" = "1" ]; then c_ttmo=$((c_ttmo + 1)); fi
        if [ "$(compare "$v_par2" '>=' "$tmo2")" = "1" ]; then v_ttmo=$((v_ttmo + 1)); fi
        # regress = variant par2 worse than control par2 by > REGRESS_MAX_PCT.
        # Equal timeouts -> equal par2 (both TMO) -> not a regression.
        if [ "$(compare "$c_par2" '>' "$NOISE_FLOOR")" = "1" ] && [ "$(compare "$v_par2" '>' "$c_par2")" = "1" ]; then
            local pct
            pct=$(awk -v v="$v_par2" -v c="$c_par2" 'BEGIN{printf "%.1f", 100.0*(v-c)/c}')
            if [ "$(compare "$pct" '>' "$REGRESS_MAX_PCT")" = "1" ]; then
                regress=$((regress + 1))
            fi
        fi
    done < "$famfile"

    # newTMO = control solved (par2 < 2*TIMEOUT) but variant timed out.
    local new_tmo=0 i2
    for ((i2 = 0; i2 < cnt; i2++)); do
        if [ "$(compare "${cvals[$i2]}" '<' "$tmo2")" = "1" ] && \
           [ "$(compare "${vvals[$i2]}" '>=' "$tmo2")" = "1" ]; then
            new_tmo=$((new_tmo + 1))
        fi
    done

    local c_med v_med
    c_med=$(printf '%s\n' "${cvals[@]}" | median_of)
    v_med=$(printf '%s\n' "${vvals[@]}" | median_of)

    local ok_new_tmo=1 ok_regress=1 ok_median=1 fail_reason=""
    if [ "$new_tmo" -gt 0 ]; then ok_new_tmo=0; fail_reason="${fail_reason}(newTMO=$new_tmo)"; fi
    local rf
    rf=$(awk -v r="$regress" -v c="$cnt" 'BEGIN{ if(c>0) printf "%.2f", r/c; else print 0 }')
    if [ "$(compare "$rf" '>' "$REGRESS_MAX_FRAC")" = "1" ]; then
        ok_regress=0; fail_reason="${fail_reason}(regressFrac=${regress}/${cnt})"
    fi
    local med_pct=0
    if [ "$(compare "$c_med" '>' "$NOISE_FLOOR")" = "1" ]; then
        med_pct=$(awk -v v="$v_med" -v c="$c_med" 'BEGIN{printf "%.1f", 100.0*(v-c)/c}')
    fi
    if [ "$(compare "$v_med" '>' "$c_med")" = "1" ] && [ "$(compare "$med_pct" '>' "$MEDIAN_REGRESS_PCT")" = "1" ]; then
        ok_median=0; fail_reason="${fail_reason}(medRegress=${med_pct}%)"
    fi

    local verdict="PASS"
    if [ "$ok_new_tmo" = "0" ] || [ "$ok_regress" = "0" ] || [ "$ok_median" = "0" ]; then
        verdict="FAIL$fail_reason"
    fi
    printf '%s|%d|%.3f|%.3f|%d|%d|%d|%s|%s\n' \
        "$fam" "$cnt" "$c_med" "$v_med" "$c_ttmo" "$v_ttmo" "$new_tmo" "$rf" "$verdict"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
echo "=== Satience Distributional Held-Out Gate ==="
echo "Control : $CONTROL_BINARY"
echo "Variant : $VARIANT_BINARY"
echo "Timeout : ${TIMEOUT}s (PAR2: TMO=$((${TIMEOUT}*2))s)"
echo "Workers : $JOBS"
echo "Dev rail : $DEV_ITERATIONS/family | Validation rail: $VALIDATION_ITERATIONS/family"
echo "Regress cap: >$REGRESS_MAX_PCT% on <=$REGRESS_MAX_FRAC frac; median regress <=$MEDIAN_REGRESS_PCT%"
echo ""

mkdir -p "$WORK_DIR/dev" "$WORK_DIR/val"

# Dev rail (tuning aid only — never accept/reject).
run_rail "dev" "$DEV_ITERATIONS" "$WORK_DIR/dev" || true

# Validation rail (accept/reject measurement).
run_rail "validation" "$VALIDATION_ITERATIONS" "$WORK_DIR/val"

echo ""
echo "=== Validation rail verdicts (per family) ==="
OVERALL_FAIL=0
family_needed=0
for ((fam = 0; fam < MATRIX_SIZE; fam++)); do
    fname=$(family_name "$fam")
    famfile="$WORK_DIR/val/family_${fname}.dat"
    : > "$famfile"
    for ((k = 0; k < VALIDATION_ITERATIONS; k++)); do
        idx=$(( fam + k * MATRIX_SIZE ))
        res="$WORK_DIR/val/res_validation_${idx}"
        line=$(cat "$res" 2>/dev/null || true)
        if [ -n "$line" ]; then
            accumulate_instance "$line" "$famfile"
        fi
    done
    verdict=$(compute_verdict "$fname" "$famfile")
    echo "  $verdict"
    famline=$(echo "$verdict" | awk -F'|' '{print $NF}')
    if echo "$famline" | grep -q FAIL; then
        OVERALL_FAIL=$((OVERALL_FAIL + 1))
    fi
    family_needed=$((family_needed + 1))
done

echo ""
if [ "$OVERALL_FAIL" -eq 0 ]; then
    echo "HELDOUT: PASS — variant passes on all $family_needed validation families."
    exit 0
else
    echo "HELDOUT: FAIL — variant fails on $OVERALL_FAIL of $family_needed validation families."
    echo "NOTE: params may only be tuned against the DEV rail; do not re-tune and re-measure"
    echo "      on the VALIDATION rail, or the validation distribution is burned."
    exit 1
fi
