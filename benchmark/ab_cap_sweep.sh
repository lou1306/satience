#!/bin/bash
# DEV-rail cap sweep for prefer-true (learned-only) using the HARDER heldout
# matrix (search-heavy: randkcnf 150/200, rand4 100, tseitin grid17/gnd80) with
# -no-classify -parity=false to isolate the scan heuristic. Partitions DEV seeds
# (idx*7919) only -- the validation seed rail is untouched.
# USAGE: CAPS="0 4 6 8 10 12 16" DEV=8 TIMEOUT=15 JOBS=8 ./ab_cap_sweep.sh
CTL="${CTL:-/tmp/satience_ctl}"
PT="${PT:-/tmp/satience_pt}"
CAPS="${CAPS:-0 4 6 8 10 12 16}"
DEV="${DEV:-8}"
TIMEOUT="${TIMEOUT:-15}"
JOBS="${JOBS:-8}"
TMO2=$((2 * TIMEOUT))
NOISE=0.25
REGRESS_PCT=25
EXTRA="-parity=false"
cd "$(dirname "$0")"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

MATRIX=(
  "s_rand3_150_3.5|randkcnf 3 150 525"
  "s_rand3_150_4.26|randkcnf 3 150 639"
  "s_rand3_150_4.8|randkcnf 3 150 720"
  "s_rand3_200_4.26|randkcnf 3 200 852"
  "s_rand3_200_4.8|randkcnf 3 200 960"
  "s_rand4_100_9.0|randkcnf 4 100 900"
  "s_kcolor4_gnp40|kcolor 4 gnp 40 0.4"
  "s_kcolor4_gnp50|kcolor 4 gnp 50 0.3"
  "s_kclique6_gnp50|kclique 6 gnp 50 0.3"
  "s_tseitin_grid17|tseitin randomodd grid 17 17"
  "s_tseitin_gnd80|tseitin randomodd gnd 80 6"
)
MS=${#MATRIX[@]}
Ntotal=$((MS * DEV))

entry_at() { # $1 idx -> prints "MNAME|CNFGENARGS"; seed derived as idx*7919
  echo "${MATRIX[$(( $1 % MS ))]}"
}

# run_one: $1=cmd(space-joined) $2=cnf -> prints "RES DUR"
run_one() {
  local cmd="$1" cnf="$2" s e d
  s=$(date +%s.%N)
  timeout "$TIMEOUT" $cmd "$cnf" >/dev/null 2>&1
  local ec=$?
  e=$(date +%s.%N)
  d=$(awk "BEGIN{printf \"%.3f\", $e-$s}")
  case $ec in
    10) printf 'SAT %s' "$d" ;; 20) printf 'UNSAT %s' "$d" ;;
    124) printf 'TMO %s' "$d" ;; *) printf 'EC%s %s' "$ec" "$d" ;;
  esac
}

echo "generating $Ntotal instances (parity off)..." >&2
for ((k = 0; k < Ntotal; k++)); do
  IFS='|' read -r _fname fargs <<< "$(entry_at "$k")"
  kseed=$(( k * 7919 ))
  cnfgen -S "$kseed" $fargs > "$TMP/i_$k.cnf" 2>/dev/null &
  if (( (k + 1) % JOBS == 0 )); then wait; fi
done
wait

echo "control (cached across caps)..." >&2
for ((k = 0; k < Ntotal; k++)); do
  ( printf '%s' "$(run_one "$CTL $EXTRA" "$TMP/i_$k.cnf")" > "$TMP/c_$k" ) &
  if (( (k + 1) % JOBS == 0 )); then wait; fi
done
wait

printf '%-36s %8s %8s %7s %7s %6s %6s\n' "cap" "sumT" "medT" "cTMO" "vTMO" "newTMO" "regr"
csum=0; cmax=0; cTMO=0; cvals=()
for ((k = 0; k < Ntotal; k++)); do
  read -r c_res c_dur <<< "$(cat "$TMP/c_$k")"
  if [ "$c_res" = "TMO" ]; then cpar2=$TMO2; cTMO=$((cTMO+1)); else cpar2=$c_dur; fi
  cvals+=("$cpar2")
  csum=$(awk "BEGIN{print $csum+$cpar2}")
  if [ "$(awk "BEGIN{print ($cpar2>$cmax)?1:0}")" = "1" ]; then cmax=$cpar2; fi
done
cmed=$(printf '%s\n' "${cvals[@]}" | sort -n | awk '{v[NR]=$1}END{n=NR;m=n%2?v[(n+1)/2]:(v[n/2]+v[n/2+1])/2;printf "%.3f",m}')

for cap in $CAPS; do
  samp=0; vTMO=0; newTMO=0; regr=0; vvals=(); vmax=0
  for ((k = 0; k < Ntotal; k++)); do
    read -r v_res v_dur <<< "$(run_one "$PT $EXTRA -prefer-true-cap=$cap" "$TMP/i_$k.cnf")"
    if [ "$v_res" = "TMO" ]; then vpar2=$TMO2; vTMO=$((vTMO+1)); else vpar2=$v_dur; fi
    read -r c_res c_dur <<< "$(cat "$TMP/c_$k")"
    if [ "$c_res" = "TMO" ]; then cpar2=$TMO2; else cpar2=$c_dur; fi
    vvals+=("$vpar2")
    samp=$(awk "BEGIN{print $samp+$vpar2}")
    if [ "$(awk "BEGIN{print ($vpar2>$vmax)?1:0}")" = "1" ]; then vmax=$vpar2; fi
    if [ "$(awk "BEGIN{print ($cpar2<$TMO2 && $vpar2>=$TMO2)?1:0}")" = "1" ]; then newTMO=$((newTMO+1)); fi
    if [ "$(awk "BEGIN{print ($cpar2>$NOISE && $vpar2>$cpar2)?1:0}")" = "1" ]; then
      pct=$(awk -v v="$vpar2" -v c="$cpar2" 'BEGIN{printf "%.1f",100*(v-c)/c}')
      if [ "$(awk "BEGIN{print ($pct>$REGRESS_PCT)?1:0}")" = "1" ]; then regr=$((regr+1)); fi
    fi
  done
  vmed=$(printf '%s\n' "${vvals[@]}" | sort -n | awk '{v[NR]=$1}END{n=NR;m=n%2?v[(n+1)/2]:(v[n/2]+v[n/2+1])/2;printf "%.3f",m}')
  printf '%-36s %8.1f %8s %7d %7d %6d %6d\n' "cap=$cap" "$samp" "$vmed" "$cTMO" "$vTMO" "$newTMO" "$regr"
done
echo "CONTROL sumT=$csum medT=$cmed maxT=$cmax cTMO=$cTMO ($Ntotal draws, TIMEOUT=${TIMEOUT}s)"
