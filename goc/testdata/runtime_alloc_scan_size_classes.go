// Reaches every pointerful small size class served without a malloc header,
// and proves the pointer bitmap the allocator wrote for each one is correct.
//
// RUNTIME_PLAN.md section 6 asks for pointerful allocations across the
// size-class families. The live family here is
// runtime.mallocgcSmallScanNoHeader: a pointerful object of at most
// runtime.minSizeForMallocHeader (512) bytes carries no header and has its
// pointer/scalar bitmap written into the span's trailing heap-bits region by
// writeHeapBitsSmall. That bitmap is per-size-class code -- the bit offset,
// whether one or two words of the bitmap are written, and whether the object
// straddles a bitmap word all depend on the class -- so each class needs to be
// reached deliberately.
//
// Every element of the allocated object is a pointer, so the correct bitmap has
// every word set. The check is semantic rather than structural: each slot is
// given its own uniquely valued referent, every other reference to those
// referents is dropped, a collection is forced, and the values are read back. A
// bitmap bit that is missing for some word makes the collector treat that word
// as scalar, so the referent is unreachable, collected, and its memory reused;
// a bitmap bit set for the wrong word makes the collector follow a scalar. Both
// show up as a wrong value or a fault here.
package main

import (
	"runtime"
	"unsafe"
)

// allocationsPerClass has to be large enough that the counter delta is
// unambiguous and that a reused slot is likely to have been overwritten.
const allocationsPerClass = 48

// maxNoHeaderSize is runtime.minSizeForMallocHeader: goarch.PtrSize *
// goarch.PtrBits, that is 8 * 64 on a 64-bit target. A pointerful allocation at
// or below it is served by mallocgcSmallScanNoHeader.
const maxNoHeaderSize = 512

const reportedClasses = 61

// referent is what each pointer word in the allocated objects points at. It is
// deliberately pointer-free so that the collector's reachability decision about
// it depends only on the bitmap of the object holding the pointer.
type referent struct {
	value int64
}

// scanSink retains the pointerful objects. The referents are retained only
// through the pointer words under test.
var scanSink [][]*referent

func mallocsByClass(stats *runtime.MemStats) [reportedClasses]uint64 {
	var counts [reportedClasses]uint64
	for class := range stats.BySize {
		counts[class] = stats.BySize[class].Mallocs
	}
	return counts
}

func allocatePointerful(words int, tag int64) []*referent {
	object := make([]*referent, words)
	for index := range object {
		object[index] = &referent{value: tag + int64(index)}
	}
	scanSink = append(scanSink, object)
	return object
}

func checkPointerful(object []*referent, words int, tag int64) {
	if len(object) != words {
		panic("pointerful allocation has the wrong length")
	}
	for index, element := range object {
		if element == nil {
			panic("a pointer word in a heap object was cleared")
		}
		if element.value != tag+int64(index) {
			panic("a pointer word in a heap object no longer names its referent")
		}
	}
}

func main() {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	classSizes := make([]int, reportedClasses)
	for class := range stats.BySize {
		classSizes[class] = int(stats.BySize[class].Size)
	}

	before := mallocsByClass(&stats)

	// Class 1 is the eight-byte class, which is the one-pointer object. Every
	// class up to maxNoHeaderSize is a whole number of pointer words, so a
	// slice of pointers can hit each one exactly.
	lastClass := 1
	tag := int64(1)
	for class := 1; class < reportedClasses; class++ {
		size := classSizes[class]
		if size > maxNoHeaderSize {
			break
		}
		lastClass = class
		words := size / 8
		for repetition := 0; repetition < allocationsPerClass; repetition++ {
			object := allocatePointerful(words, tag)
			checkPointerful(object, words, tag)
			if uintptr(unsafe.Pointer(&object[0]))%8 != 0 {
				panic("pointerful allocation is not eight-byte aligned")
			}
			tag += int64(words) + 1
		}
	}
	if classSizes[lastClass] != maxNoHeaderSize {
		panic("the header-free pointerful range did not end at the header threshold")
	}

	// The only remaining references to the referents are the pointer words
	// inside the objects in scanSink. Collect twice so that anything the first
	// collection freed has been swept and is available for reuse, then allocate
	// over the freed memory before reading the pointer words back.
	runtime.GC()
	runtime.GC()
	overwrite := make([][]int64, 0, 4096)
	for repetition := 0; repetition < 4096; repetition++ {
		block := make([]int64, 8)
		for index := range block {
			block[index] = -1
		}
		overwrite = append(overwrite, block)
	}

	position := 0
	verifyTag := int64(1)
	for class := 1; class <= lastClass; class++ {
		words := classSizes[class] / 8
		for repetition := 0; repetition < allocationsPerClass; repetition++ {
			checkPointerful(scanSink[position], words, verifyTag)
			position++
			verifyTag += int64(words) + 1
		}
	}
	if position != len(scanSink) {
		panic("the retained allocation list does not match what was allocated")
	}
	runtime.KeepAlive(overwrite)

	runtime.ReadMemStats(&stats)
	after := mallocsByClass(&stats)
	for class := 1; class <= lastClass; class++ {
		if after[class]-before[class] < uint64(allocationsPerClass) {
			println("size class")
			println(classSizes[class])
			println("allocations observed")
			println(int(after[class] - before[class]))
			panic("a pointerful size class did not serve the allocations routed to it")
		}
	}
}
