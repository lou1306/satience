#!/usr/bin/env python3
import re

# Parse our results
our_results = {}
with open("satience_fast_suite_results_20260624_122045.txt") as f:
    for line in f:
        match = re.match(r'\[\d+/\d+\] ([✓✗⊠?]) (\S+) .* - (\w+)', line)
        if match:
            symbol, instance, verdict = match.groups()
            our_results[instance] = verdict

# Parse MiniSat results (from previous run - these are the ground truth)
minisat_results = {}
try:
    with open("minisat_fast_results_20260622_124607.txt") as f:
        for line in f:
            match = re.match(r'\[\d+/\d+\] ([✓✗⊠?]) (\S+) .* - (\w+)', line)
            if match:
                symbol, instance, verdict = match.groups()
                minisat_results[instance] = verdict
except FileNotFoundError:
    print("MiniSat results file not found")
    exit(1)

# Compare
wrong = []
for instance, our_verdict in our_results.items():
    if instance in minisat_results:
        ms_verdict = minisat_results[instance]
        if our_verdict != ms_verdict and our_verdict != "TIMEOUT" and ms_verdict != "TIMEOUT":
            wrong.append((instance, ms_verdict, our_verdict))

print(f"Total instances: {len(our_results)}")
print(f"MiniSat instances compared: {len([i for i in our_results if i in minisat_results])}")
print(f"Wrong verdicts: {len(wrong)}")
if wrong:
    print("\nWRONG VERDICTS:")
    for inst, expected, got in wrong:
        print(f"  {inst}: expected {expected}, got {got}")
else:
    print("\n✓ No wrong verdicts! All solved instances match MiniSat.")
