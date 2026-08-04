// `append(s, make([]T, n)...)`, the idiom that extends a slice by n zero
// elements.
//
// The standard library relies on it costing one allocation. slices.Grow is
//
//	s = append(s[:cap(s)], make([]E, n)...)[:len(s)]
//
// with the comment "This expression allocates only once (see test)". Compiled
// literally it allocates twice, and every element copied out of the fresh slice
// is zero; goc now extends the destination and clears the new region instead.
// See goc.appendedMakeLength.
//
// This program is about the parts of that rewrite that are not the allocation
// count, each of which is a way to get it wrong:
//
//   - the new region must be zero even when the growing branch was not taken,
//     because then it is a reused backing array whose tail holds whatever a
//     previous, longer life of the slice left there;
//   - a pointer-bearing element type must be cleared through the write barrier,
//     because zeroing a pointer word is a deletion the collector has to see;
//   - a negative length must still panic, and must not reach the clear with a
//     byte count that is negative signed and enormous unsigned.
//
// It is deterministic under the host toolchain, which is the reference for every
// answer it checks.
package main

import (
	"runtime"
	"slices"
)

type gcbox struct{ n int }

var sink []*gcbox
var n = 3

func main() {
	// 1. extend an empty slice
	var s []int
	s = append(s[:cap(s)], make([]int, n)...)[:len(s)]
	if len(s) != 0 || cap(s) < 3 {
		panic("grow shape wrong")
	}
	s = s[:3]
	if s[0] != 0 || s[1] != 0 || s[2] != 0 {
		panic("extension not zeroed")
	}

	// 2. extend a slice with existing elements, reusing dirty capacity
	base := make([]int, 0, 8)
	base = append(base, 1, 2, 3, 4, 5, 6, 7, 8)
	base = base[:2]
	base = append(base, make([]int, 3)...)
	if len(base) != 5 {
		panic("wrong length after extension")
	}
	if base[0] != 1 || base[1] != 2 {
		panic("existing elements lost")
	}
	if base[2] != 0 || base[3] != 0 || base[4] != 0 {
		panic("reused capacity not cleared")
	}

	// 3. pointer elements, under the collector
	boxes := make([]*gcbox, 0, 4)
	boxes = append(boxes, &gcbox{1}, &gcbox{2}, &gcbox{3}, &gcbox{4})
	boxes = boxes[:1]
	boxes = append(boxes, make([]*gcbox, 3)...)
	for attempt := 0; attempt < 16; attempt++ {
		runtime.GC()
	}
	if boxes[0].n != 1 {
		panic("retained pointer element corrupted")
	}
	for _, box := range boxes[1:] {
		if box != nil {
			panic("pointer extension not cleared")
		}
	}
	sink = boxes

	// 4. slices.Grow, which is the caller this exists for
	grown := slices.Grow([]int{1, 2}, 5)
	if len(grown) != 2 || cap(grown) < 7 || grown[0] != 1 || grown[1] != 2 {
		panic("slices.Grow wrong")
	}

	// 5. a negative length still panics
	defer func() {
		if recover() == nil {
			panic("negative extension did not panic")
		}
		println("append make extension ok")
	}()
	negative := -1
	s = append(s, make([]int, negative)...)
	panic("unreachable")
}
