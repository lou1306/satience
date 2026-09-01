#!/bin/bash
# run_floor.sh — R-1 floor micro-benchmark runner (go/no-go gate).
#
# Compares satience's minimal per-watch propagation loop vs an equivalent
# C++/-O2 (minisat-grade) loop on IDENTICAL flat arrays, no parse/init/GC in
# the timed region. If Go's flat+unsafe ns/watch is >=2x the C++ ns/watch,
# the propagation-representation rewrite cannot close the per-prop gap and is
# abandoned.
#
# Usage: ./run_floor.sh     (must run from benchmark/propagation_bench)
# Requires: go, g++
set -eu
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "building C++ floor benchmark (g++ -O2 -march=x86-64-v3)..."
g++ -O2 -march=x86-64-v3 -o "$DIR/cpp/floor_cpp" "$DIR/cpp/floor.cpp"

cpp_out="$("$DIR/cpp/floor_cpp" | grep -oE 'ns/watch=[0-9.]+' | cut -d= -f2)"
echo "C++ flat -O2 v3    : ${cpp_out} ns/watch"

echo "building Go floor benchmark..."
cd "$DIR/.."
go_out="$(GOAMD64=v3 go test -bench=. -benchtime=1.5s -run='^$' ./propagation_bench/ 2>&1 | grep -E 'BenchmarkGoFlat')"
echo "$go_out"

go_unsafe=$(echo "$go_out" | grep GoFlatUnsafe | awk '{print $3}')
echo "Go flat+unsafe    : $(awk -v n="$go_unsafe" 'BEGIN{printf "%.3f", n/8000000}') ns/watch"

ratio=$(awk -v a="$go_unsafe" -v b="$cpp_out" 'BEGIN{if(b>0) printf "%.3f", (a/8000000)/b}')
echo "ratio GoFlatUnsafe/C++ = ${ratio}"
if awk -v r="$ratio" 'BEGIN{exit !(r >= 2.0)}'; then
    echo "VERDICT: GO/NO-GO -> ABORT (Go floor >=2x C++ floor; rewrite cannot close gap)"
else
    echo "VERDICT: GO/NO-GO -> PROCEED (Go floor <2x C++; rewrite has headroom)"
fi
