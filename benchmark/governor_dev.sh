#!/usr/bin/env bash
set -u
# Targeted GOVERNOR DEV rail: instances where the search-governor (Det4/Det6)
# demonstrably matters, so a governor change can be validated on the failure
# class it targets instead of on rails where the spiral rarely appears.
#
# The anchor family is the ORDERING PRINCIPLE (cnfgen op N): structured (score
# ~0.78), weak-phase (PolImb ~0.32), ternary-heavy, no-glue, deep unguided
# cascade. Measured on a quiet box: governor ON solves op_16..24 in ~0.7-6s,
# while governor OFF (Det6 disabled) TMOs at 15s+ — a clean discriminating
# signal. `op N` is deterministic; `op N d` (graph ordering principle on a
# d-regular graph) adds fresh-draw variety via the -S seed.
#
# Usage:
#   CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var ./benchmark/governor_dev.sh
#   (control = governor ON = current defaults)
#   CRUN_ARGS/VRUN_ARGS add flags per side (e.g. VRUN_ARGS="-gov-spiral-pdec=1e9"
#   to disable Det6). VLIST=<file> sweeps N configs against one control.
CTL="${CONTROL_BINARY:-/tmp/ctl}"
VAR="${VARIANT_BINARY:-/tmp/var}"
TIMEOUT="${TIMEOUT:-15}"
JOBS="${JOBS:-4}"
CRUN_ARGS="${CRUN_ARGS:-}"
VRUN_ARGS="${VRUN_ARGS:-}"
DIR="$(mktemp -d /tmp/govdev.XXXX)"
if [ -n "${DEBUG:-}" ]; then trap 'echo "kept $DIR"' EXIT; else trap 'rm -rf "$DIR"; kill 0 2>/dev/null' EXIT; fi

echo "Control=$CTL Variant=$VAR timeout=${TIMEOUT}s jobs=${JOBS}"
echo "Generating governor-targeted (ordering-principle) instances..."

inst=()
# Fixed, deterministic anchor tier.
for n in 16 18 20 22 24; do
  f="$DIR/op_$n.cnf"; cnfgen op "$n" 2>/dev/null > "$f"; inst+=("$f")
done
# Fresh-draw tier: GOP on d-regular graphs (seeded) for distributional variety.
FOP_SEEDS="${FOP_SEEDS:-3}"
for dd in 4 5; do
  for i in $(seq 1 "$FOP_SEEDS"); do
    n=$((24 + i))
    f="$DIR/op${n}_d${dd}_s$i.cnf"
    cnfgen -S $((5000 + n*13 + dd)) op "$n" "$dd" 2>/dev/null > "$f"
    inst+=("$f")
  done
done
N=${#inst[@]}
echo "instances=$N"

if [ -n "${VLIST:-}" ] && [ -f "$VLIST" ]; then
  mapfile -t CFGS < "$VLIST"
else
  CFGS=( "$VRUN_ARGS" )
fi
printf '%s\n' "${CFGS[@]}" > "$DIR/cfgs.txt"
export CFG_FILE="$DIR/cfgs.txt" CTL VAR TIMEOUT CRUN_ARGS
# measure: prints "el ver" (ver=OK|TMO) for one solver invocation.
measure() { # args file
  local start end el rc
  start=$(date +%s.%N)
  eval "timeout \"$TIMEOUT\" $1 \"$2\"" >/dev/null 2>/dev/null
  rc=$?
  end=$(date +%s.%N)
  el=$(echo "$end $start" | awk '{printf "%.6f", $1-$2}')
  if [ $rc -eq 124 ]; then printf "%s TMO" "$el"; else printf "%s OK" "$el"; fi
}
export -f measure
run_pair() {
  local f="$1" cfg a b line=""
  set -- $(measure "$CTL $CRUN_ARGS" "$f"); line="$1 $2"
  mapfile -t CFG < "$CFG_FILE"
  for cfg in "${CFG[@]}"; do
    set -- $(measure "$VAR $cfg" "$f"); line="$line $1 $2"
  done
  printf '%s\n' "$line"   # one write() per instance -> atomic vs parallel peers
}
export -f run_pair

echo "configs=${#CFGS[@]}  (control=governor ON, ${CRUN_ARGS:-defaults})"
printf '%s\n' "${inst[@]}" | xargs -P "$JOBS" -I{} bash -c 'run_pair "{}"' > "$DIR/pairs.raw"
wait

awk -v t="$TIMEOUT" -v ncfg="${#CFGS[@]}" '
function par2(el,ver){ if(ver=="TMO") return 2.0*t; return el }
BEGIN{n=0}
{ n++
  cel=$1; cver=$2
  for(k=0;k<ncfg;k++){ j=3+2*k; vel=$j; vver=$(j+1)
    pc=par2(cel,cver); pv=par2(vel,vver)
    Sc[k]+=log(pc); Sv[k]+=log(pv)
    if(cver=="TMO" && vver=="OK") votes[k]++
    if(cver=="OK" && vver=="TMO") ct2v[k]++
  }
}
END{
  for(k=0;k<ncfg;k++){ gc=exp(Sc[k]/n); gv=exp(Sv[k]/n); impr=(gv-gc)/gc*100
    printf "cfg%d: ctl=%.4f var=%.4f improve=%+.2f%%  ctrl-solves-only=%d var-solves-only=%d\n", k, gc, gv, impr, ct2v[k], votes[k] }
}
' "$DIR/pairs.raw"
