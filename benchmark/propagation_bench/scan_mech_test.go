// Package propagation_bench: scan_mech_test.go measures the per-entry CPU cost
// of the two literal-watch-list scan mechanisms — swap-with-last deletion
// (satiance's current propagateWatched) vs write-pointer compaction (minisat's
// canonical form) — on IDENTICAL synthetic watch lists and move patterns.
//
// This isolates the mechanical per-entry difference that end-to-end props/sec
// cannot (because compaction is not bit-identical to swap-with-last, so their
// trajectories diverge). If compaction's per-entry loop is NOT faster at the
// relevant move fractions, there is no mechanical headroom to retune for.
//
// Run:  go test -bench=. -benchtime=1s ./benchmark/propagation_bench
package propagation_bench

import (
	"testing"
)

// scanSize: entries per synthesis. Move fraction is swept.
const scanSize = 4000000

// moveFraction variants (fraction of entries that "move", i.e. are deleted).
var moveFractions = []float64{0.0, 0.1, 0.25, 0.5, 0.8}

var scanSink uint64

// swapWithLast settles a literal's watch list using swap-with-last deletion
// (mirrors the current propagateWatched move block, including the readIdx
// re-examination of the pulled tail entry). move[i] marks deletions. All
// non-deleted entries survive; survivors are in w[:wLen] with swap-delete's
// (order-perturbing) layout.
func swapWithLast(w []Watch, move []bool) {
	wlLen := len(w)
	readIdx := 0
	for readIdx < wlLen {
		if move[readIdx] {
			lastIdx := wlLen - 1
			if readIdx != lastIdx {
				w[readIdx] = w[lastIdx]
			}
			wlLen = lastIdx
			// swap-with-last re-visits the pulled tail via readIdx-- (not modeled
			// as a distinct iteration here, but the copy cost is included above).
			continue
		}
		readIdx++
	}
	scanSink += uint64(wlLen)
}

// compaction settles a literal's watch list using write-pointer compaction:
// survivors are copied forward to wIdx; moved entries are skipped; truncated.
func compaction(w []Watch, move []bool) {
	wlLen := len(w)
	wIdx := 0
	for readIdx := 0; readIdx < wlLen; readIdx++ {
		if move[readIdx] {
			continue // dropped
		}
		w[wIdx] = w[readIdx]
		wIdx++
	}
	scanSink += uint64(wIdx)
}

func benchScan(b *testing.B, frac float64, fn func([]Watch, []bool)) {
	w := make([]Watch, scanSize)
	move := make([]bool, scanSize)
	nf := int(frac * float64(scanSize))
	for i := 0; i < scanSize; i++ {
		w[i] = Watch{ClauseIdx: int32(i), Blit: uint32(i)}
		move[i] = i < nf // nip off the first `nf` entries as movers
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fn(w, move)
	}
}

func BenchmarkScan_SwapWithLast_Frac0(b *testing.B)  { benchScan(b, 0.0, swapWithLast) }
func BenchmarkScan_SwapWithLast_Frac10(b *testing.B) { benchScan(b, 0.1, swapWithLast) }
func BenchmarkScan_SwapWithLast_Frac25(b *testing.B) { benchScan(b, 0.25, swapWithLast) }
func BenchmarkScan_SwapWithLast_Frac50(b *testing.B) { benchScan(b, 0.5, swapWithLast) }
func BenchmarkScan_SwapWithLast_Frac80(b *testing.B) { benchScan(b, 0.8, swapWithLast) }

func BenchmarkScan_Compaction_Frac0(b *testing.B)  { benchScan(b, 0.0, compaction) }
func BenchmarkScan_Compaction_Frac10(b *testing.B) { benchScan(b, 0.1, compaction) }
func BenchmarkScan_Compaction_Frac25(b *testing.B) { benchScan(b, 0.25, compaction) }
func BenchmarkScan_Compaction_Frac50(b *testing.B) { benchScan(b, 0.5, compaction) }
func BenchmarkScan_Compaction_Frac80(b *testing.B) { benchScan(b, 0.8, compaction) }
