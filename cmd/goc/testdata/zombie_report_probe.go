// A program that puts a genuine zombie object on a span with Green Tea inline
// mark bits, so that mspan.reportZombies has something real to name.
//
// A zombie is an object the sweeper finds marked in this cycle but free from
// the last one. This builds case 1 of reportZombies' own list: a pointer hidden
// in a uintptr across a collection and converted back afterwards.
//
// Every ingredient is load-bearing.
//
//   - block is 32 bytes and holds no pointers, so its span has elemsize 32.
//     gcUsesSpanInlineMarkBits is `heapBitsInSpan(size) && size >= 16`, so 32 is
//     inside the inline-mark-bit range (16..512) that reportZombies used to be
//     blind on. Pointer-free matters too: marking a freed object takes
//     tryDeferToSpanScan's noscan fast path, so the collector never reads the
//     dead object's contents and the run fails as a zombie report rather than
//     as "found bad pointer in Go heap".
//
//   - The 63 surviving objects keep the span in use. If the hidden object were
//     the only one on its span, the span would be freed to the heap after the
//     first collection and resurrecting the pointer would fault somewhere else.
//
//   - scribbleStack overwrites the frames allocateAndHideOne left behind, so no
//     stale stack slot keeps the hidden object alive through the first
//     collection.
//
//   - The payload words identify the object in the hexdump: the first word is
//     0x7a6f6d6269650000 plus the loop index, so the dump proves the report
//     named the object that was actually resurrected and not some neighbour.
//
// The program is expected to die with "fatal error: found pointer to free
// object". Printing the final line means the fault did not happen and the test
// learned nothing.
package main

import (
	"runtime"
	"unsafe"
)

type block [4]int64

var live []*block
var resurrected *block

//go:noinline
func allocateAndHideOne() uintptr {
	var hidden uintptr
	for i := 0; i < 64; i++ {
		b := new(block)
		b[0] = 0x7a6f6d6269650000 + int64(i)
		b[1] = 0x1111111111111111
		b[2] = 0x2222222222222222
		b[3] = 0x3333333333333333
		if i == 40 {
			hidden = uintptr(unsafe.Pointer(b))
			continue
		}
		live = append(live, b)
	}
	return hidden
}

//go:noinline
func scribbleStack(depth int) int {
	var pad [64]uintptr
	for i := range pad {
		pad[i] = uintptr(depth)
	}
	if depth == 0 {
		return int(pad[0])
	}
	return scribbleStack(depth-1) + int(pad[63])
}

func main() {
	hidden := allocateAndHideOne()
	scribbleStack(24)

	// Nothing references the hidden object, so this collection sweeps it free.
	runtime.GC()

	// Resurrect it through the uintptr. It is now a free object reachable from
	// a global root.
	resurrected = (*block)(unsafe.Pointer(hidden))

	// The next collection marks it, and the one after that sweeps its span,
	// finds a marked free object and calls reportZombies.
	runtime.GC()
	runtime.GC()
	runtime.GC()

	println("no zombie was reported; resurrected[0] =", resurrected[0])
}
