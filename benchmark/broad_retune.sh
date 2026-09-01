#!/bin/bash
# Broad re-tune: each config = satisfied-keep ON + one knob value, scored on the
# cnfgen-Dev rail (cached /tmp/retune/i_*.cnf, parity off) AND de2b584 time.
# Control reference: cnfgen-Dev sumPAR2=550.2 TMO=11 ; de2b584 ~5.9s
DEVDIR="${DEVDIR:-/tmp/retune}"
TMO=20; TMO2=40
DEB=/home/luca/git/opencode-sat-new/benchmark/gbd_instances/de2b584eeec81d08bd16c3c15e78625a.cnf
CONFIGS=(
  "skdefault|"
  "lbd005|-lbd-scale=0.05"
  "rbase100|-restart-base=100"
  "rbase400|-restart-base=400"
  "ratio6|-restart-glucose-ratio=6"
  "ratio20|-restart-glucose-ratio=20"
  "pdec200|-restart-props-dec=200"
  "lvlcap20|-restart-level-cap=20"
  "phaseflip03|-restart-phase-flip=0.3"
  "adphase02|-adaptive-phase-flip=0.2"
  "adphase08|-adaptive-phase-flip=0.8"
  "bump35|-bump-amount=35"
  "decay092|-initial-decay=0.92"
  "tier1_3|-lbd-tier1=3"
  "dbtrig20|-del-trigger-ratio=2.0"
  "shrsh8|-db-shrink-thresh=8"
  "vivify100|-vivify-period=100"
  "subsum50|-subsumption-period=50"
  "msrestart|-geometric"
)
eval_cfg(){ # $1 name $2 flags
  local name="$1" flags="$2" sum=0 vtm=0 i tt
  local vals=()
  local fifo="$OR/sem_$name"; mkfifo "$fifo"; exec 9<>"$fifo"; rm "$fifo"
  for i in $(seq 1 2); do echo >&9; done   # per-config concurrency cap
  local cnt=0
  for f in "$DEVDIR"/i_*.cnf; do
    read -u 9
    ( s=$(date +%s.%N); timeout "$TMO" /tmp/satience_sk -satisfied-keep $flags "$f" >/dev/null 2>&1; ec=$?
      e=$(date +%s.%N); d=$(awk "BEGIN{printf \"%.3f\",$e-$s}")
      case $ec in 10|20) echo OK $d;; *) echo TMO $TMO2;; esac
    ) > "$OR/$name.$(basename $f).res"; echo >&9
  done
  wait; exec 9>&-
  for f in "$DEVDIR"/i_*.cnf; do
    read -r r tt <<< "$(cat "$OR/$name.$(basename $f).res")"
    if [ "$r" = TMO ]; then vtm=$((vtm+1)); fi
    sum=$(awk "BEGIN{print $sum+$tt}")
  done
  local db=99
  for run in 1 2; do
    /usr/bin/time -f "%e" timeout 25 /tmp/satience_sk -satisfied-keep $flags "$DEB" >/dev/null 2>/tmp/db_
    db=$(awk -v a="$(tail -1 /tmp/db_)" -v b="$db" 'BEGIN{print (a<b)?a:b}')
  done
  if [ -z "$flags" ]; then deb_lbl="(default)"; fi
  echo "RESULT $name sum=$sum TMO=$vtm de2b584=${db}"
}
OR=/tmp/broad_rt; rm -rf "$OR"; mkdir -p "$OR"
printf '%-14s %8s %5s %8s\n' "config" "cgnPAR2" "cgnTMO" "de2b584"
i=0
for entry in "${CONFIGS[@]}"; do
  name="${entry%%|*}"; flags="${entry#*|}"
  eval_cfg "$name" "$flags" > "$OR/out.$name" &
  i=$((i+1))
  [ $((i%6)) -eq 0 ] && wait
done
wait
grep -h RESULT "$OR"/out.* | sed -E 's/RESULT ([^ ]+) sum=([0-9.]+) TMO=([0-9]+) de2b584=([0-9.]+)/\1 \2 \3 \4/' | sort -k2 -n | awk '{printf "%-14s %8.1f %5s %8s\n",$1,$2,$3,$4}'
