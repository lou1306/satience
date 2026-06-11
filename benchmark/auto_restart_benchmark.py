#!/usr/bin/env python3
"""
Automated restart policy benchmark - Makes code changes automatically
"""

import subprocess
import time
import json
import re
from pathlib import Path
from datetime import datetime

TEST_INSTANCES = [
    '11c893b7c37aeb53cdaf5f677dda0b7d.cnf',
    '1a3320d3cf32f211b3e7b875745713e2.cnf',
    '39835f263f4afe43886e31dfa6464e72.cnf',
    '274099073ca1be8ecc4123e63d24465a.cnf',
    '109aa0f5e177c1efb72f133a6f8c723b.cnf',
    '0f4576a6e7399336e11f0828d32263dd.cnf',
    '0f877a1f984f35fdfdce011cb7152123.cnf',
    '172ecb98a80b859e62612ff192a53729.cnf',
]

def run_benchmark(timeout=30):
    results = []
    for inst in TEST_INSTANCES:
        path = Path('benchmark/gbd_instances') / inst
        if not path.exists():
            continue
        try:
            start = time.time()
            result = subprocess.run(['./satience', str(path)], capture_output=True, text=True, timeout=timeout)
            elapsed = time.time() - start
            res = 'SAT' if result.returncode == 10 else ('UNSAT' if result.returncode == 20 else 'TIMEOUT')
            results.append((inst, res, elapsed))
        except subprocess.TimeoutExpired:
            results.append((inst, 'TIMEOUT', timeout))
    solved = sum(1 for _, r, _ in results if r in ['SAT', 'UNSAT'])
    return results, solved, len(results)

def test_config(name, timeout=30):
    print(f"\n{'='*70}")
    print(f"Testing: {name}")
    print('='*70)
    
    result = subprocess.run(['go', 'build', '-o', 'satience', 'cmd/satience/main.go'], capture_output=True, text=True)
    if result.returncode != 0:
        print(f"Compilation FAILED")
        return None
    
    results, solved, total = run_benchmark(timeout=timeout)
    print(f"Result: {solved}/{total} ({100*solved/total:.1f}%)")
    for inst, res, t in results:
        status = '✓' if res in ['SAT', 'UNSAT'] else 'TO'
        print(f"  {inst[:30]:30s}: {res:8s} {t:6.2f}s {status}")
    
    return {'name': name, 'solved': solved, 'total': total, 'solve_rate': solved/total, 'details': results}

def main():
    print("="*80)
    print("Automated Restart Policy Benchmark")
    print("="*80)
    
    all_results = {}
    
    # Config 1: BASELINE
    print("\n[1/7] BASELINE")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    result = test_config('BASELINE')
    if result: all_results['baseline'] = result
    
    # Config 2: Option 1 - Align (LBD≤3)
    print("\n[2/7] Option 1: Align restart with deletion (LBD≤3)")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    with open('internal/solver/solver_cdcl.go', 'r') as f:
        content = f.read()
    content = re.sub(r'if lbd <= 10 \{', 'if lbd <= 3 {  // OPTION1: Aligned with deletion', content)
    with open('internal/solver/solver_cdcl.go', 'w') as f:
        f.write(content)
    result = test_config('Option1: Align LBD≤3')
    if result: all_results['option1'] = result
    
    # Config 3: Option 2 - Protect LBD 3-5
    print("\n[3/7] Option 2: Protect LBD 3-5 from deletion")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    with open('internal/solver/solver_cdcl.go', 'r') as f:
        content = f.read()
    # Add protection for LBD=3
    content = re.sub(
        r'(// PROTECTION: Core glue clauses.*?\n.*?if lbd <= GlueLBDThreshold \{[^}]+\})',
        r'\1\n\n\t\t// OPTION2: Protect LBD 3-5\n\t\tif lbd <= 5 && size <= 5 && age < 200 {\n\t\t\tscore = -200.0\n\t\t}',
        content,
        flags=re.DOTALL
    )
    with open('internal/solver/solver_cdcl.go', 'w') as f:
        f.write(content)
    result = test_config('Option2: Protect LBD3-5')
    if result: all_results['option2'] = result
    
    # Config 4: Option 4 - Aggressive adaptive
    print("\n[4/7] Option 4: Aggressive adaptive restarts")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    with open('internal/solver/solver_cdcl.go', 'r') as f:
        content = f.read()
    content = re.sub(r'if s\.lastConflictLBD > int\(3\.0\*avgLBD\) && s\.lastConflictLBD > 20',
                    'if s.lastConflictLBD > int(1.5*avgLBD) && s.lastConflictLBD > 10  // OPTION4',
                    content)
    with open('internal/solver/solver_cdcl.go', 'w') as f:
        f.write(content)
    result = test_config('Option4: Aggressive adaptive')
    if result: all_results['option4'] = result
    
    # Config 5: Option 5 - Luby base 100
    print("\n[5/7] Option 5: Luby base=100 (less frequent)")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    with open('internal/solver/solver_cdcl.go', 'r') as f:
        content = f.read()
    content = re.sub(r'DefaultRestartBase\s+= \d+', 'DefaultRestartBase      = 100  // OPTION5', content)
    with open('internal/solver/solver_cdcl.go', 'w') as f:
        f.write(content)
    result = test_config('Option5: Luby base=100')
    if result: all_results['option5'] = result
    
    # Config 6: Option 6 - Luby base 25
    print("\n[6/7] Option 6: Luby base=25 (more frequent)")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    with open('internal/solver/solver_cdcl.go', 'r') as f:
        content = f.read()
    content = re.sub(r'DefaultRestartBase\s+= \d+', 'DefaultRestartBase      = 25  // OPTION6', content)
    with open('internal/solver/solver_cdcl.go', 'w') as f:
        f.write(content)
    result = test_config('Option6: Luby base=25')
    if result: all_results['option6'] = result
    
    # Config 7: Combined best options
    print("\n[7/7] Combined: Aggressive adaptive + LBD≤3 keep")
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    with open('internal/solver/solver_cdcl.go', 'r') as f:
        content = f.read()
    content = re.sub(r'if lbd <= 10 \{', 'if lbd <= 3 {  // COMBINED', content)
    content = re.sub(r'if s\.lastConflictLBD > int\(3\.0\*avgLBD\) && s\.lastConflictLBD > 20',
                    'if s.lastConflictLBD > int(1.5*avgLBD) && s.lastConflictLBD > 10',
                    content)
    with open('internal/solver/solver_cdcl.go', 'w') as f:
        f.write(content)
    result = test_config('Combined: Agg+Align')
    if result: all_results['combined'] = result
    
    # Save results
    timestamp = datetime.now().strftime('%Y%m%d_%H%M%S')
    output_file = f'restart_auto_results_{timestamp}.json'
    with open(output_file, 'w') as f:
        json.dump(all_results, f, indent=2)
    
    print(f"\n{'='*80}")
    print("FINAL SUMMARY")
    print('='*80)
    sorted_results = sorted(all_results.items(), key=lambda x: x[1]['solve_rate'], reverse=True)
    for name, data in sorted_results:
        print(f"{name:20s}: {data['solved']}/{data['total']} ({100*data['solve_rate']:5.1f}%)")
    
    subprocess.run(['git', 'checkout', 'internal/solver/solver_cdcl.go'], check=True)
    print(f"\nResults saved to: {output_file}")
    print("Baseline restored.")

if __name__ == '__main__':
    main()
