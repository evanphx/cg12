// The frame slot a call homes its aggregate result into is not a GC root at
// that call, because the call is what defines it.
//
// arm64/goabi.go's lowerGoAggregateResult reserves the home with an OAlloc
// emitted before the call and fills it with stores emitted after the call. The
// home's address is therefore live across the call -- its only uses are those
// stores -- so computeSafepointRoots used to report the allocation's pointer
// words at the one safepoint where they cannot yet hold the result.
//
// Straight-line code survives that: the prologue zeroes every pointer-bearing
// allocation word, so an unwritten home reads as nil. A loop does not. The
// OAlloc names a fresh local on each iteration and nothing re-zeroes the slot,
// so on iteration n the home still holds iteration n-1's pointer while the call
// is running. A collection that lands there follows a pointer the program
// abandoned; once its span has been released, findObject throws
//
//	runtime: pointer 0x... to unallocated span ... span.state=0
//	fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)
//
// reported inside runtime_scanframeworker, not a write barrier. See
// RUNTIME_PLAN.md section 26's residue.
//
// The program makes that deterministic rather than probable:
//
//   - buildText collects *before* it allocates anything, so the caller is parked
//     at exactly the safepoint after its call while the previous round's string
//     is already swept and its span returned to the page allocator;
//   - the result is never bound to a named local, so the home is the only place
//     the pointer survives the round, and the collection at the end of the round
//     is free to reclaim it;
//   - each round's string is a megabyte larger than the last, so it needs its
//     own span and cannot be handed back the region the previous one left;
//   - the call goes through a variable, so it stays a call.
//
// On the compiler that reported the home, this dies on every run at every
// GOMAXPROCS and at either GOGC.
package main

import "runtime"

var source = make([]byte, 8<<20)

var keep int

var freshText = buildText

func buildText(round int) string {
	runtime.GC()
	return string(source[:(round+1)<<20])
}

func loop(rounds int) {
	for round := 0; round < rounds; round++ {
		keep += len(freshText(round))
		runtime.GC()
	}
}

func main() {
	loop(8)
	if keep != 36<<20 {
		println("kept", keep)
		panic("the loop did not build every string")
	}
	println("stale result home ok")
}
