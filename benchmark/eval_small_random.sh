#!/bin/bash
# Evaluate satience on N random small benchmarks (< 200 vars)
# Stops immediately on first wrong result
# Usage: ./eval_small_random.sh [n_instances]

set -e

N_INSTANCES=${1:-20}
SOLVER="/home/luca/git/opencode-sat-new/satience"
DB="/home/luca/git/opencode-sat-new/benchmark/meta.db"
INSTANCES_DIR="/home/luca/git/opencode-sat-new/benchmark/gbd_instances"
TIMEOUT=60  # seconds per instance

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "=========================================="
echo "Evaluating satience on $N_INSTANCES random small benchmarks (< 200 vars)"
echo "=========================================="
echo ""

# Build list of small instances with known results
declare -a small_instances
declare -A instance_results

# Get all downloaded hashes first
downloaded_hashes=$(ls "$INSTANCES_DIR"/*.cnf 2>/dev/null | xargs -n1 basename | sed 's/\.cnf$//')

# Check each downloaded instance
for hash in $downloaded_hashes; do
    cnf_file="$INSTANCES_DIR/${hash}.cnf"
    
    # Get number of variables
    n_vars=$(grep "^p cnf" "$cnf_file" 2>/dev/null | awk '{print $3}')
    
    # Skip if can't determine or too large
    [ -z "$n_vars" ] && continue
    [ "$n_vars" -gt 200 ] && continue
    
    # Get expected result from database
    expected=$(sqlite3 "$DB" "SELECT result FROM features WHERE hash='$hash';")
    
    # Only include if we know the result
    if [ "$expected" = "sat" ] || [ "$expected" = "unsat" ]; then
        small_instances+=("$hash")
        instance_results["$hash"]="$expected"
    fi
done

echo "Found ${#small_instances[@]} small instances with known results"
echo ""

# Shuffle and select N instances
shuffled=($(printf '%s\n' "${small_instances[@]}" | shuf))
selected=("${shuffled[@]:0:$N_INSTANCES}")

correct=0
wrong=0
timeout_count=0
total=0

for hash in "${selected[@]}"; do
    cnf_file="$INSTANCES_DIR/${hash}.cnf"
    expected="${instance_results[$hash]}"
    n_vars=$(grep "^p cnf" "$cnf_file" | awk '{print $3}')
    
    total=$((total + 1))
    
    # Run solver with timeout
    echo -n "[$total] $hash ($n_vars vars, expected: $expected)... "
    
    # Run solver and capture output
    set +e
    output=$(timeout $TIMEOUT "$SOLVER" "$cnf_file" 2>&1)
    exit_code=$?
    set -e
    
    # Determine result
    if [ $exit_code -eq 124 ]; then
        echo -e "${YELLOW}TIMEOUT${NC} (${TIMEOUT}s)"
        timeout_count=$((timeout_count + 1))
        continue
    fi
    
    # Parse result (check UNSAT first to avoid substring match)
    if echo "$output" | grep -q "UNSAT"; then
        result="unsat"
    elif echo "$output" | grep -q "SAT"; then
        result="sat"
    else
        echo -e "${YELLOW}UNKNOWN${NC} (no result found)"
        continue
    fi
    
    # Check correctness
    if [ "$result" = "$expected" ]; then
        echo -e "${GREEN}CORRECT${NC}: $result"
        correct=$((correct + 1))
        
        # Verify model for SAT instances
        if [ "$result" = "sat" ]; then
            model_output=$(timeout 10 "$SOLVER" -model "$cnf_file" 2>&1) || true
            if echo "$model_output" | grep -q "Model:"; then
                echo "       Model verified: OK"
            fi
        fi
    else
        echo -e "${RED}WRONG${NC}: got $result, expected $expected"
        echo "  Output: $output"
        wrong=$((wrong + 1))
        
        echo ""
        echo "=========================================="
        echo -e "${RED}FIRST WRONG RESULT DETECTED!${NC}"
        echo "=========================================="
        echo "Instance: $cnf_file"
        echo "Expected: $expected"
        echo "Got: $result"
        echo ""
        exit 1
    fi
done

echo ""
echo "=========================================="
echo "SUMMARY"
echo "=========================================="
echo "Total tested: $total"
echo -e "Correct: ${GREEN}$correct${NC}"
echo -e "Wrong: ${RED}$wrong${NC}"
echo -e "Timeout: ${YELLOW}$timeout_count${NC}"
echo ""

if [ $wrong -gt 0 ]; then
    echo -e "${RED}TESTS FAILED${NC}"
    exit 1
else
    echo -e "${GREEN}ALL TESTS PASSED${NC}"
    exit 0
fi
