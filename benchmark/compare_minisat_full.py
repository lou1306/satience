#!/usr/bin/env python3
"""
Comprehensive comparison: Satience vs MiniSat
Test all instances < 200 vars, immediately investigate discrepancies.
"""

import subprocess
import time
import sys
from pathlib import Path

TIMEOUT = 30
INSTANCE_DIR = Path("benchmark/gbd_instances")
MINISAT = "/home/luca/bin/minisat"

def run_solver(solver_cmd, instance_path, timeout=TIMEOUT):
    """Run solver and return (result, time, stats)."""
    try:
        start = time.time()
        result = subprocess.run(
            solver_cmd + [str(instance_path)],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        elapsed = time.time() - start
        output = result.stdout + result.stderr
        
        # Parse result
        if "UNSATISFIABLE" in output:
            sat_result = "UNSAT"
        elif "SATISFIABLE" in output:
            sat_result = "SAT"
        else:
            sat_result = "UNKNOWN"
        
        # Parse statistics (for satience verbose output)
        stats = {}
        for line in output.split('\n'):
            if ':' in line:
                parts = line.split(':')
                if len(parts) == 2:
                    key = parts[0].strip()
                    val = parts[1].strip()
                    if key in ['Conflicts', 'Decisions', 'Variables', 'Clauses']:
                        try:
                            stats[key] = int(val)
                        except:
                            pass
        
        return sat_result, elapsed, stats
    except subprocess.TimeoutExpired:
        return "TIMEOUT", timeout, {}
    except Exception as e:
        return f"ERROR: {e}", 0, {}

def main():
    # Get all CNF files with < 200 vars
    cnf_files = sorted(INSTANCE_DIR.glob("*.cnf"))
    instances = []
    
    for cnf in cnf_files:
        with open(cnf, 'r') as f:
            for line in f:
                if line.startswith('p cnf'):
                    parts = line.split()
                    if len(parts) >= 3:
                        n_vars = int(parts[2])
                        if n_vars <= 200:
                            instances.append((cnf, n_vars))
                    break
    
    print(f"Testing {len(instances)} instances with ≤200 variables")
    print(f"Comparing Satience vs MiniSat (timeout: {TIMEOUT}s)")
    print("="*120)
    print(f"{'Instance':<35} {'Vars':<6} {'Satient':<12} {'MiniSat':<12} {'Ratio':<8} {'Status':<10}")
    print("="*120)
    
    results = []
    discrepancies = []
    
    for inst_path, n_vars in sorted(instances):
        inst_name = inst_path.name
        
        # Run Satience
        sat_result, sat_time, sat_stats = run_solver(["./satience"], inst_path)
        
        # Run MiniSat
        ms_result, ms_time, ms_stats = run_solver([MINISAT], inst_path)
        
        # Calculate ratio
        if sat_time < TIMEOUT and ms_time > 0.001:
            ratio = sat_time / ms_time
            ratio_str = f"{ratio:.2f}x"
        else:
            ratio_str = "-"
        
        # Check for discrepancies
        if sat_result != ms_result:
            status = "✗ DISCREPANCY"
            discrepancies.append({
                'instance': inst_name,
                'satience': sat_result,
                'minisat': ms_result,
                'sat_time': sat_time,
                'ms_time': ms_time
            })
        elif sat_result == "TIMEOUT":
            status = "⏱ TIMEOUT"
        elif ms_result == "TIMEOUT":
            status = "✓ (MS timeout)"
        else:
            status = "✓"
        
        sat_str = f"{sat_time:.3f}s" if sat_time < TIMEOUT else "TIMEOUT"
        ms_str = f"{ms_time:.3f}s" if ms_time < TIMEOUT else "TIMEOUT"
        
        print(f"{inst_name:<35} {n_vars:<6} {sat_str:<12} {ms_str:<12} {ratio_str:<8} {status}")
        
        results.append({
            'name': inst_name,
            'vars': n_vars,
            'sat_result': sat_result,
            'sat_time': sat_time,
            'ms_result': ms_result,
            'ms_time': ms_time,
            'ratio': ratio if ratio_str != "-" else None
        })
    
    # Summary
    print("="*120)
    print("SUMMARY:")
    
    matching = sum(1 for r in results if r['sat_result'] == r['ms_result'])
    mismatch = sum(1 for r in results if r['sat_result'] != r['ms_result'])
    sat_timeout = sum(1 for r in results if r['sat_result'] == 'TIMEOUT')
    ms_timeout = sum(1 for r in results if r['ms_result'] == 'TIMEOUT')
    
    print(f"Total instances: {len(results)}")
    print(f"Matching results: {matching}/{len(results)}")
    print(f"Mismatches: {mismatch}/{len(results)}")
    print(f"Satience timeouts: {sat_timeout}")
    print(f"MiniSat timeouts: {ms_timeout}")
    
    # Performance comparison (for solved instances)
    solved_both = [r for r in results if r['sat_time'] < TIMEOUT and r['ms_time'] < TIMEOUT and r['ratio']]
    if solved_both:
        ratios = [r['ratio'] for r in solved_both]
        median_ratio = sorted(ratios)[len(ratios)//2]
        avg_ratio = sum(ratios) / len(ratios)
        print(f"\nPerformance (for {len(solved_both)} instances solved by both):")
        print(f"  Median slowdown: {median_ratio:.2f}x")
        print(f"  Average slowdown: {avg_ratio:.2f}x")
        print(f"  Min slowdown: {min(ratios):.2f}x")
        print(f"  Max slowdown: {max(ratios):.2f}x")
    
    # Report discrepancies
    if discrepancies:
        print("\n" + "="*120)
        print("⚠️  DISCREPANCIES FOUND - Investigating immediately:")
        print("="*120)
        
        for disc in discrepancies:
            print(f"\nInstance: {disc['instance']}")
            print(f"  Satience: {disc['satience']} ({disc['sat_time']:.3f}s)")
            print(f"  MiniSat:  {disc['minisat']} ({disc['ms_time']:.3f}s)")
            
            # Investigate by running with verbose/model flags
            inst_path = INSTANCE_DIR / disc['instance']
            
            print(f"\n  Investigating Satience...")
            result = subprocess.run(
                ["./satience", "-verbose", "-model", str(inst_path)],
                capture_output=True,
                text=True,
                timeout=TIMEOUT
            )
            output = result.stdout + result.stderr
            
            if "SATISFIABLE" in output:
                print(f"    Satience claims SAT")
                # Extract and verify model
                model_lines = [l for l in output.split('\n') if l.startswith('v ') and l != 'v']
                if model_lines:
                    print(f"    Model: {model_lines[0][:100]}...")
            elif "UNSATISFIABLE" in output:
                print(f"    Satience claims UNSAT")
            
            print(f"\n  Investigating MiniSat...")
            result = subprocess.run(
                [MINISAT, str(inst_path)],
                capture_output=True,
                text=True,
                timeout=TIMEOUT
            )
            ms_output = result.stdout + result.stderr
            if "SATISFIABLE" in ms_output:
                print(f"    MiniSat claims SAT")
            elif "UNSATISFIABLE" in ms_output:
                print(f"    MiniSat claims UNSAT")
    
    return len(discrepancies) == 0

if __name__ == "__main__":
    success = main()
    sys.exit(0 if success else 1)
