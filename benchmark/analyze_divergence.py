#!/usr/bin/env python3
"""Analyze divergence_unsat.tsv: separate search coverage (conflict ratio) from
per-step overhead (time/conflict) and stratify by verdict (UNSAT vs SAT).

TSV columns:
label fam s_res m_res s_dur m_dur s_conf m_conf s_dec m_dec s_prop m_prop
"""
import sys, math, statistics

def col(row, i):
    v = row[i]
    try:
        return float(v)
    except ValueError:
        return None

rows = []
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    p = line.split("|")
    if len(p) < 12:
        continue
    if p[2] == "GENFAIL":
        continue
    rows.append(p)

def geo(xs):
    xs = [x for x in xs if x > 0]
    if not xs:
        return float("nan")
    return math.exp(sum(math.log(x) for x in xs) / len(xs))

def analyze(bucket, label):
    print(f"\n{'='*70}\n{label} (n={len(bucket)})\n{'='*70}")
    print(f"{'label':<10} {'fam':<9} {'verd':<5} {'tRatio':>7} {'cRatio':>7} {'dRatio':>7} {'sat t/c':>8} {'mini t/c':>8} {'t/c r':>6}")
    tr = []; cr = []; dr = []; tpc_ratio = []
    for r in bucket:
        sat_dur, mini_dur = col(r,4), col(r,5)
        sat_conf, mini_conf = col(r,6), col(r,7)
        sat_dec, mini_dec = col(r,8), col(r,9)
        if sat_dur is None or mini_dur is None or mini_dur == 0:
            continue
        if sat_dur >= 15 or mini_dur >= 15:
            print(f"{r[0]:<10} {r[1]:<9} {r[3]:<5} {'TMO':>7}")
            continue
        tr_ = sat_dur/mini_dur
        cr_ = sat_conf/mini_conf if mini_conf and sat_conf else float('nan')
        dr_ = sat_dec/mini_dec if mini_dec and sat_dec else float('nan')
        sat_tpc = sat_dur/sat_conf if sat_conf else float('nan')
        mini_tpc = mini_dur/mini_conf if mini_conf else float('nan')
        tpc_r = sat_tpc/mini_tpc if sat_tpc and mini_tpc and mini_tpc>0 else float('nan')
        tr.append(tr_); cr.append(cr_); dr.append(dr_); tpc_ratio.append(tpc_r)
        def f(x): return f"{x:.1f}" if x==x else "  - "
        print(f"{r[0]:<10} {r[1]:<9} {r[3]:<5} {f(tr_):>7} {f(cr_):>7} {f(dr_):>7} "
              f"{f(sat_tpc):>8} {f(mini_tpc):>8} {f(tpc_r):>6}")
    print("-"*70)
    md = lambda x: statistics.median(x) if x else float('nan')
    gd = lambda x: geo(x)
    print(f"time-ratio:   median {md(tr):.2f}  geomean {gd(tr):.2f}")
    print(f"conflict-ratio(SEARCH COVERAGE): median {md(cr):.2f}  geomean {gd(cr):.2f}")
    print(f"decision-ratio: median {md(dr):.2f}  geomean {gd(dr):.2f}")
    print(f"time/conflict-ratio(PER-STEP COST): median {md(tpc_ratio):.2f}  geomean {gd(tpc_ratio):.2f}")
    print(f"  -> note: conflict-ratio>>1 = weak coverage (more conflicts); "
          f"t/c-ratio>>1 = slow propagation (same conflicts, more cost)")

unsat = [r for r in rows if r[3] == "UNSAT" and r[2] == "UNSAT"]
sat   = [r for r in rows if r[3] == "SAT"   and r[2] == "SAT"]

analyze(unsat, "UNSAT instances (both agree UNSAT)")
analyze(sat, "SAT instances (both agree SAT)")

# Overall timeouts
print("\n" + "="*70 + "\nTIMEOUTS")
for r in rows:
    if "TMO" in (r[2], r[3]):
        print(f"{r[0]:<10} sat={r[2]} mini={r[3]}")
