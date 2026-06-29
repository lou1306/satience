#!/bin/bash
# Run satience on benchmark instances

BINARY="../satience"
TIMEOUT=30
INSTANCES_FILE="/tmp/test_instances.txt"
RESULTS_FILE="/tmp/satience_results_$(date +%Y%m%d_%H%M%S).txt"

echo "=== Satience Benchmark ===" > "$RESULTS_FILE"
echo "Binary: $BINARY" >> "$RESULTS_FILE"
echo "Timeout: ${TIMEOUT}s per instance" >> "$RESULTS_FILE"
echo "Date: $(date)" >> "$RESULTS_FILE"
echo "" >> "$RESULTS_FILE"

count=0
solved=0
timeout_count=0
wrong=0

while read -r instance; do
    count=$((count + 1))
    basename=$(basename "$instance")
    
    # Run satience
    result=$(timeout $TIMEOUT $BINARY "$instance" 2>&1 | grep "^s " | head -1)
    
    if [ -z "$result" ]; then
        echo "[$count/$count] ⊠ $basename - TIMEOUT (${TIMEOUT}s)" | tee -a "$RESULTS_FILE"
        timeout_count=$((timeout_count + 1))
    else
        echo "[$count/$count] $basename - $result" | tee -a "$RESULTS_FILE"
        solved=$((solved + 1))
    fi
done < "$INSTANCES_FILE"

echo "" >> "$RESULTS_FILE"
echo "=== Summary ===" >> "$RESULTS_FILE"
echo "Total: $count" >> "$RESULTS_FILE"
echo "Solved: $solved" >> "$RESULTS_FILE"
echo "Timeout: $timeout_count" >> "$RESULTS_FILE"
echo "Results file: $RESULTS_FILE"
