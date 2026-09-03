#!/usr/bin/env bash
set -u
# Targeted SEARCH-HEAVY DEV rail for branch-heuristic comparison.
# The broad rail is preprocessing-dominated (geomean ~0.7s), so it barely
# stresses branching. This rail deliberately uses instances where the CDCL
# search dominates, so branch-heuristic differences actually appear.
# Usage: CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var ./benchmark/branch_dev.sh
#   (branch_dev cares only about PAR2 + verdicts; verdict mismatch is not a hard
#    fail here — it is a targeted search DEV rail, not the acceptance rail.)
CTL="${CONTROL_BINARY:-/tmp/ctl}"
VAR="${VARIANT_BINARY:-/tmp/var}"
TIMEOUT="${TIMEOUT:-15}"
JOBS="${JOBS:-8}"
CRUN_ARGS="${CRUN_ARGS:-}"
VRUN_ARGS="${VRUN_ARGS:-}"
DIR="$(mktemp -d /tmp/branchdev.XXXX)"
if [ -n "${DEBUG:-}" ]; then
  trap 'echo "kept $DIR"' EXIT
else
  trap 'rm -rf "$DIR"; kill 0 2>/dev/null' EXIT
fi

echo "Control=$CTL Variant=$VAR timeout=${TIMEOUT}s jobs=${JOBS}"
echo "Generating search-heavy instances..."
gen_rand() { # target
  cnfgen -S $((1000+$1)) randkcnf 3 280 1240 2>/dev/null > "$2"
}
gen_rand2() { # target
  cnfgen -S $((2000+$1)) randkcnf 3 350 1525 2>/dev/null > "$2"
}
gen_rand4() { # target
  cnfgen -S $((3000+$1)) randkcnf 4 220 1700 2>/dev/null > "$2"
}
gen_kcol() { # target
  cnfgen -S $((4000+$1)) kcolor 3 gnp 120 0.35 2>/dev/null > "$2"
}
inst=()
for i in $(seq 1 6); do f="$DIR/rand3_a$i.cnf"; gen_rand $i "$f"; inst+=("$f"); done
for i in $(seq 1 6); do f="$DIR/rand3_b$i.cnf"; gen_rand2 $i "$f"; inst+=("$f"); done
for i in $(seq 1 5); do f="$DIR/rand4_c$i.cnf"; gen_rand4 $i "$f"; inst+=("$f"); done
for i in $(seq 1 3); do f="$DIR/kcol_d$i.cnf"; gen_kcol $i "$f"; inst+=("$f"); done
N=${#inst[@]}
echo "instances=$N"

export CTL VAR TIMEOUT CRUN_ARGS VRUN_ARGS

# If VLIST names a file, run the control once then EVERY config (one per line)
# against the same paired control in a single run (efficient sweep; each config
# shares the control instance). Otherwise VRUN_ARGS is a single config.
if [ -n "${VLIST:-}" ] && [ -f "$VLIST" ]; then
  mapfile -t CFGS < "$VLIST"
else
  CFGS=( "$VRUN_ARGS" )
fi
printf '%s\n' "${CFGS[@]}" > "$DIR/cfgs.txt"
export CFG_FILE="$DIR/cfgs.txt" CTL VAR TIMEOUT CRUN_ARGS
time_one() { # bin args file  -> prints "el ver"
  local bin="$1" args="$2" f="$3" t="$TIMEOUT"
  local start="" end="" el="" rc=""
  start=$(date +%s.%N)
  eval "timeout \"$t\" \"$bin\" $args \"$f\"" >/dev/null 2>/dev/null
  rc=$?
  end=$(date +%s.%N)
  el=$(echo "$end $start" | awk '{printf "%.6f", $1-$2}')
  if [ $rc -eq 124 ]; then printf "%s TMO" "$el"; else printf "%s OK" "$el"; fi
}
export -f time_one

# Tight pairing: control (and each config) run back-to-back per instance
# (bounds load-drift between the pair on a noisy machine).
run_pair() { # file ; outputs "cel cver <for each cfg: el ver>"
  local f="$1" cfg
  local cel cver first second
  set -- $(time_one "$CTL" "$CRUN_ARGS" "$f"); cel=$1; cver=$2
  printf "%s %s" "$cel" "$cver"
  mapfile -t CFG < "$CFG_FILE"
  for cfg in "${CFG[@]}"; do
    set -- $(time_one "$VAR" "$cfg" "$f"); first=$1; second=$2
    printf " %s %s" "$first" "$second"
  done
  printf "\n"
}
export -f run_pair

echo "configs=${#CFGS[@]}  jobs=${JOBS} (control=${CRUN_ARGS:-vsids})"
printf '%s\n' "${inst[@]}" | xargs -P "$JOBS" -I{} bash -c 'run_pair "{}"' > "$DIR/pairs.raw"
wait
awk 'END{printf "pairs.raw: lines=%d maxfields=%d\n", NR, NF}' "$DIR/pairs.raw"
[ -n "${DEBUG:-}" ] && { cp "$DIR/pairs.raw" /tmp/pairs_debug.raw; echo "first line: $(head -1 "$DIR/pairs.raw")"; }

awk -v t="$TIMEOUT" -v ncfg="${#CFGS[@]}" '
function par2(el,ver){ if(ver=="TMO") return 2.0*t; return el }
BEGIN{n=0}
{ n++
  cel=$1; cver=$2
  for(k=0;k<ncfg;k++){
    j=3+2*k
    vel=$j; vver=$(j+1)
    pc=par2(cel,cver); pv=par2(vel,vver)
    Nc[k]=n
    Sc[k]+=log(pc); Sv[k]+=log(pv)
    if(cver=="TMO" && vver=="OK") votes[k]++
    if(cver=="OK" && vver=="TMO") ct2v[k]++
    if(cver=="OK" && vver=="OK" && vel>cel) reg[k]++
  }
}
END{
  for(k=0;k<ncfg;k++){
    gc=exp(Sc[k]/Nc[k]); gv=exp(Sv[k]/Nc[k])
    impr=(gv-gc)/gc*100
    printf "cfg%d: ctl=%.4f var=%.4f improve=%+.2f%%  ctrl-solves-only=%d var-solves-only=%d regress=%d\n", k, gc, gv, impr, ct2v[k], votes[k], reg[k]
  }
}
' "$DIR/pairs.raw"
