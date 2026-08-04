// A slice expression that consumes all of its source -- b[len(b):], or the
// s[n:] in compress/flate's `f.toRead = f.toRead[n:]` -- has to keep its data
// pointer inside the source's allocation.
//
// The obvious lowering, ptr + low*elemsize, puts it one byte *past* the end. The
// collector will not accept that pointer: findObject looks the address up in the
// page it lands on, which belongs to the next allocation, and either finds a span
// the address is outside of or an unallocated one, and throws "found bad pointer
// in Go heap". Worse first: a one-past pointer refers to nothing, so it does not
// retain the buffer it came from, and the buffer is collected out from under a
// slice that is still reachable.
//
// The host toolchain masks the offset to zero when the result has no capacity
// left, so the pointer falls back to the start of the source's storage -- a
// pointer the collector accepts and one that retains the source. See
// cmd/compile/internal/ssagen/ssa.go's slice(): "the masking is to make sure
// that we don't generate a slice that points to the next object in memory".
//
// This program is the reducer. Part one reads the generated pointer directly and
// is deterministic: it fails on every run of a compiler that does not mask.
// Part two is the collector consequence -- tails of buffers that nothing else
// refers to, kept across several cycles -- which is what goc-built
// compress/flate was dying of, in the decompressor's toRead field, at a rate the
// performance suite had to build retry logic around.
package main

import (
	"runtime"
	"unsafe"
)

// sliceHeader is the layout the collector reads, which is the thing under test.
type sliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

type stringHeader struct {
	data unsafe.Pointer
	len  int
}

func sliceData(b []byte) uintptr {
	return uintptr((*sliceHeader)(unsafe.Pointer(&b)).data)
}

func stringData(s string) uintptr {
	return uintptr((*stringHeader)(unsafe.Pointer(&s)).data)
}

// tail, tailFull and stringTail are not inlined so that the offset stays a
// runtime value; a constant one would let any constant folder decide the
// question the collector actually asks.
//
//go:noinline
func tail(b []byte, low int) []byte {
	return b[low:]
}

//go:noinline
func tailFull(b []byte, low, high, max int) []byte {
	return b[low:high:max]
}

//go:noinline
func stringTail(s string, low int) string {
	return s[low:]
}

// checkPointers is part one: every one of these results has no storage left, so
// every one of them must still point at the start of the source's storage.
func checkPointers() {
	const size = 1 << 15

	buffer := make([]byte, size)
	base := sliceData(buffer)

	if got := sliceData(tail(buffer, len(buffer))); got != base {
		println("b[len(b):] is", int64(got-base), "bytes past the start of a", size, "byte buffer")
		panic("a slice with no capacity left points outside its allocation")
	}
	if got := sliceData(tailFull(buffer, 4096, 4096, 4096)); got != base {
		println("b[4096:4096:4096] is", int64(got-base), "bytes past the start")
		panic("a three-index slice with no capacity left points outside its allocation")
	}

	// A slice that still has capacity keeps its offset: the mask must not
	// collapse a pointer that is legitimately interior.
	if got := sliceData(tail(buffer, 4096)); got != base+4096 {
		println("b[4096:] is", int64(got-base), "bytes past the start, want 4096")
		panic("a slice with capacity left lost its offset")
	}

	// A string has no capacity, so its length is the only evidence of whether
	// any of the source's bytes are still referred to.
	text := string(make([]byte, 64))
	textBase := stringData(text)
	if got := stringData(stringTail(text, len(text))); got != textBase {
		println("s[len(s):] is", int64(got-textBase), "bytes past the start of a 64 byte string")
		panic("an empty string slice points outside its allocation")
	}
	if got := stringData(stringTail(text, 32)); got != textBase+32 {
		println("s[32:] is", int64(got-textBase), "bytes past the start, want 32")
		panic("a non-empty string slice lost its offset")
	}
}

// retained holds tails and nothing else. If a tail does not refer to the buffer
// it came from, the buffer is unreachable the moment this loop drops it.
var retained [][]byte

// scenery is heap the collector has to walk past, so the freed buffers' pages
// are reused rather than sitting untouched.
var scenery []*[512]byte

// checkCollector is part two. Each round makes a large buffer, keeps only its
// empty tail, and lets the buffer go; the tail is the only thing that can retain
// it. A collector that is handed a one-past-the-end pointer here throws on the
// scan, and a buffer that is freed underneath a live tail leaves that tail
// pointing into a reused span.
func checkCollector() {
	const rounds = 48
	const size = 1 << 15

	for round := 0; round < rounds; round++ {
		buffer := make([]byte, size)
		for i := 0; i < size; i += 4096 {
			buffer[i] = byte(round)
		}
		retained = append(retained, tail(buffer, len(buffer)))

		for i := 0; i < 24; i++ {
			scenery = append(scenery, new([512]byte))
		}
		if len(scenery) > 512 {
			scenery = scenery[len(scenery)-512:]
		}
		if round%8 == 7 {
			runtime.GC()
		}
	}

	for cycle := 0; cycle < 4; cycle++ {
		runtime.GC()
	}

	for index, kept := range retained {
		if len(kept) != 0 || cap(kept) != 0 {
			println("retained tail", index, "has len", len(kept), "cap", cap(kept))
			panic("a tail slice changed shape")
		}
	}
}

func main() {
	checkPointers()
	checkCollector()
	println("slice tail pointer ok")
}
