// Reaches every pointer-free small size class the allocator has, and proves it
// reached them rather than assuming it.
//
// RUNTIME_PLAN.md section 6 asks for specialized allocations across every
// size-class family. The size-specialized malloc entry points in
// malloc_generated.go are disabled in this build (see section 6), so the live
// pointer-free family is runtime.mallocgcSmallNoscan, and the thing that varies
// per family is the size class: its span class, its elemsize, and the
// mcache/mcentral path that refills it.
//
// runtime.MemStats.BySize is the instrument. BySize[c].Size is the size class's
// elemsize and BySize[c].Mallocs counts objects allocated in it, so allocating
// a known number of objects of a known size and reading the counter back is a
// direct measurement of which family ran, not an inference from coverage.
//
// Three properties are asserted per class:
//
//   - an allocation of exactly the class size lands in that class;
//   - an allocation one byte above the previous class also lands in it, which
//     is the size-to-class rounding rule;
//   - the object is 8-byte aligned and holds the bytes written into it after a
//     collection, so the class's span really owns memory of that size.
package main

import (
	"runtime"
	"unsafe"
)

// allocationsPerClass is large enough that the measured counter delta cannot be
// explained by unrelated runtime allocations of the same size, and small enough
// that the largest classes do not dominate the program's footprint.
const allocationsPerClass = 64

// reportedClasses is the number of size classes runtime.MemStats.BySize
// describes. Classes above it exist but are not reported, so they are covered
// by runtime_alloc_large_objects.go instead.
const reportedClasses = 61

// noscanSink retains every allocation so escape analysis cannot keep them on
// the stack and the collector cannot reuse them mid-measurement.
var noscanSink [][]byte

func mallocsByClass(stats *runtime.MemStats) [reportedClasses]uint64 {
	var counts [reportedClasses]uint64
	for class := range stats.BySize {
		counts[class] = stats.BySize[class].Mallocs
	}
	return counts
}

func allocateExactly(size int) []byte {
	block := make([]byte, size)
	for index := range block {
		block[index] = byte(index + size)
	}
	noscanSink = append(noscanSink, block)
	return block
}

func checkContents(block []byte, size int) {
	if len(block) != size {
		panic("pointer-free allocation has the wrong length")
	}
	for index := range block {
		if block[index] != byte(index+size) {
			panic("pointer-free allocation lost its contents")
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
	// Class 1 is the eight-byte class. A pointer-free eight-byte request is
	// below runtime.maxTinySize, so it is served by the tiny allocator out of a
	// class-2 block and never allocates in class 1 on its own. Classes 2 and up
	// are the pointer-free families this program owns.
	const firstPointerFreeClass = 2

	before := mallocsByClass(&stats)
	for class := firstPointerFreeClass; class < reportedClasses; class++ {
		size := classSizes[class]
		previousSize := classSizes[class-1]
		for repetition := 0; repetition < allocationsPerClass; repetition++ {
			block := allocateExactly(size)
			checkContents(block, size)
			if uintptr(unsafe.Pointer(&block[0]))%8 != 0 {
				panic("pointer-free allocation is not eight-byte aligned")
			}
		}
		// The smallest request that must round up into this class.
		roundedUp := allocateExactly(previousSize + 1)
		checkContents(roundedUp, previousSize+1)
	}

	runtime.GC()

	runtime.ReadMemStats(&stats)
	after := mallocsByClass(&stats)

	for class := firstPointerFreeClass; class < reportedClasses; class++ {
		// allocationsPerClass exact-size requests plus the one rounded-up
		// request must all have been served by this class.
		expected := uint64(allocationsPerClass + 1)
		if after[class]-before[class] < expected {
			println("size class")
			println(classSizes[class])
			println("allocations observed")
			println(int(after[class] - before[class]))
			panic("a pointer-free size class did not serve the allocations routed to it")
		}
	}

	// Everything is still reachable through noscanSink, so a collection must
	// not have disturbed any of it.
	position := 0
	for class := firstPointerFreeClass; class < reportedClasses; class++ {
		size := classSizes[class]
		for repetition := 0; repetition < allocationsPerClass; repetition++ {
			checkContents(noscanSink[position], size)
			position++
		}
		checkContents(noscanSink[position], classSizes[class-1]+1)
		position++
	}
	if position != len(noscanSink) {
		panic("the retained allocation list does not match what was allocated")
	}
}
