// Package propagation_bench clause-layout microbenchmark.
//
// Falsification experiment for the general-slow-path (non-binary replacement
// scan) per-entry structural hypothesis. Compares the memory-access cost of the
// replacement scan under two original-clause layouts on IDENTICAL data:
//
//   SoA (current satience): loc[clauseID] (Offset,Size) + literalPool[off:off+sz]
//                           slice build + blitPos re-read + optional search hint
//   AoS (minisat-style)   : clauses[clauseID].Lits[j] inline with the metadata
//
// If SoA and AoS cost the same per scan, the per-entry layout delta is NOT the
// source of the ~2.9x per-prop gap, and the full AoS migration is not worth its
// FIFM risk (three-strike falsification, cheaply). If AoS is meaningfully faster
// on the same scattered access pattern, the migration is justified.
//
// Run:  GOAMD64=v3 go test -bench=. -benchtime=1s ./benchmark/propagation_bench
package propagation_bench

import (
	"math/rand"
	"testing"
)

const (
	layoutNumVars    = 1000000
	layoutNumClauses = 1_000_000
)

// Loc mirrors cnf.ClauseLoc.
type layoutLoc struct {
	Off  int32
	Size int32
}

// searchHint mirrors the per-clause hint array.
var layoutHint = make([]int32, layoutNumClauses)

// SoA backing store.
var (
	layoutLocs  = make([]layoutLoc, layoutNumClauses)
	layoutPool  = make([]uint32, layoutNumClauses*4) // size<=4 inline region
	layoutLevel = make([]int32, layoutNumVars)
	layoutVal   = make([]int8, layoutNumVars) // 1 pos, 0 unassigned, -1 neg
)

// AoS backing store (metadata + inline literals colocated).
type layoutClause struct {
	Size  int16
	Lits  [4]uint32
}

var layoutClauses = make([]layoutClause, layoutNumClauses)

// fireOrder precomputes a scattered clause-visit order (like the watch list
// traversal) so both layouts see the identical access pattern.
var fireOrder = func() []uint32 {
	o := make([]uint32, layoutNumClauses)
	r := rand.New(rand.NewSource(7))
	for i := 0; i < layoutNumClauses; i++ {
		o[i] = uint32(r.Intn(layoutNumClauses))
	}
	return o
}()

func initLayout() {
	for i := 0; i < layoutNumClauses; i++ {
		// size-4 clauses, contiguous literal region
		layoutLocs[i] = layoutLoc{Off: int32(i * 4), Size: 4}
		for j := 0; j < 4; j++ {
			lit := (rndTab[(i*4+j)%len(rndTab)] % uint32(layoutNumVars)) << 1 // var<<1 | neg 0
			lit |= (rndTab[(i*4+j+1)%len(rndTab)] & 1)                        // neg bit
			layoutPool[i*4+j] = lit
		}
		layoutClauses[i] = layoutClause{Size: 4}
		copy(layoutClauses[i].Lits[:], layoutPool[i*4:i*4+4])
		layoutHint[i] = int32(2 + (i % 3))
	}
	for v := 0; v < layoutNumVars; v++ {
		layoutLevel[v] = int32((v % 5) - 1) // some unassigned, some at levels
		layoutVal[v] = int8((v%3)*2 - 1)    // -1 / 1 pattern with 0 overlaps
	}
}

// rndTab is a small literal-pattern table.
var rndTab = func() []uint32 {
	r := rand.New(rand.NewSource(3))
	t := make([]uint32, 4096)
	for i := range t {
		t[i] = r.Uint32()
	}
	return t
}()

var layoutSink uint64

// scanSoA mirrors the current slow-path scan (loc->slice build + hint read +
// positions 2..size-1 + assignment read) for a size-4 clause.
func scanSoA(id uint32) {
	loc := layoutLocs[id]
	cl := layoutPool[loc.Off : loc.Off+loc.Size]
	h := layoutHint[id]
	_ = cl[uint32(h)&3]
	var acc uint64
	for j := 2; j < 4; j++ {
		l := cl[j]
		v := int(l >> 1)
		if layoutLevel[v] >= 0 {
			_ = layoutVal[v]
		}
		acc += uint64(l)
	}
	layoutSink += acc
}

// scanAoS mirrors the AoS slow-path scan (inline literals, no hint).
func scanAoS(id uint32) {
	c := layoutClauses[id]
	var acc uint64
	for j := 2; j < int(c.Size); j++ {
		l := c.Lits[j]
		v := int(l >> 1)
		if layoutLevel[v] >= 0 {
			_ = layoutVal[v]
		}
		acc += uint64(l)
	}
	layoutSink += acc
}

func BenchmarkLayoutSoA(b *testing.B) {
	initLayout()
	fo := fireOrder
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range fo {
			scanSoA(id)
		}
	}
}

func BenchmarkLayoutAoS(b *testing.B) {
	initLayout()
	fo := fireOrder
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range fo {
			scanAoS(id)
		}
	}
}
