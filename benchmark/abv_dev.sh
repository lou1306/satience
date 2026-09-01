#!/bin/bash
# Hard DEV-rail A/B: control vs variant over a harder cnfgen matrix (parity=false),
# fresh seed-partitioned draws. Reports per-family sum/median PAR2 + TMO + per-
# instance conflict/move/LBD if STATS=1.
# USAGE: CONTROL_BINARY=/x VARIANT_BINARY=/y VARIANT_FLAGS="-satisfied-keep" DEV=8 TIMEOUT=20 JOBS=8 STATS=1 ./abv_dev.sh
CONTROL="${CONTROL_BINARY:?need CONTROL_BINARY}"
VARIANT="${VARIANT_BINARY:?need VARIANT_BINARY}"
VARIANT_FLAGS="${VARIANT_FLAGS:--satisfied-keep}"
DEV="${DEV:-8}"
TIMEOUT="${TIMEOUT:-20}"
JOBS="${JOBS:-8}"
STATS="${STATS:-0}"
TMO2=$((2*TIMEOUT))
cd "$(dirname "$0")"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
# Harder combinational matrix (larger than heldout-search; parity=false isolates CDCL).
MATRIX=(
  "s_r3_200_4.26|randkcnf 3 200 852"
  "s_r3_300_4.26|randkcnf 3 300 1278"
  "s_r3_300_4.8|randkcnf 3 300 1440"
  "s_r3_400_5.0|randkcnf 3 400 2000"
  "s_r4_150_9.0|randkcnf 4 150 1350"
  "s_r4_180_9.0|randkcnf 4 180 1620"
  "s_kcol4_gnp60|kcolor 4 gnp 60 0.4"
  "s_kcol5_gnp80|kcolor 5 gnp 80 0.35"
  "s_kcl6_gnp60|kclique 6 gnp 60 0.3"
  "s_tseitin_g17|tseitin randomodd grid 17 17"
  "s_tseitin_gnd90|tseitin randomodd gnd 90 6"
)
MS=${#MATRIX[@]}
Ntotal=$((MS*DEV))
entry_at(){ echo "${MATRIX[$(( $1 % MS ))]}"; }
echo "generating $Ntotal instances (parity off)..." >&2
for ((k=0;k<Ntotal;k++)); do
  IFS='|' read -r _fn fargs <<< "$(entry_at "$k")"
  kseed=$((k*7919))
  cnfgen -S "$kseed" $fargs > "$TMP/i_$k.cnf" 2>/dev/null &
  if (( (k+1)%JOBS==0 )); then wait; fi
done; wait
one(){ # $1 cmd $2 cnf -> "res par2 [conf mov scanL]" where stats in brackets if STATS
  local s e d ec res stats res2
  s=$(date +%s.%N)
  if [ "$STATS" = "1" ]; then
    timeout "$TIMEOUT" $1 "$2" >/dev/null 2>"$TMP/$$"
  else
    timeout "$TIMEOUT" $1 "$2" >/dev/null 2>&1
  fi
  ec=$?; e=$(date +%s.%N)
  d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
  case $ec in 10) res=SAT;;20) res=UNSAT;;124) res=TMO;;*) res=EC$ec;; esac
  if [ "$res" = TMO ]; then res2="$res $TMO2"; else res2="$res $d"; fi
  if [ "$STATS" = "1" ] && [ "$res" != TMO ]; then
    local ls=$(grep -oE "conflicts=[0-9]+|mov=[0-9]+|scanLits=[0-9]+|emaLBD=[0-9.]+" "$TMP/$$"|tr '\n' ' ')
    echo "$res2 $ls"
  else
    echo "$res2"
  fi
}
echo "running control + variant..." >&2
: > "$TMP/res"
for ((k=0;k<Ntotal;k++)); do
  ( read -r lc <<< "$(one "$CONTROL" "$TMP/i_$k.cnf")"
    read -r lv <<< "$(one "$VARIANT $VARIANT_FLAGS" "$TMP/i_$k.cnf")"
    echo "$((k%MS)) $lc | $lv" >> "$TMP/res"
  ) &
  if (( (k+1)%JOBS==0 )); then wait; fi
done; wait
python3 - "$MS" "$TMP/res" "$TMO2" "$STATS" <<'PY'
import sys,collections
MS=int(sys.argv[1]); f=sys.argv[2]; TMO2=int(sys.argv[3]); STATS=int(sys.argv[4])
names="r3_200_4.26 r3_300_4.26 r3_300_4.8 r3_400_5.0 r4_150_9.0 r4_180_9.0 kcol4_60 kcol5_80 kcl6_60 tg17 gnd90".split()
d=collections.defaultdict(list); agg=collections.defaultdict(lambda:[0,0,0,0,0])
allc=alv=0
for line in open(f):
    if '|' not in line: continue
    fam = int(line.split()[0])
    tok = line.split('|')
    cfields = tok[0].split()[1:]   # drop fam index
    vfields = tok[1].split()
    cr, cp = cfields[0] if cfields else 'EC', float(cfields[1]) if len(cfields)>1 else 0
    vr, vp = vfields[0] if vfields else 'EC', float(vfields[1]) if len(vfields)>1 else 0
    cs=cfields[2:]; vs=vfields[2:]
    d[fam].append((cp,vp))
    if STATS and len(cs)>=4 and cr!='TMO': allc += int(next((x.split('=')[1] for x in cs if x.startswith('conflicts')),0))
    if STATS and len(vs)>=4 and vr!='TMO': alv += int(next((x.split('=')[1] for x in vs if x.startswith('conflicts')),0))
tc=tv=0; csct=csvt=0
print(f"{'family':12} {'n':>3} {'ctlSum':>8} {'varSum':>8} {'ctlTMO':>6} {'varTMO':>6} {'newTMO':>6}")
for fam in range(MS):
    rows=d.get(fam,[]); cs=sum(r[0] for r in rows); vs=sum(r[1] for r in rows)
    ct=sum(1 for r in rows if r[0]>=TMO2); vt=sum(1 for r in rows if r[1]>=TMO2)
    newt=sum(1 for r in rows if r[0]<TMO2 and r[1]>=TMO2)
    csct+=ct; csvt+=vt; tc+=cs; tv+=vs
    print(f"{names[fam]:12} {len(rows):>3} {cs:8.2f} {vs:8.2f} {ct:6d} {vt:6d} {newt:6d}")
print(f"\nTOTAL ctlPAR2={tc:.1f} varPAR2={tv:.1f} delta={tv-tc:+.1f} ({(tv-tc)/max(tc,0.0001)*100:+.1f}%)  ctlTMO={csct} varTMO={csvt}")
if STATS: print(f"TOTAL conflicts: ctl={allc} var={alv}")
PY
