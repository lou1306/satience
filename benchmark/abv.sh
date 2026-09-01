#!/bin/bash
# Interleaved A/B control(CONTROL) vs variant(VARIANT) over INSTANCE_FILE (or dir).
# Per-instance back-to-back ctl then var to cancel drift.
CONTROL="${CONTROL:-/tmp/satience_prof}"
VARIANT="${VARIANT:-/tmp/satience_ms}"
FLAGS="${FLAGS:-}"
TIMEOUT="${TIMEOUT:-60}"
INST="${INST:-benchmark/minisat_fast_suite/instances.txt}"
DIR="${DIR:-benchmark/gbd_instances}"
cd "$(dirname "$0")/.."
mkdir -p /tmp/abv
one(){ /usr/bin/time -f "%e" timeout "$TIMEOUT" $2 $FLAGS "$DIR/$3" >/dev/null 2>/tmp/abv/t; echo "$? $(tail -1 /tmp/abv/t)"; }
{
  while IFS= read -r inst; do
    [ -z "$inst" ] && continue
    c=($(one x "$CONTROL" "$inst"))
    s=($(one x "$VARIANT" "$inst"))
    # normalize: 124=timeout
    cs=${c[1]}; ss=${s[1]}
    [ "${c[0]}" = "124" ] && cs="$TIMEOUT"
    [ "${s[0]}" = "124" ] && ss="$TIMEOUT"
    echo "$inst  ${c[0]}  $cs  ${s[0]}  $ss"
  done < "$INST"
} | tee /tmp/abv_report.txt
