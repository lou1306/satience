#!/bin/bash
# Focused re-tune of the satisfied-keep watch handling on a hard cnfgen DEV rail.
# Control (satisfied-keep OFF, defaults) is run once and cached; each config is
# satisfied-keep ON + one swept param. Reports sum PAR2 / TMO / newTMO per config.
# USAGE: CONTROL_BINARY=/x DEV=8 TIMEOUT=20 JOBS=12 ./retune_dev.sh
CONTROL="${CONTROL_BINARY:?need CONTROL_BINARY}"
DEV="${DEV:-8}"
TIMEOUT="${TIMEOUT:-20}"
JOBS="${JOBS:-12}"
TMO2=$((2*TIMEOUT))
cd "$(dirname "$0")"
W=/tmp/retune; rm -rf "$W"; mkdir -p "$W"
# Refined discriminating families (drop all-TMO dead ones).
MATRIX=(
  "r3_200_4.26|randkcnf 3 200 852"
  "r3_300_4.26|randkcnf 3 300 1278"
  "r3_300_4.8|randkcnf 3 300 1440"
  "r4_150_9.0|randkcnf 4 150 1350"
  "kcl6_60|kclique 6 gnp 60 0.3"
  "kcol4_60|kcolor 4 gnp 60 0.4"
)
MS=${#MATRIX[@]}
Ntotal=$((MS*DEV))
entry_at(){ echo "${MATRIX[$(( $1 % MS ))]}"; }
echo "generating $Ntotal..." >&2
for ((k=0;k<Ntotal;k++)); do IFS='|' read -r _fn fargs <<< "$(entry_at "$k")"; kseed=$((k*7919)); cnfgen -S "$kseed" $fargs > "$W/i_$k.cnf" 2>/dev/null & if (( (k+1)%JOBS==0 )); then wait; fi; done; wait
one(){ # $1 cmd $2 cnf -> "res par2"
  local s e d ec res
  s=$(date +%s.%N); timeout "$TIMEOUT" $1 "$2" >/dev/null 2>&1; ec=$?; e=$(date +%s.%N)
  d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
  case $ec in 10) res=SAT;;20) res=UNSAT;;124) res=TMO;;*) res=EC$ec;; esac
  if [ "$res" = TMO ]; then echo "$res $TMO2"; else echo "$res $d"; fi
}
echo "control (cached)..." >&2
for ((k=0;k<Ntotal;k++)); do ( one "$CONTROL" "$W/i_$k.cnf" > "$W/c_$k" ) & if (( (k+1)%JOBS==0 )); then wait; fi; done; wait
# Each line: savename, variant-invoke-suffix appended to /tmp/satience's flags (satisfied-keep ON always)
CONFIGS=(
  "sk_default|"
  "ratio6|-restart-glucose-ratio=6"
  "ratio15|-restart-glucose-ratio=15"
  "rbase100|-restart-base=100"
  "rbase400|-restart-base=400"
  "lbd005|-lbd-scale=0.05"
  "lbdoff|-lbd-scale=-1"
  "pratio12|-restart-props-dec=12"
  "levelcap|-restart-level-cap=20"
)
printf '%-12s %8s %8s %6s %6s %6s\n' "config" "sumT" "medT" "cTMO" "vTMO" "newTMO"
echo "  cfg: satisfied-keep ON + flag" 
printf '%-12s %8s %8s %6s %6s %6s\n' "CONTROL" "-" "-" "-" "-" "-"
for entry in "${CONFIGS[@]}"; do
  name="${entry%%|*}"; flags="${entry#*|}"
  sum=0; med=(); vtm=0; newt=0; ctmp=()
  for ((k=0;k<Ntotal;k++)); do
    # variant = /tmp/satience_sk -satisfied-keep + flags
    read -r vr vp <<< "$(one "/tmp/satience_sk -satisfied-keep $flags" "$W/i_$k.cnf")"
    read -r cr cp <<< "$(cat "$W/c_$k")"
    sum=$(awk "BEGIN{print $sum+$vp}"); med+=("$vp")
    if [ "$vr" = TMO ]; then vtm=$((vtm+1)); fi
    if [ "$cr" != TMO ] && [ "$vr" = TMO ]; then newt=$((newt+1)); fi
  done
  mid=$(printf '%s\n' "${med[@]}" | sort -n | awk '{a[NR]=$1}END{n=NR;print (n%2)?a[(n+1)/2]:(a[n/2]+a[n/2+1])/2}')
  printf '%-12s %8.1f %8s %6s %6d %6d\n' "$name" "$sum" "$mid" "-" "$vtm" "$newt"
done
