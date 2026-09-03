#!/usr/bin/env bash
set -u
# Targeted SEARCH-HEAVY DEV rail for branch-heuristic comparison.
# The broad rail is preprocessing-dominated (geomean ~0.7s), so it barely
# stresses branching. This rail deliberately uses instances where the CDCL
# search dominates, so branch-heuristic differences actually appear.
# Usage: CONTROL_BINARY=/tmp/ctl VARIANT_BINARY=/tmp/var ./benchmark/branch_dev.sh
CTL="${CONTROL_BINARY:-/tmp/ctl}"
VAR="${VARIANT_BINARY:-/tmp/var}"
TIMEOUT="${TIMEOUT:-15}"
JOBS="${JOBS:-8}"
DIR="$(mktemp -d /tmp/branchdev.XXXX)"
trap 'rm -rf "$DIR"; kill 0 2>/dev/null' EXIT

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
run_one() { # bin args file
  local bin="$1" f="$3" t="$TIMEOUT"
  local start="" end="" el="" rc=""
  start=$(date +%s.%N)
  timeout "$t" $bin "$2" "$f" >/dev/null 2>/dev/null
  rc=$?
  end=$(date +%s.%N)
  el=$(echo "$end $start" | awk '{printf "%.6f", $1-$2}')
  if [ $rc -eq 124 ]; then printf "%s %s TMO\n" "$el" "$t"; else printf "%s %s OK\n" "$el" "$el"; fi
}
export -f run_one

echo "Running paired (ctl/var)..."
printf '%s\n' "${inst[@]}" | xargs -P "$JOBS" -I{} bash -c 'run_one "$CTL" "$CRUN_ARGS" "{}"' > "$DIR/ctl.raw"
printf '%s\n' "${inst[@]}" | xargs -P "$JOBS" -I{} bash -c 'run_one "$VAR" "$VRUN_ARGS" "{}"' > "$DIR/var.raw"
wait

paste "$DIR/ctl.raw" "$DIR/var.raw" | awk -v t="$TIMEOUT" '
function par2(el,ver){ if(ver=="TMO") return 2.0*t; return el }
BEGIN{n=0;sc=0;sv=0;ct2v=0;v2c=0;reg=0}
{ n++
  cel=$1; cver=$3
  vel=$4; vver=$6
  pc=par2(cel,cver); pv=par2(vel,vver)
  sc+=log(pc); sv+=log(pv)
  if(cver=="TMO" && vver=="OK") v2c++
  if(cver=="OK" && vver=="TMO") ct2v++
  if(cver=="OK" && vver=="OK" && vel>cel) reg++
}
END{
  gc=exp(sc/n); gv=exp(sv/n)
  impr=(gv-gc)/gc*100
  printf "N=%d\n", n
  printf "geomean ctl=%.4f  var=%.4f  improve=%+.2f%%\n", gc, gv, impr
  printf "ctrl-that-variant-SOLVES: %d   variant-that-ctrl-SOLVES: %d   ctrl-regressions(slower): %d\n", v2c, ct2v, reg
}
'
