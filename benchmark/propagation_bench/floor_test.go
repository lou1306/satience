// Package propagation_bench measures the raw floor throughput of satience's
// propagation inner loop against an equivalent C++/minisat loop on IDENTICAL
// flat arrays, with no parse/init/GC in the timed region.
//
// This is the R-1 go/no-go gate for the propagation-representation rewrite:
// if satience's minimal per-watch loop is already >>2x slower than the C++
// equivalent, the rewrite (narrow flat arrays + unsafe hot loop) cannot close
// the ~3x per-prop gap and is not worth pursuing.
//
// Run:  go test -bench=. -benchtime=1.5s ./benchmark/propagation_bench
// The C++ counterpart is floor.cpp (compiled with -O2), which reports ns/watch
// for the identically-shaped loop. A single runner script compares them.
package propagation_bench

import (
	"math/rand"
	"testing"
	"unsafe"
)

const (
	numVars      = 2000000
	totalWatches = 8000000
)

// Watch mirrors cnf.Watch (8 bytes): packing the clause index and blocking lit.
type Watch struct {
	ClauseIdx int32
	Blit      uint32
}

var (
	// flatWatch is the fully contiguous arena (the R-2 representation floor).
	flatWatch = make([]Watch, totalWatches)
	// value indexes by Blit (varIdx*2+negated); even index => true.
	value = make([]byte, numVars*2)

	// perLit is the current slice-of-slice access pattern.
	perLit = func() [][]Watch {
		pl := make([][]Watch, numVars*2)
		for _, w := range flatWatch {
			idx := int(w.Blit)
			pl[idx] = append(pl[idx], w)
		}
		return pl
	}()

	// sink prevents the compiler from eliding the loops (global escape).
	sink uint64
)

func init() {
	r := rand.New(rand.NewSource(1))
	for i := range flatWatch {
		v := r.Intn(numVars)
		flatWatch[i] = Watch{ClauseIdx: int32(i), Blit: uint32(v*2 + r.Intn(2))}
	}
	for i := 0; i < len(value); i += 2 {
		value[i] = 1 // ~half the blit targets are "true" (realistic fast-path mix)
	}
}

// BenchmarkGoFlatUnsafe is the R-2 target shape: flat arena, unsafe snapshot
// length, unsafe base for the element loads, byte value lookup by Blit.
func BenchmarkGoFlatUnsafe(b *testing.B) {
	watches := flatWatch
	val := value
	base := unsafe.Pointer(&watches[0])
	valBase := unsafe.Pointer(&val[0])
	ws := unsafe.Sizeof(Watch{})
	n := len(watches)
	acc := sink
	b.SetBytes(int64(n) * int64(ws))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for readIdx := 0; readIdx < n; readIdx++ {
			w := *(*Watch)(unsafe.Add(base, uintptr(readIdx)*ws))
			if *(*byte)(unsafe.Add(valBase, w.Blit)) != 0 {
				continue
			}
			acc += uint64(w.ClauseIdx)
		}
	}
	sink = acc
}

// BenchmarkGoFlatChecked is the same flat arena but with Go bounds checking
// (quantifies how much unsafe saves on this loop).
func BenchmarkGoFlatChecked(b *testing.B) {
	watches := flatWatch
	val := value
	n := len(watches)
	acc := sink
	b.SetBytes(int64(n) * int64(unsafe.Sizeof(Watch{})))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < n; j++ {
			w := watches[j]
			if val[w.Blit] != 0 {
				continue
			}
			acc += uint64(w.ClauseIdx)
		}
	}
	sink = acc
}

// BenchmarkGoSliceOfSlices is the current representation (slice-of-slice,
// per-literal lists, bounds-checked) as a reference.
func BenchmarkGoSliceOfSlices(b *testing.B) {
	pl := perLit
	val := value
	acc := sink
	var total int
	for _, wl := range pl {
		total += len(wl)
	}
	b.SetBytes(int64(total))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, wl := range pl {
			for j := 0; j < len(wl); j++ {
				w := wl[j]
				if val[w.Blit] != 0 {
					continue
				}
				acc += uint64(w.ClauseIdx)
			}
		}
	}
	sink = acc
}
