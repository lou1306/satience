#!/bin/bash
SATIENCE="../satience_bench"
MINISAT="/home/luca/bin/minisat"
TIMEOUT=30

echo "Instance                                   Vars  Clauses Satience     MiniSat"
echo "------------------------------------------------------------------------------------------------------"

while IFS= read -r instance; do
    [ -z "$instance" ] && continue
    instance_path="gbd_instances/$instance"
    [ ! -f "$instance_path" ] && continue
    
    header=$(grep "^p cnf" "$instance_path" 2>/dev/null || echo "p cnf 0 0")
    vars=$(echo $header | awk '{print $3}')
    clauses=$(echo $header | awk '{print $4}')
    name=$(basename "$instance" .cnf)
    
    # Satience
    start=$(date +%s.%N)
    result=$(timeout $TIMEOUT $SATIENCE "$instance_path" 2>&1)
    exit=$?
    time=$(echo "$(date +%s.%N) - $start" | bc)
    [ $exit -eq 124 ] && sat_result="TIMEOUT" || sat_result=$(echo "$result" | grep -q "SATISFIABLE" && echo "SAT" || echo "UNSAT")
    
    # MiniSat
    start=$(date +%s.%N)
    result=$(timeout $TIMEOUT $MINISAT "$instance_path" /dev/null 2>&1)
    exit=$?
    mini_time=$(echo "$(date +%s.%N) - $start" | bc)
    [ $exit -eq 124 ] && mini_result="TIMEOUT" || mini_result=$(echo "$result" | grep -qi "SAT" && echo "SAT" || echo "UNSAT")
    
    match="✓"
    [ "$sat_result" != "$mini_result" ] && match="✗"
    
    printf "%-40s %6s %8s %-12s %-12s %s\n" "$name" "$vars" "$clauses" "$sat_result ${time}s" "$mini_result ${mini_time}s" "$match"
done < ./minisat_fast_suite/instances.txt | head -40
