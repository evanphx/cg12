// Covers the tiny allocation family: pointer-free objects smaller than
// runtime.maxTinySize, which the allocator combines several at a time into one
// 16-byte block.
//
// RUNTIME_PLAN.md section 6 asks for tiny allocations across the families in
// malloc_generated.go. That file has one tiny family per exact request size 1
// through 15 (mallocgcTinySize1 .. mallocgcTinySize15); those entry points are
// disabled in this build, so the live family is runtime.mallocgcTiny, but the
// per-size behaviour they specialize is still there and is what this program
// exercises: the alignment tinyStub applies to the offset within the block
// depends on the request size, and so does how many objects fit in a block.
//
// The asserted properties are the tiny allocator's actual contract:
//
//   - a request whose size is a multiple of 8, 4, or 2 is aligned to that
//     amount, so a tiny object with an eight-byte field is safe to access
//     atomically. Odd sizes carry no alignment guarantee and none is asserted.
//   - distinct tiny objects do not overlap, even though they share a block.
//   - many one-byte objects consume far fewer 16-byte blocks than there are
//     objects, which is the combining the family exists for.
//   - contents survive a collection, so a block is not freed while any object
//     in it is still reachable.
package main

import (
	"runtime"
	"unsafe"
)

// maxTinySize is runtime.maxTinySize. A pointer-free request below it uses the
// tiny allocator.
const maxTinySize = 16

// combiningAllocations is large enough that the block count cannot be confused
// with unrelated 16-byte allocations made by the runtime.
const combiningAllocations = 4096

// tinyBlockClass is the size class the tiny allocator draws its blocks from:
// runtime.tinySpanClass, the 16-byte class.
const tinyBlockClass = 2

var tinySink [][]byte

func allocateTiny(size int, tag byte) []byte {
	object := make([]byte, size)
	for index := range object {
		object[index] = tag + byte(index)
	}
	tinySink = append(tinySink, object)
	return object
}

func checkTiny(object []byte, size int, tag byte) {
	if len(object) != size {
		panic("tiny allocation has the wrong length")
	}
	for index := range object {
		if object[index] != tag+byte(index) {
			panic("tiny allocation lost its contents")
		}
	}
}

func requiredAlignment(size int) uintptr {
	if size&7 == 0 {
		return 8
	}
	if size&3 == 0 {
		return 4
	}
	if size&1 == 0 {
		return 2
	}
	return 1
}

func main() {
	// One pass over every tiny request size, checking alignment and contents.
	for size := 1; size < maxTinySize; size++ {
		for repetition := 0; repetition < 64; repetition++ {
			object := allocateTiny(size, byte(size*7+repetition))
			address := uintptr(unsafe.Pointer(&object[0]))
			if address%requiredAlignment(size) != 0 {
				println("tiny request size")
				println(size)
				panic("tiny allocation does not meet the alignment its size requires")
			}
			checkTiny(object, size, byte(size*7+repetition))
		}
	}

	// Distinct tiny objects share a block but must not overlap. Writing a
	// distinct pattern into every object allocated so far and reading them all
	// back afterwards detects any overlap.
	for index, object := range tinySink {
		for position := range object {
			object[position] = byte(index*13 + position*3 + 1)
		}
	}
	for index, object := range tinySink {
		for position := range object {
			if object[position] != byte(index*13+position*3+1) {
				panic("two tiny allocations overlap")
			}
		}
	}

	// Combining: allocating many one-byte objects must consume far fewer
	// 16-byte blocks than there are objects.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	oneByteObjects := make([][]byte, 0, combiningAllocations)
	for repetition := 0; repetition < combiningAllocations; repetition++ {
		object := make([]byte, 1)
		object[0] = byte(repetition)
		oneByteObjects = append(oneByteObjects, object)
	}
	runtime.ReadMemStats(&after)

	blocks := after.BySize[tinyBlockClass].Mallocs - before.BySize[tinyBlockClass].Mallocs
	if blocks >= combiningAllocations/2 {
		println("tiny blocks consumed")
		println(int(blocks))
		panic("the tiny allocator did not combine one-byte objects into shared blocks")
	}
	if after.Mallocs-before.Mallocs < combiningAllocations {
		panic("tiny allocations were not counted as allocations")
	}
	for index, object := range oneByteObjects {
		if len(object) != 1 || object[0] != byte(index) {
			panic("a one-byte tiny allocation lost its contents")
		}
	}

	// A block must stay alive while any object in it is reachable.
	runtime.GC()
	runtime.GC()
	for index, object := range tinySink {
		for position := range object {
			if object[position] != byte(index*13+position*3+1) {
				panic("a tiny allocation was freed while it was still reachable")
			}
		}
	}
	for index, object := range oneByteObjects {
		if object[0] != byte(index) {
			panic("a one-byte tiny allocation was freed while it was still reachable")
		}
	}
}
