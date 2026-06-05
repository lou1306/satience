#!/usr/bin/env python3
"""
Performance regression benchmark runner for satience SAT solver.

This script runs the solver on a set of standard benchmark instances
and compares performance against baseline values to detect regressions.

Usage:
    python3 benchmark_regression.py [--baseline] [--json]

Options:
    --baseline    Update baseline values with current performance
    --json        Output results in JSON format
    --timeout     Timeout per instance in seconds (default: 60)
"""

import subprocess
import time
import json
import os
import sys
from pathlib import Path
from dataclasses import dataclass, asdict
from typing import Optional, Dict, List

@dataclass
class InstanceResult:
    """Results from running solver on a single instance"""
    name: str
    status: str  # "SAT", "UNSAT", "TIMEOUT", "ERROR"
    conflicts: int
    decisions: int
    iterations: int
    learned_clauses: int
    time_seconds: float
    expected_sat: Optional[bool]
    regression: bool = False
    regression_details: Optional[Dict] = None

@dataclass
class Baseline:
    """Baseline performance values for an instance"""
    name: str
    conflicts: int
    decisions: int
    time_seconds: float

# Baseline performance values (to be populated with actual measurements)
BASELINES: Dict[str, Baseline] = {
    # Add baselines as they are measured
    # Example:
    # "tseitin_grid_3x3_sat.cnf": Baseline(
    #     name="tseitin_grid_3x3_sat.cnf",
    #     conflicts=25,
    #     decisions=50,
    #     time_seconds=0.5
    # ),
}

# Test instances from different families
TEST_INSTANCES = [
    # Tseitin grid (structured, propagation-heavy)
    "tseitin_grid_3x3_sat.cnf",
    "tseitin_grid_4x4_unsat.cnf",
    "tseitin_grid_5x5_unsat.cnf",
    
    # XOR/Equality (algebraic structures)
    "algebra_xor_20_sat.cnf",
    "algebra_xor_30_unsat.cnf",
    
    # Chain instances
    "arg_chain_50_sat.cnf",
    "arg_chain_100_sat.cnf",
    
    # Random structured
    "random_k3_50_sat.cnf",
    "random_k3_100_unsat.cnf",
    
    # Pigeonhole (theoretically hard)
    "php_5p_6h_sat.cnf",
    "php_6p_5h_unsat.cnf",
]

def run_solver(instance_path: str, timeout: int = 60) -> InstanceResult:
    """Run satience solver on an instance and collect statistics"""
    
    solver_path = Path(__file__).parent.parent / "satience"
    if not solver_path.exists():
        return InstanceResult(
            name=Path(instance_path).name,
            status="ERROR",
            conflicts=0,
            decisions=0,
            iterations=0,
            learned_clauses=0,
            time_seconds=0,
            expected_sat=None,
            regression=False
        )
    
    start_time = time.time()
    
    try:
        # Run solver with verbose output to capture statistics
        result = subprocess.run(
            [str(solver_path), "-verbose", instance_path],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        
        elapsed = time.time() - start_time
        
        # Parse statistics from verbose output
        stats = parse_solver_output(result.stdout)
        
        # Determine SAT/UNSAT status
        status = "UNKNOWN"
        if "s SATISFIABLE" in result.stdout:
            status = "SAT"
        elif "s UNSATISFIABLE" in result.stdout:
            status = "UNSAT"
        
        return InstanceResult(
            name=Path(instance_path).name,
            status=status,
            conflicts=stats.get("conflicts", 0),
            decisions=stats.get("decisions", 0),
            iterations=stats.get("iterations", 0),
            learned_clauses=stats.get("learned", 0),
            time_seconds=elapsed,
            expected_sat=None,  # Would need to lookup from metadata
            regression=False
        )
        
    except subprocess.TimeoutExpired:
        return InstanceResult(
            name=Path(instance_path).name,
            status="TIMEOUT",
            conflicts=0,
            decisions=0,
            iterations=0,
            learned_clauses=0,
            time_seconds=timeout,
            expected_sat=None,
            regression=False
        )
    except Exception as e:
        return InstanceResult(
            name=Path(instance_path).name,
            status="ERROR",
            conflicts=0,
            decisions=0,
            iterations=0,
            learned_clauses=0,
            time_seconds=0,
            expected_sat=None,
            regression=False
        )

def parse_solver_output(output: str) -> Dict:
    """Parse solver statistics from verbose output"""
    stats = {}
    
    for line in output.split('\n'):
        if line.startswith('c '):
            # Parse statistics lines like "c Conflicts:     123"
            if 'Conflicts:' in line:
                stats["conflicts"] = int(line.split(':')[1].strip())
            elif 'Decisions:' in line:
                stats["decisions"] = int(line.split(':')[1].strip())
            elif 'Iterations:' in line:
                stats["iterations"] = int(line.split(':')[1].strip())
            elif 'Learned:' in line:
                stats["learned"] = int(line.split(':')[1].strip())
    
    return stats

def check_regression(result: InstanceResult, baseline: Optional[Baseline]) -> bool:
    """Check if current performance represents a regression from baseline"""
    if baseline is None:
        return False
    
    # Allow 20% degradation threshold
    threshold = 1.2
    
    regression_details = {}
    
    if result.conflicts > baseline.conflicts * threshold:
        regression_details["conflicts"] = {
            "current": result.conflicts,
            "baseline": baseline.conflicts,
            "ratio": result.conflicts / baseline.conflicts
        }
    
    if result.decisions > baseline.decisions * threshold:
        regression_details["decisions"] = {
            "current": result.decisions,
            "baseline": baseline.decisions,
            "ratio": result.decisions / baseline.decisions
        }
    
    if result.time_seconds > baseline.time_seconds * threshold:
        regression_details["time"] = {
            "current": result.time_seconds,
            "baseline": baseline.time_seconds,
            "ratio": result.time_seconds / baseline.time_seconds
        }
    
    return len(regression_details) > 0

def update_baseline(result: InstanceResult) -> Baseline:
    """Create or update baseline from current result"""
    return Baseline(
        name=result.name,
        conflicts=result.conflicts,
        decisions=result.decisions,
        time_seconds=result.time_seconds
    )

def main():
    import argparse
    
    parser = argparse.ArgumentParser(description='Performance regression benchmark runner')
    parser.add_argument('--baseline', action='store_true',
                       help='Update baseline values with current performance')
    parser.add_argument('--json', action='store_true',
                       help='Output results in JSON format')
    parser.add_argument('--timeout', type=int, default=60,
                       help='Timeout per instance in seconds')
    parser.add_argument('--instances-dir', type=str, 
                       default=str(Path(__file__).parent / "gbd_instances"),
                       help='Directory containing benchmark instances')
    
    args = parser.parse_args()
    
    instances_dir = Path(args.instances_dir)
    results: List[InstanceResult] = []
    
    print(f"Running performance regression tests...")
    print(f"Instances directory: {instances_dir}")
    print(f"Timeout: {args.timeout}s per instance")
    print()
    
    for instance_name in TEST_INSTANCES:
        instance_path = instances_dir / instance_name
        
        if not instance_path.exists():
            print(f"⊘ {instance_name}: NOT FOUND")
            continue
        
        print(f"Running {instance_name}...", end=" ", flush=True)
        result = run_solver(str(instance_path), timeout=args.timeout)
        
        # Check for regression
        baseline = BASELINES.get(instance_name)
        result.regression = check_regression(result, baseline)
        
        if args.baseline and result.status not in ["TIMEOUT", "ERROR"]:
            # Update baseline
            new_baseline = update_baseline(result)
            BASELINES[instance_name] = new_baseline
            print(f"✓ {result.status} (baseline updated)")
        else:
            if result.status == "TIMEOUT":
                print(f"⊘ TIMEOUT")
            elif result.status == "ERROR":
                print(f"✗ ERROR")
            elif result.regression:
                print(f"✗ REGRESSION")
            else:
                print(f"✓ {result.status}")
        
        results.append(result)
    
    # Output results
    print()
    print("=" * 60)
    
    if args.json:
        output = {
            "results": [asdict(r) for r in results],
            "baselines": {k: asdict(v) for k, v in BASELINES.items()}
        }
        print(json.dumps(output, indent=2))
    else:
        # Summary
        total = len(results)
        solved = sum(1 for r in results if r.status in ["SAT", "UNSAT"])
        timeouts = sum(1 for r in results if r.status == "TIMEOUT")
        errors = sum(1 for r in results if r.status == "ERROR")
        regressions = sum(1 for r in results if r.regression)
        
        print(f"Summary:")
        print(f"  Total instances: {total}")
        print(f"  Solved: {solved}")
        print(f"  Timeouts: {timeouts}")
        print(f"  Errors: {errors}")
        print(f"  Regressions: {regressions}")
        
        if regressions > 0:
            print()
            print("Regressions detected:")
            for r in results:
                if r.regression:
                    print(f"  - {r.name}")
                    if r.regression_details:
                        for metric, details in r.regression_details.items():
                            print(f"      {metric}: {details['current']} (+{details['ratio']:.1f}× from baseline)")
    
    # Save baselines if requested
    if args.baseline:
        baseline_file = Path(__file__).parent / "baselines.json"
        with open(baseline_file, 'w') as f:
            json.dump({k: asdict(v) for k, v in BASELINES.items()}, f, indent=2)
        print(f"\nBaselines saved to {baseline_file}")
    
    # Exit with error if regressions detected
    if any(r.regression for r in results):
        sys.exit(1)

if __name__ == "__main__":
    main()
