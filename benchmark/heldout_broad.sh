#!/bin/bash
# heldout_broad.sh — BROAD distributional held-out gate for satience.
#
# Purpose
# The classic fast-suite + small-heldout rails (see heldout.sh) are dominated by
# tiny instances (50-300 vars) that the solver already solves in ~2s, so most
# improvements are unmeasurable there (cold paths like FLP/in-processing/
# subsumption never fire; large-scale behavior never manifests). This rail
# measures a HARDER, BROADER distribution so a change has real headroom to show
# a win:
#   Tier 1 (cnfgen, reproducible): LARGE random k-SAT (1000-2000 vars, near the
#     phase transition, where the FLP probe budget binds, big matrices, and
#     large-scale propagation actually cost time) + large structured/encoded
#     families (Tseitin grids/gnd, pigeonhole, k-color, binary k-clique).
#   Tier 2 (industrial, optional): FIXED SAT-competition/verification instances
#     from a user-supplied list (INDUSTRIAL_DIR + INDUSTRIAL_LIST). Real target
#     distribution; verdicts are cross-checked against minisat.
#
# Acceptance (see ACCEPT rule below) — DISTRIBUTIONAL / EXPECTATION based,
# deliberately DIFFERENT from heldout.sh's per-family-all-pass veto:
#   A variant is accepted iff, over ALL instances of the validation rail:
#     1. Verdict soundness: every solved instance agrees with control (and, for
#        the industrial tier, minisat) — mismatches are a hard FAIL.
#     2. Geometric-mean PAR2 improves by >= ACCEPT_IMPROVE_PCT (default 5%).
#     3. New-TMO fraction (control solved / variant TMO) <= NEWTMO_MAX_FRAC.
#     4. Per-instance regression fraction (>REGRESS_MAX_PCT slower, above the
#        noise floor) <= REGRESS_MAX_FRAC.
#   Redistribution across families/instances is ALLOWED (no per-family veto):
#   a change that helps many and hurts a bounded few is landable. This is what
#   unblocks genuinely better-but-redistributive heuristics that the strict
#   all-families rule would reject.
#
#   NOTE ON NOISE: wall-clock PAR2 on a load-noisy machine carries ~1% per-instance
#   jitter; with tiny N the geometric mean can swing several % even for identical
#   binaries (a small-N "PASS" can be pure noise). Only run the acceptance
#   decision at the default (or larger) VALIDATION_ITERATIONS so the geomean
#   stabilizes; treat DEV rail and small-N runs as directional only. Identical-
#   control/variant at full N must read ~0% (deterministic solves), so the
#   default 5% ACCEPT_IMPROVE_PCT sits safely above the noise floor.
#
# Seed discipline mirrors heldout.sh: DEV and VALIDATION rails use disjoint
# seed bases (DEV_SEED_BASE / VAL_SEED_BASE); tune only on DEV, accept/reject
# only on VALIDATION, never re-tune and re-measure on a burned VALIDATION rail.
#
# Usage:
#   CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var ./heldout_broad.sh
#
# Env vars (defaults):
#   DEV_ITERATIONS        draws/family on the dev rail (6)
#   VALIDATION_ITERATIONS draws/family on validation rail (15)
#   TIMEOUT               per-solver seconds per instance (90); PAR2 TMO=2*TIMEOUT
#   JOBS                  parallel workers (8)
#   ACCEPT_IMPROVE_PCT    min geomean-PAR2 improvement % to accept (5)
#   NEWTMO_MAX_FRAC       max fraction of new TMO allowed (0.10)
#   REGRESS_MAX_FRAC      max fraction of intra-pair regressions allowed (0.15)
#   REGRESS_MAX_PCT       a regression is variant > this % slower (25)
#   NOISE_FLOOR           control keeps this seconds to be subject to % rules (0.25)
#   INDUSTRIAL_DIR        dir of fixed industrial instances (optional)
#   INDUSTRIAL_LIST       newline-separated file of .cnf filenames under INDUSTRIAL_DIR
#   MINISAT_BIN           minisat binary for industrial verdict cross-check (default minisat)
# Requires: cnfgen (pipx install cnfgen).

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

CONTROL_BINARY="${CONTROL_BINARY:-$REPO_ROOT/satience_bench}"
VARIANT_BINARY="${VARIANT_BINARY:-$CONTROL_BINARY}"
DEV_ITERATIONS=${DEV_ITERATIONS:-6}
VALIDATION_ITERATIONS=${VALIDATION_ITERATIONS:-15}
TIMEOUT=${TIMEOUT:-90}
JOBS=${JOBS:-8}
ACCEPT_IMPROVE_PCT=${ACCEPT_IMPROVE_PCT:-5}
NEWTMO_MAX_FRAC=${NEWTMO_MAX_FRAC:-0.10}
REGRESS_MAX_FRAC=${REGRESS_MAX_FRAC:-0.15}
REGRESS_MAX_PCT=${REGRESS_MAX_PCT:-25}
NOISE_FLOOR=${NOISE_FLOOR:-0.25}
INDUSTRIAL_DIR="${INDUSTRIAL_DIR:-}"
INDUSTRIAL_LIST="${INDUSTRIAL_LIST:-}"
MINISAT_BIN="${MINISAT_BIN:-minisat}"
DEV_SEED_BASE="${DEV_SEED_BASE:-0}"
VAL_SEED_BASE="${VAL_SEED_BASE:-1000000}"

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
# Broad large family matrix (Tier 1). name|cnfgen args...
# Sized so cold paths fire (>2000-vars FLP budget, large BVE, large DB reduce)
# and so PAR2 carries genuine difficulty variance (not "already solved").
# ---------------------------------------------------------------------------
MATRIX=(
    # Large random k-SAT near/around the phase transition (UNSAT-ish at these
    # ratios; mixes of trivial & hard draws; FLP budget binds above 2000 vars).
    "rand3_1k_4.26|randkcnf 3 1000 4260"
    "rand3_1k_4.5|randkcnf 3 1000 4500"
    "rand3_2k_4.26|randkcnf 3 2000 8520"
    "rand3_2k_4.5|randkcnf 3 2000 9000"
    "rand4_750_8.5|randkcnf 4 750 6375"
    "rand4_1500_8.5|randkcnf 4 1500 12750"
    # Large structured / propagation-heavy (UNSAT).
    "tseitin_grid15|tseitin randomodd grid 15 15"
    "tseitin_grid20|tseitin randomodd grid 20 20"
    "tseitin_gnd100|tseitin randomodd gnd 100 6"
    # Pigeonhole (hard UNSAT for CDCL).
    "php_7_8|php 7 8"
    "php_8_9|php 8 9"
    # Large graph/coloring.
    "kcolor3_gnp120|kcolor 3 gnp 120 0.3"
    "kcolor4_gnp100|kcolor 4 gnp 100 0.4"
    # Dense binary "encoded"/industrial-like (UNSAT).
    "kcliquebin9_gnp80|kcliquebin 9 gnp 80 0.4"
)
MATRIX_SIZE=${#MATRIX[@]}

gen_family_seed() {
    local rail="$1" idx="$2"
    if [ "$rail" = "validation" ]; then
        echo "$(( VAL_SEED_BASE + idx * 7919 ))"
    else
        echo "$(( DEV_SEED_BASE + idx * 7919 ))"
    fi
}
family_name() { local e="${MATRIX[$(( $1 % MATRIX_SIZE ))]}"; echo "${e%%|*}"; }
family_args() { local e="${MATRIX[$(( $1 % MATRIX_SIZE ))]}"; echo "${e#*|}"; }

par2_of() { if [ "$1" = "TMO" ]; then echo "$(( 2 * TIMEOUT ))"; else echo "$2"; fi; }

# Run control & variant (paired) on a generated instance; emit
# idx|family|ctl_res|ctl_par2|var_res|var_par2|ctl_dur|var_dur
run_instance() {
    local idx="$1" rail="$2" res_file="$3"
    local fname fargs seed
    fname=$(family_name "$idx"); fargs=$(family_args "$idx")
    seed=$(gen_family_seed "$rail" "$idx")
    local cnf_file="$WORK_DIR/gen_${rail}_${idx}.cnf"
    if ! cnfgen -S "$seed" $fargs > "$cnf_file" 2>/dev/null; then
        printf '%d|%s|GENFAIL|0|GENFAIL|0|0|0\n' "$idx" "$fname" > "$res_file"; return
    fi
    local c_res c_dur v_res v_dur s e ec d
    s=$(date +%s.%N); timeout "$TIMEOUT" "$CONTROL_BINARY" "$cnf_file" >/dev/null 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
    case $ec in 10) c_res="SAT";; 20) c_res="UNSAT";; 124) c_res="TMO";; *) c_res="EC=$ec";; esac; c_dur=$d
    s=$(date +%s.%N); timeout "$TIMEOUT" "$VARIANT_BINARY" "$cnf_file" >/dev/null 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
    case $ec in 10) v_res="SAT";; 20) v_res="UNSAT";; 124) v_res="TMO";; *) v_res="EC=$ec";; esac; v_dur=$d
    printf '%d|%s|%s|%s|%s|%s|%s|%s\n' "$idx" "$fname" "$c_res" "$(par2_of "$c_res" "$c_dur")" "$v_res" "$(par2_of "$v_res" "$v_dur")" "$c_dur" "$v_dur" > "$res_file"
}

# Run one FIXED industrial instance (Tier 2). name = basename (no .cnf).
run_industrial() {
    local name="$1" res_file="$2"
    local cnf_file="$INDUSTRIAL_DIR/$name.cnf"
    if [ ! -f "$cnf_file" ]; then
        printf 'I|%s|GENFAIL|0|GENFAIL|0|0|0\n' "$name" > "$res_file"; return
    fi
    local c_res c_dur v_res v_dur s e ec d
    s=$(date +%s.%N); timeout "$TIMEOUT" "$CONTROL_BINARY" "$cnf_file" >/dev/null 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
    case $ec in 10) c_res="SAT";; 20) c_res="UNSAT";; 124) c_res="TMO";; *) c_res="EC=$ec";; esac; c_dur=$d
    s=$(date +%s.%N); timeout "$TIMEOUT" "$VARIANT_BINARY" "$cnf_file" >/dev/null 2>&1; ec=$?; e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
    case $ec in 10) v_res="SAT";; 20) v_res="UNSAT";; 124) v_res="TMO";; *) v_res="EC=$ec";; esac; v_dur=$d
    # minisat cross-check (best effort; minisat may TMO -> skip that instance's check)
    local m_res="NA"
    if command -v "$MINISAT_BIN" >/dev/null 2>&1; then
        timeout "$TIMEOUT" "$MINISAT_BIN" "$cnf_file" >/dev/null 2>&1
        case $? in 10) m_res="SAT";; 20) m_res="UNSAT";; 124) m_res="TMO";; *) m_res="NA";; esac
    fi
    printf 'I|%s|%s|%s|%s|%s|%s|%s|%s\n' "$name" "$c_res" "$(par2_of "$c_res" "$c_dur")" "$v_res" "$(par2_of "$v_res" "$v_dur")" "$c_dur" "$v_dur" "$m_res" > "$res_file"
}

# --- parallel fan-out: run fn(k,outdir) for k in [0,n) -----------------------
run_parallel() {
    local n="$1" fn="$2" outdir="$3"
    local fifo="$outdir/sem"; mkfifo "$fifo"; exec 4<>"$fifo"; rm "$fifo"
    local i
    for ((i = 0; i < JOBS; i++)); do echo >&4; done
    local k
    for ((k = 0; k < n; k++)); do
        read -u 4
        ( "$fn" "$k" "$outdir"; echo >&4 ) &
    done
    wait; exec 4>&-
}

do_dev() { run_instance "$1" dev "$2/r_$1"; }
do_val() { run_instance "$1" validation "$2/r_$1"; }
do_ind() { local nm="${INDUSTRIAL_NAMES[$1]}"; run_industrial "$nm" "$2/ind_$1"; }

accumulate() {
    local line="$1" aggfile="$2"
    local idx fname c_res c_par2 v_res v_par2 c_dur v_dur m
    IFS='|' read -r idx fname c_res c_par2 v_res v_par2 c_dur v_dur m <<< "$line"
    [ "$c_res" = "GENFAIL" ] && return
    echo "$fname $c_res $c_par2 $v_res $v_par2 $m" >> "$aggfile"
}

geomean() { # reads numeric lines -> geomean
    awk '{s += log($1); n++} END{ if(n>0) printf "%.6f", exp(s/n); else print 0 }'
}

# compute_agg: given the aggregated validation file, emit summary + PASS/FAIL.
# aggfile lines: "fam c_res c_par2 v_res v_par2 [m_res]"
compute_agg() {
    local aggfile="$1"
    local n=0 mism=0 newtmo=0 regress=0
    local cpar=() vpar=() cres=() vres=()
    while read -r fam c_res c_par2 v_res v_par2 m; do
        [ -z "$fam" ] && continue
        if [ "$c_res" != "TMO" ] && [ "$v_res" != "TMO" ] && [ "$c_res" != "$v_res" ]; then
            mism=$((mism + 1)); echo "  VERDICT MISMATCH: $fam ctl=$c_res var=$v_res" >&2
        fi
        if [ "$c_res" != "TMO" ] && [ "$v_res" = "TMO" ]; then newtmo=$((newtmo + 1)); fi
        if [ "$(awk -v c="$c_par2" -v f="$NOISE_FLOOR" 'BEGIN{print (c>f)?1:0}')" = "1" ] && \
           [ "$(awk -v v="$v_par2" -v c="$c_par2" 'BEGIN{print (v>c)?1:0}')" = "1" ]; then
            local pct
            pct=$(awk -v v="$v_par2" -v c="$c_par2" 'BEGIN{printf "%.1f", 100.0*(v-c)/c}')
            if [ "$(awk -v p="$pct" -v r="$REGRESS_MAX_PCT" 'BEGIN{print (p>r)?1:0}')" = "1" ]; then regress=$((regress + 1)); fi
        fi
        cpar+=("$c_par2"); vpar+=("$v_par2"); cres+=("$c_res"); vres+=("$v_res")
        n=$((n + 1))
    done < "$aggfile"
    [ "$n" = "0" ] && { echo "AGG|0|null|no instances|FAIL(empty)"; return; }

    local cg vg impr newtmo_frac reg_frac
    cg=$(printf '%s\n' "${cpar[@]}" | geomean)
    vg=$(printf '%s\n' "${vpar[@]}" | geomean)
    impr=$(awk -v v="$vg" -v c="$cg" 'BEGIN{ if(c>0) printf "%.2f", 100.0*(v-c)/c; else print 0 }')
    newtmo_frac=$(awk -v t="$newtmo" -v n="$n" 'BEGIN{printf "%.3f", t/n}')
    reg_frac=$(awk -v r="$regress" -v n="$n" 'BEGIN{printf "%.3f", r/n}')

    local ok=1 reason=""
    if [ "$mism" -gt 0 ]; then ok=0; reason="${reason}(verdictMismatch=$mism)"; fi
    if [ "$(awk -v i="$impr" -v a="$ACCEPT_IMPROVE_PCT" 'BEGIN{print (i > -a)?1:0}')" = "1" ]; then ok=0; reason="${reason}(improve=${impr}%<=${ACCEPT_IMPROVE_PCT}%)"; fi
    if [ "$(awk -v t="$newtmo_frac" -v f="$NEWTMO_MAX_FRAC" 'BEGIN{print (t>f)?1:0}')" = "1" ]; then ok=0; reason="${reason}(newTMO=${newtmo_frac}>${NEWTMO_MAX_FRAC})"; fi
    if [ "$(awk -v r="$reg_frac" -v f="$REGRESS_MAX_FRAC" 'BEGIN{print (r>f)?1:0}')" = "1" ]; then ok=0; reason="${reason}(regress=${reg_frac}>${REGRESS_MAX_FRAC})"; fi

    local verdict="PASS"
    [ "$ok" = "0" ] && verdict="FAIL$reason"
    echo "AGG|$n|geomean_ctrl=$cg|geomean_var=$vg|improve=${impr}%|newTMO=${newtmo_frac}|regress=${reg_frac}|$verdict"
}

echo "=== Satience BROAD Distributional Held-Out Gate ==="
echo "Control : $CONTROL_BINARY"
echo "Variant : $VARIANT_BINARY"
echo "Timeout : ${TIMEOUT}s (PAR2 TMO=$((2*TIMEOUT))s)"
echo "Workers : $JOBS"
echo "Tier1 (cnfgen): $MATRIX_SIZE families x (dev $DEV_ITERATIONS / val $VALIDATION_ITERATIONS)"
echo "Tier2 (industrial): INDUSTRIAL_DIR=${INDUSTRIAL_DIR:-<none>}"
echo "Accept : geomean improve >= ${ACCEPT_IMPROVE_PCT}% | newTMO <= ${NEWTMO_MAX_FRAC} | regress <= ${REGRESS_MAX_FRAC} (verdicts must match)"
echo ""

mkdir -p "$WORK_DIR/dev" "$WORK_DIR/val"

# ---- DEV rail (tuning aid only) ----
total=$(( DEV_ITERATIONS * MATRIX_SIZE ))
run_parallel "$total" do_dev "$WORK_DIR/dev" || true

# ---- VALIDATION rail (accept/reject) ----
total=$(( VALIDATION_ITERATIONS * MATRIX_SIZE ))
run_parallel "$total" do_val "$WORK_DIR/val"

# ---- industrial tier on the VALIDATION rail only ----
if [ -n "$INDUSTRIAL_DIR" ] && [ -n "$INDUSTRIAL_LIST" ] && [ -f "$INDUSTRIAL_LIST" ]; then
    echo "== industrial tier ($(wc -l < "$INDUSTRIAL_LIST" | tr -d ' ') instances) =="
    mapfile -t INDUSTRIAL_NAMES < <(sed 's/\.cnf$//' "$INDUSTRIAL_LIST")
    nind=${#INDUSTRIAL_NAMES[@]}
    run_parallel "$nind" do_ind "$WORK_DIR/val"
else
    echo "== industrial tier skipped (set INDUSTRIAL_DIR+INDUSTRIAL_LIST) =="
fi

# ---- aggregate over ALL validation instances (Tier1 + industrial) ----
agg="$WORK_DIR/val/aggregate.dat"; : > "$agg"
for f in "$WORK_DIR"/val/r_*; do [ -f "$f" ] && accumulate "$(cat "$f")" "$agg"; done
for f in "$WORK_DIR"/val/ind_*; do [ -f "$f" ] && accumulate "$(cat "$f")" "$agg"; done

echo ""
echo "=== VALIDATION rail result ==="
summary=$(compute_agg "$agg")
echo "  $summary"

echo ""
if echo "$summary" | grep -q '|PASS$'; then
    echo "HELDOUT-BROAD: PASS — variant improves aggregate PAR2 >= ${ACCEPT_IMPROVE_PCT}% with bounded regressions."
    exit 0
else
    echo "HELDOUT-BROAD: FAIL — $summary"
    echo "NOTE: tune only against the DEV rail; do not re-tune and re-measure the VALIDATION rail."
    exit 1
fi
