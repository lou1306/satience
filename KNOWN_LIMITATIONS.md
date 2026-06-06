# Satience: Known Limitations and Performance Facts

**CRITICAL: Read this before making performance claims!**

---

## 🚨 MOST IMPORTANT: PHP is NOT a CDCL Limitation

### The Fact
**MiniSat solves PHP (pigeonhole principle) instantly.** MiniSat is a CDCL solver with 1-UIP clause learning, just like Satience.

### The Implication
If MiniSat solves PHP in 0.001s and Satience timeouts at 60s, this is **NOT** a "fundamental CDCL limitation" or "theoretical limitation of 1-UIP learning".

**This is an IMPLEMENTATION GAP in our preprocessing.**

### The Evidence
| Metric | MiniSat | Satience | Gap |
|--------|---------|----------|-----|
| Variables after preprocessing | 9 (eliminated 21/30 = 70%) | 24 (eliminated 6/30 = 20%) | **3.5x weaker** |
| Conflicts to solve | 251 | 200,000+ (timeout) | **800x more** |
| Total time | 0.001s | 60s+ (timeout) | **60,000x slower** |

### What MiniSat Has That We Don't
1. **Cardinality constraint detection** - PHP encodes "at most k of n" constraints
2. **Symmetry breaking** - PHP has n! symmetries that can be exploited
3. **More aggressive preprocessing** - Our time/degree limits too conservative
4. **Better subsumption** - Uses occurrence lists, we use O(n²) checking

### What To Say
❌ **WRONG:** "PHP is exponentially hard for CDCL with 1-UIP learning"

✅ **CORRECT:** "PHP requires cardinality reasoning and symmetry breaking. MiniSat has these techniques; we don't yet. This is a preprocessing gap, not a CDCL limitation."

---

## 📊 Actual Performance Status (June 5, 2026)

### Where We're Competitive (≤2x MiniSat)
| Instance Type | Slowdown | Status |
|--------------|----------|--------|
| Tseitin Grid | 0.9-1.9x | ✅ Excellent (sometimes FASTER!) |
| XOR/Algebra | 1.5-2.1x | ✅ Good |
| Chain | 1.6-1.7x | ✅ Good |
| **Median (excluding PHP)** | **1.72x** | ✅ **PRODUCTION READY** |

### Where We Struggle (1000x+ slower)
| Instance Type | Slowdown | Root Cause |
|--------------|----------|------------|
| PHP (pigeonhole) | 60,000x+ | Missing cardinality detection |
| Dense random | Timeout | Propagation bottleneck (watched literals working, too many clauses) |

### Soundness
✅ **100% verified** - All solved instances match MiniSat's results
✅ **No regressions** - All previously solvable instances still work
✅ **Models verified** - SAT assignments satisfy all clauses

---

## 🎯 Performance Trend

| Date | Median Slowdown | Improvement |
|------|----------------|-------------|
| Initial (AGENTS.md) | 5.39x | baseline |
| Current (June 5) | **1.72x** | **3.1x FASTER** |

**We've improved 3.1x through:**
- Watched literals for all clause types
- Failed literal elimination (re-enabled with safeguards)
- Variable elimination (with strict bounds)
- Adaptive restarts (LBD-based)
- Backjumping
- Preprocessing pipeline (subsumption, hyper-binary, etc.)

---

## 🛠️ How to Fix PHP (In Order of Impact)

### High Priority (1-2 days each)
1. **Cardinality detection** - Detect "at most k of n" patterns in binary clauses
2. **Symmetry breaking** - Add lex-leader constraints for variable symmetries
3. **Stronger preprocessing limits** - Allow 2s/200 degree for instances <50 vars

### Medium Priority (3-5 days each)
4. **Occurrence lists** - O(n) subsumption instead of O(n²)
5. **Inprocessing** - Apply preprocessing during search (every 500 conflicts)
6. **Better equivalence detection** - Fix and re-enable (currently disabled)

### Low Priority (weeks-months)
7. **Extended resolution** - Research-level, can prove PHP efficiently
8. **Gaussian elimination** - For XOR-heavy instances
9. **Portfolio solving** - Multiple configurations in parallel

---

## 📝 What To Tell Users

### For Practical SAT Solving
✅ "Satience is production-ready with 1.72x median slowdown vs MiniSat"
✅ "Excellent on structured instances: Tseitin, XOR, chain constraints"
✅ "100% sound - all results verified"

### For PHP/Cardinality Instances
⚠️ "PHP instances require cardinality reasoning which we're implementing"
⚠️ "For PHP specifically, use MiniSat or CaDiCaL until we add cardinality detection"
⚠️ "This is a known gap - MiniSat solves PHP via preprocessing, we're adding similar techniques"

### For Competition Benchmarking
⚠️ "Competitive on most instance types (1.72x median)"
⚠️ "Not ready for competition due to PHP/cardinality gap"
⚠️ "Focus on practical instances, not theoretical benchmarks"

---

## 🔍 How to Verify Performance Claims

### Before Claiming "X is hard for CDCL"
1. **Check if MiniSat solves it** - If yes, it's NOT a CDCL limitation
2. **Compare preprocessing** - How many variables/clauses eliminated?
3. **Check conflict count** - Are we learning useful clauses?
4. **Profile the code** - Where is time actually spent?

### Before Claiming "No Regression"
1. **Run full benchmark suite** - Not just unit tests
2. **Compare to documented baseline** - AGENTS.md has 5.39x median
3. **Verify soundness** - All results match reference solver
4. **Check multiple instance types** - Not just one family

---

## 📚 Reference Files

- `benchmark/PERFORMANCE_COMPARISON_JUNE5.md` - Full benchmark results
- `benchmark/PHP_ANALYSIS_UPDATED.md` - Detailed PHP analysis
- `benchmark/PREPROCESSING_IMPROVEMENTS.md` - Preprocessing changes
- `AGENTS.md` - Project documentation and baseline metrics

---

## ✅ Checklist for Future Performance Analysis

- [ ] Did I check if MiniSat solves the instance?
- [ ] Did I compare preprocessing effectiveness (vars eliminated)?
- [ ] Did I verify soundness (results match reference)?
- [ ] Did I run full benchmark suite (not just one instance)?
- [ ] Did I check conflict counts (not just time)?
- [ ] Did I profile to find actual bottleneck?
- [ ] Am I blaming "CDCL limitations" when it's actually our implementation?

---

**Last Updated:** June 5, 2026  
**Commit:** 330d112 (Failed literal re-enabled)  
**Median Slowdown:** 1.72x (excluding PHP)  
**Status:** Production Ready ✅
