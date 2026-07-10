#!/bin/bash

BINARY="../satience_bench"
TIMEOUT_SEC=30
INSTANCE_FILE="minisat_fast_suite/instances.txt"
RESULTS_FILE="satience_fast_suite_results_$(date +%Y%m%d_%H%M%S).txt"

echo "=== Satience Fast Suite Benchmark ===" | tee "$RESULTS_FILE"
echo "Binary: $BINARY" | tee -a "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT_SEC}s per instance" | tee -a "$RESULTS_FILE"
echo "Date: $(date)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

TOTAL=0
SAT=0
UNSAT=0
TIMEOUT=0

while IFS= read -r instance; do
    TOTAL=$((TOTAL + 1))
    FILE="gbd_instances/$instance"
    
    if [ ! -f "$FILE" ]; then
        echo "[$TOTAL/72] ? $instance - FILE NOT FOUND" | tee -a "$RESULTS_FILE"
        continue
    fi
    
    # Get file stats
    VARS=$(head -20 "$FILE" | grep "^p " | awk '{print $3}')
    CLAUSES=$(head -20 "$FILE" | grep "^p " | awk '{print $4}')
    
    # Run with timeout and capture exit code
    START=$(date +%s.%N)
    timeout ${TIMEOUT_SEC}s $BINARY -lbd-scale 15 "$FILE" > /tmp/output.txt 2>&1
    EXIT_CODE=$?
    END=$(date +%s.%N)
    DURATION=$(echo "$END - $START" | bc)
    
    # Determine verdict from exit code
    if [ $EXIT_CODE -eq 10 ]; then
        VERDICT="SAT"
        SAT=$((SAT + 1))
        SYMBOL="✓"
    elif [ $EXIT_CODE -eq 20 ]; then
        VERDICT="UNSAT"
        UNSAT=$((UNSAT + 1))
        SYMBOL="✗"
    elif [ $EXIT_CODE -eq 124 ]; then
        VERDICT="TIMEOUT"
        TIMEOUT=$((TIMEOUT + 1))
        SYMBOL="⊠"
    else
        VERDICT="ERROR (exit=$EXIT_CODE)"
        SYMBOL="?"
    fi
    
    echo "[$TOTAL/72] $SYMBOL $instance (${VARS}v, ${CLAUSES}c) - $VERDICT (${DURATION}s)" | tee -a "$RESULTS_FILE"
    
done < "$INSTANCE_FILE"

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
echo "Total: $TOTAL" | tee -a "$RESULTS_FILE"
echo "SAT: $SAT" | tee -a "$RESULTS_FILE"
echo "UNSAT: $UNSAT" | tee -a "$RESULTS_FILE"
echo "TIMEOUT: $TIMEOUT" | tee -a "$RESULTS_FILE"
echo "Solve Rate: $(echo "scale=1; ($SAT + $UNSAT) * 100 / $TOTAL" | bc)%" | tee -a "$RESULTS_FILE"
