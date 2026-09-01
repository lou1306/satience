#!/bin/bash
# fast_suite_stability.sh — NOISE-AWARE stability harness for the fast suite.
#
# WHY (measurement hardening)
# run_satience_fast_suite.sh runs each instance once with CONCURRENCY=4 and
# reports a single mean PAR2. On a load-noisy machine the per-instance times
# (and even verdicts) jitter enough to swamp a genuine A/B delta — LRB looked
# 1.91s, interval=4000 looked 2.68s, when both were ~os noise on a handful of
# amplifier instances. This harness repeats each instance R times and publishes
# a ROBUST (per-instance median) PAR2, per-instance variance, and the list of
# volatile "amplifier" instances — so a variant's single-run PAR2 can be
# judged against *measured* run-to-run spread, not a single point estimate.
#
# Reports:
#   - Robust PAR2  = mean over instances of the per-instance MEDIAN par2
#                    (immune to a single noisy repeat on any one instance).
#   - Mean PAR2 + SD: classic aggregate plus its spread.
#   - Amplifier instances: per-instance CV above AMPLIFIER_CV and/or a verdict
#     that flips across repeats.
#   - Verdict-flip rate across repeats (solved-set instability).
#
# Usage: bash benchmark/fast_suite_stability.sh
# Env:
#   BINARY        solver binary              (default ../satience_bench)
#   EXTRA_FLAGS   extra flags passed to BINARY (default empty)
#   REPEATS       repeats per instance       (default 3)
#   TIMEOUT       per-run seconds            (default 30; PAR2 TMO = 2*TIMEOUT)
#   CONCURRENCY   parallel instance workers  (default 1 — serial minimizes jitter)
#   INSTANCE_FILE instances list file        (default minisat_fast_suite/instances.txt)
#   INSTANCE_DIR  dir containing instances   (default gbd_instances)
#   AMPLIFIER_CV  CV fraction marking an amplifier (default 0.35)
#
# Exit: 0 always (informational).

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="${BINARY:-$REPO_ROOT/satience_bench}"
EXTRA_FLAGS="${EXTRA_FLAGS:-}"
REPEATS=${REPEATS:-3}
TIMEOUT=${TIMEOUT:-30}
CONCURRENCY=${CONCURRENCY:-1}
INSTANCE_FILE="${INSTANCE_FILE:-$SCRIPT_DIR/minisat_fast_suite/instances.txt}"
INSTANCE_DIR="${INSTANCE_DIR:-$SCRIPT_DIR/gbd_instances}"
AMPLIFIER_CV=${AMPLIFIER_CV:-0.35}

case "$BINARY" in /*) ;; *) BINARY="$REPO_ROOT/$BINARY" ;; esac
if [ ! -x "$BINARY" ]; then echo "ERROR: binary not executable: $BINARY" >&2; exit 2; fi

TMO2=$((2 * TIMEOUT))

# toolset: awk median/mean/cv over newline-separated numbers from stdin.
awk_stat() { awk -v tmo="$TMO2" '
    { v[NR]=$1 }
    END {
        if (NR==0) { printf "0 0 0 1 0\n"; exit }
        n=NR
        # sum for mean
        s=0; for (i=1;i<=n;i++) s+=v[i]; mean=s/n
        # median
        m=n%2 ? v[(n+1)/2] : (v[n/2]+v[n/2+1])/2
        # sd of par2
        ss=0; for (i=1;i<=n;i++){d=v[i]-mean; ss+=d*d} sd=sqrt(ss/n)
        # cv deflated away from 0 mean
        cv = (mean>0.001) ? sd/mean : 0
        # flip count: any value != first (verdict/par2 tokens passed as token1)
        printf "%.3f %.3f %.3f %.3f\n", m, mean, sd, cv
    }'; }

# par2_of RESULT DURATION
par2_of() { if [ "$1" = "TMO" ]; then echo "$TMO2"; else awk "BEGIN{printf \"%.3f\", $2}"; fi; }

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

INSTANCE_LIST=()
while IFS= read -r i; do INSTANCE_LIST+=("$i"); done < "$INSTANCE_FILE"
TOTAL=${#INSTANCE_LIST[@]}

# run_one_inst INSTANCE_IDX INSTANCE FILE-> writes "repeats..." info
run_one_inst() {
    local num="$1"
    local instance="$2"
    local out="$WORK/$num"
    local file="$INSTANCE_DIR/$instance"
    if [ ! -f "$file" ]; then echo "NOTFOUND" > "$out"; return; fi
    local r res dur
    for ((r=0; r<REPEATS; r++)); do
        local s e d ec
        s=$(date +%s.%N)
        timeout "$TIMEOUT" "$BINARY" $EXTRA_FLAGS "$file" > /dev/null 2>&1
        ec=$?; e=$(date +%s.%N)
        d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
        case $ec in
            10) res="SAT";; 20) res="UNSAT";; 124) res="TMO";; *) res="EC=$ec";;
        esac
        echo "$res|$d|$(par2_of "$res" "$d")" >> "$out"
    done
}

# launch with concurrency gate
LAUNCH_CUR=0
for ((n=0; n<TOTAL; n++)); do
    run_one_inst "$n" "${INSTANCE_LIST[$n]}" &
    LAUNCH_CUR=$((LAUNCH_CUR+1))
    if [ "$LAUNCH_CUR" -ge "$CONCURRENCY" ]; then wait -n; LAUNCH_CUR=$((LAUNCH_CUR-1)); fi
done
wait

echo "=== Fast-suite stability harness ==="
echo "Binary   : $BINARY"
echo "Repeats  : $REPEATS  Timeout: ${TIMEOUT}s (PAR2 TMO=$TMO2)  Concurrency: $CONCURRENCY"
echo "Instances: $TOTAL  Amplifier CV threshold: $AMPLIFIER_CV"
echo ""
printf "%-12s %-8s %-9s %-9s %-9s %-9s %s\n" "instance" "solved" "median" "mean" "sd" "cv" "verdicts"
printf "%-12s %-8s %-9s %-9s %-9s %-9s %s\n" "--------" "------" "------" "----" "--" "--" "--------"

RobustSum=0
MeanSum=0
Amplifiers=0
Flips=0
for ((n=0; n<TOTAL; n++)); do
    instance="${INSTANCE_LIST[$n]}"
    out="$WORK/$n"
    [ ! -f "$out" ] && continue
    read -r firstline < "$out"
    [ "$firstline" = "NOTFOUND" ] && { echo "$instance NOTFOUND"; continue; }

    # per-repeat par2 list and verdict token list
    : > "$WORK/par2_$n"; : > "$WORK/ver_$n"
    while IFS='|' read -r v d p; do [ -z "$v" ] && continue; echo "$p" >> "$WORK/par2_$n"; echo "$v" >> "$WORK/ver_$n"; done < "$out"
    read -r medianv meanv sdv cv <<< "$(sort -n "$WORK/par2_$n" | awk_stat)"

    # solved-set for this instance: count repeats that solved (not TMO / not EC).
    solved=0
    while read -r v; do [ "$v" = "SAT" ] || [ "$v" = "UNSAT" ] && solved=$((solved+1)); done < "$WORK/ver_$n"

    # verdict flip = distinct verdicts across repeats (excluding EC noise)
    ndistinct=$(sort -u "$WORK/ver_$n" | grep -vE "^EC=" | wc -l)
    if [ "$ndistinct" -gt 1 ]; then
        echo "    ** FLIP: $instance verdicts: $(tr '\n' ' ' < "$WORK/ver_$n") (median ${medianv}s)"
        Flips=$((Flips+1))
        # a genuine SAT<->UNSAT flip across repeats would be severe; flag it
        if [ "$(grep -cE '^SAT$' "$WORK/ver_$n")" -gt 0 ] && [ "$(grep -cE '^UNSAT$' "$WORK/ver_$n")" -gt 0 ]; then
            echo "    *** SOUNDNESS-FLAG: same instance SAT and UNSAT across repeats!"
        fi
    fi

    is_amp=0
    if awk -v cv="$cv" -v t="$AMPLIFIER_CV" 'BEGIN{exit !(cv>t)}'; then is_amp=1; fi
    [ "$ndistinct" -gt 1 ] && is_amp=1
    RobustSum=$(awk "BEGIN{print $RobustSum+$medianv}")
    MeanSum=$(awk "BEGIN{print $MeanSum+$meanv}")
    mark=""
    [ "$is_amp" = "1" ] && { mark="  <-- amplifier"; Amplifiers=$((Amplifiers+1)); }

    printf "%-12s %-8s %-9s %-9s %-9s %-9s %s%s\n" \
        "$instance" "${solved}/$REPEATS" "$medianv" "$meanv" "$sdv" "$cv" \
        "$(tr '\n' ' ' < "$WORK/ver_$n")" "$mark"
done

echo ""
echo "=== Summary ==="
echo "Robust PAR2 (per-instance median): $(awk "BEGIN{printf \"%.3f\", $RobustSum/$TOTAL}")s"
echo "Classic PAR2 (per-instance mean) : $(awk "BEGIN{printf \"%.3f\", $MeanSum/$TOTAL}")s"
echo "Amplifier instances              : $Amplifiers (CV>$AMPLIFIER_CV or verdict flip)"
echo "Verdict-flip instances           : $Flips"
echo ""
echo "Read: a variant's single-run PAR2 delta is only meaningful if it exceeds the"
echo "      spread implied by the amplifier set above (or use per-instance medians)."
