#!/bin/bash
# Calibrate / generate a HARD cnfgen DEV rail (1-30s/instance, parity off).
# In calibrate mode: for each candidate family spec, generate one draw at the
# given seed, time it with $BINARY (parity=false), print size+time+exit.
# Usage: BINARY=./satience BINARY=/tmp/satience_prof D=-/tmp/deval ./calib_devrail.sh <calibrate|gen>
set -e
BINARY="${BINARY:-/tmp/satience_prof}"
D="${D:-/tmp/deval}"
TIMEOUT="${TIMEOUT:-50}"
PROBE_SEED="${PROBE_SEED:-12345}"
mkdir -p "$D"

# name|cnfgen args|draws-per-family
CANDIDATES=(
  "c_r3_300|randkcnf 3 300 1278"
  "c_r3_400|randkcnf 3 400 1704"
  "c_r3_500|randkcnf 3 500 2130"
  "c_r4_200|randkcnf 4 200 1800"
  "c_r4_250|randkcnf 4 250 2250"
  "c_kc80|kcolor 4 gnp 80 0.35"
  "c_kc100|kcolor 5 gnp 100 0.35"
  "c_tg19|tseitin randomodd grid 19 19"
  "c_tg21|tseitin randomodd grid 21 21"
  "c_gnd90|tseitin randomodd gnd 90 6"
)

if [ "$1" = "calibrate" ]; then
  for entry in "${CANDIDATES[@]}"; do
    nm="${entry%%|*}"; args="${entry#*|}"
    cnfgen -S "$PROBE_SEED" $args > "$D/$nm.cnf" 2>/dev/null
    s=$(date +%s.%N)
    timeout "$TIMEOUT" "$BINARY" -parity=false "$D/$nm.cnf" >/dev/null 2>&1
    ec=$?
    e=$(date +%s.%N)
    d=$(awk "BEGIN{printf \"%.2f\", $e-$s}")
    echo "$nm  size=$(stat -c%s "$D/$nm.cnf" 2>/dev/null)  exit=$ec  t=${d}s"
  done
else
  echo "usage: $0 calibrate"
fi
