#!/bin/bash
# Quick comparison of satience vs MiniSat on key instances

SATIENCE="./satience"
TIMEOUT=30

echo "======================================================================================================"
echo "SATIENCE vs MiniSat Quick Benchmark"
echo "======================================================================================================"
printf "%-40s %6s %8s %8s %10s %10s %8s\n" "Instance" "Vars" "Clauses" "Result" "Satience(s)" "MiniSat(s)" "Ratio"
echo "------------------------------------------------------------------------------------------------------"

for instance in benchmark/gbd_instances/{algebra_xor_20_sat,algebra_xor_40_sat,arg_chain_50_sat,arg_chain_100_sat,random_k3_50_sat,random_k3_75_sat,tseitin_grid_4x4_sat,tseitin_grid_5x5_sat,php_5p_6h_sat,php_6p_5h_unsat}.cnf; do
    if [ ! -f "$instance" ]; then
        continue
    fi
    
    # Get vars/clauses
    header=$(grep "^p cnf" "$instance")
    vars=$(echo $header | awk '{print $3}')
    clauses=$(echo $header | awk '{print $4}')
    name=$(basename "$instance")
    
    # Run satience
    start=$(date +%s.%N)
    sat_out=$($SATIENCE -verbose "$instance" 2>&1)
    sat_end=$(date +%s.%N)
    sat_time=$(echo "$sat_end - $start" | bc)
    
    # Parse satience result
    if echo "$sat_out" | grep -q "^SAT"; then
        sat_result="SAT"
    elif echo "$sat_out" | grep -q "^UNSAT"; then
        sat_result="UNSAT"
    else
        sat_result="TIMEOUT"
    fi
    
    # Run MiniSat
    start=$(date +%s.%N)
    mini_out=$(timeout $TIMEOUT minisat "$instance" /dev/null 2>&1)
    mini_end=$(date +%s.%N)
    mini_time=$(echo "$mini_end - $start" | bc)
    
    # Parse MiniSat result
    if echo "$mini_out" | grep -qi "SAT"; then
        mini_result="SAT"
    elif echo "$mini_out" | grep -qi "UNSAT"; then
        mini_result="UNSAT"
    else
        mini_result="TIMEOUT"
    fi
    
    # Calculate ratio
    if [ "$mini_time" != "0" ] && [ -n "$mini_time" ]; then
        ratio=$(echo "scale=2; $sat_time / $mini_time" | bc)
    else
        ratio="N/A"
    fi
    
    # Check match
    if [ "$sat_result" = "$mini_result" ]; then
        result=$sat_result
    else
        result="MISMATCH($sat_result/$mini_result)"
    fi
    
    printf "%-40s %6s %8s %8s %10.3f %10.3f %8s\n" "$name" "$vars" "$clauses" "$result" "$sat_time" "$mini_time" "$ratio"
done

echo "======================================================================================================"
