#!/bin/bash
# Verify SAT models for all instances we solved as SAT

BINARY="../satience_bench"
INSTANCE_FILE="minisat_fast_suite/instances.txt"
RESULTS_FILE="satience_fast_suite_results_20260624_122045.txt"

echo "=== Verifying SAT Models ==="

# Extract SAT instances from results
grep "✓" "$RESULTS_FILE" | grep "SAT" | awk '{print $3}' | while read instance; do
    FILE="gbd_instances/$instance"
    if [ -f "$FILE" ]; then
        # Run with -model flag to get model
        $BINARY -model="$FILE.model" "$FILE" > /tmp/verify_out.txt 2>&1
        EXIT_CODE=$?
        
        if [ $EXIT_CODE -eq 10 ]; then
            # Verify model satisfies all clauses
            if grep -q "s SATISFIABLE" /tmp/verify_out.txt; then
                echo "✓ $instance - Model verified"
            else
                echo "✗ $instance - Model verification FAILED"
            fi
        else
            echo "? $instance - Unexpected exit code: $EXIT_CODE"
        fi
    fi
done
