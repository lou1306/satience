#!/bin/bash
# Benchmark MiniSat Fast Suite with 30s timeout

BINARY="./satience_bench"
INSTANCES_DIR="./benchmark/gbd_instances"
INSTANCES_LIST="./benchmark/minisat_fast_suite/instances.txt"
RESULTS_FILE="./benchmark/minisat_fast_results_$(date +%Y%m%d_%H%M%S).txt"
TIMEOUT=30

echo "=== MiniSat Fast Suite Benchmark ===" | tee "$RESULTS_FILE"
echo "Binary: $BINARY" | tee -a "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT}s per instance" | tee -a "$RESULTS_FILE"
echo "Date: $(date)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

total=0
solved=0
sat_count=0
unsat_count=0
timeout_count=0
total_time=0

while IFS= read -r instance; do
    [ -z "$instance" ] && continue
    
    total=$((total + 1))
    instance_path="$INSTANCES_DIR/$instance"
    
    if [ ! -f "$instance_path" ]; then
        echo "[$total/32] ✗ $instance - NOT FOUND" | tee -a "$RESULTS_FILE"
        continue
    fi
    
    # Get instance size
    vars=$(head -20 "$instance_path" | grep "^p " | awk '{print $3}')
    clauses=$(head -20 "$instance_path" | grep "^p " | awk '{print $4}')
    
    # Run with timeout
    start_time=$(date +%s.%N)
    result=$(timeout $TIMEOUT $BINARY "$instance_path" 2>&1)
    exit_code=$?
    end_time=$(date +%s.%N)
    elapsed=$(echo "$end_time - $start_time" | bc)
    
    # Parse result
    if [ $exit_code -eq 124 ]; then
        status="TIMEOUT"
        timeout_count=$((timeout_count + 1))
        echo "[$total/32] ⊠ $instance (${vars}v, ${clauses}c) - TIMEOUT (${elapsed}s)" | tee -a "$RESULTS_FILE"
    elif echo "$result" | grep -q "s SATISFIABLE"; then
        status="SAT"
        sat_count=$((sat_count + 1))
        solved=$((solved + 1))
        echo "[$total/32] ✓ $instance (${vars}v, ${clauses}c) - SAT (${elapsed}s)" | tee -a "$RESULTS_FILE"
    elif echo "$result" | grep -q "s UNSATISFIABLE"; then
        status="UNSAT"
        unsat_count=$((unsat_count + 1))
        solved=$((solved + 1))
        echo "[$total/32] ✓ $instance (${vars}v, ${clauses}c) - UNSAT (${elapsed}s)" | tee -a "$RESULTS_FILE"
    else
        status="UNKNOWN/ERROR"
        echo "[$total/32] ? $instance (${vars}v, ${clauses}c) - $status (${elapsed}s, exit=$exit_code)" | tee -a "$RESULTS_FILE"
    fi
    
    total_time=$(echo "$total_time + $elapsed" | bc)
    
done < "$INSTANCES_LIST"

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
echo "Total instances: $total" | tee -a "$RESULTS_FILE"
echo "Solved: $solved/$total ($(echo "scale=1; $solved * 100 / $total" | bc)%)" | tee -a "$RESULTS_FILE"
echo "  - SAT: $sat_count" | tee -a "$RESULTS_FILE"
echo "  - UNSAT: $unsat_count" | tee -a "$RESULTS_FILE"
echo "Timeouts: $timeout_count" | tee -a "$RESULTS_FILE"
echo "Total time: ${total_time}s" | tee -a "$RESULTS_FILE"
echo "Average time (solved): $(echo "scale=2; $total_time / $solved" | bc)s" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
echo "Results saved to: $RESULTS_FILE"
