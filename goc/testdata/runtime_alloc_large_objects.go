// Covers the large allocation family and the size classes above the ones
// runtime.MemStats.BySize reports.
//
// RUNTIME_PLAN.md section 6 asks for large allocations and for objects crossing
// span boundaries. An object larger than runtime.maxSmallSize minus the malloc
// header goes to runtime.mallocgcLarge, which gives it a span of its own rather
// than a size class, so it is page aligned and spans as many pages as it needs.
// Pointerful objects between the header threshold and that limit go to
// runtime.mallocgcSmallScanHeader, which prefixes the object with an eight-byte
// type header instead of writing bits into the span's bitmap.
//
// Asserted properties:
//
//   - a large allocation is page aligned and is zeroed, including its last
//     byte, which is what proves the whole multi-page extent belongs to it;
//   - two large allocations do not overlap, checked by writing a distinct
//     pattern at both ends of each and reading them all back;
//   - a pointerful object above the header threshold keeps every pointer word,
//     including the last one, across a collection, which is the property the
//     malloc header exists to support;
//   - a pointerful object several pages long keeps pointer words at the start,
//     in the middle, and at the end.
package main

import (
	"runtime"
	"unsafe"
)

// headerThreshold is runtime.minSizeForMallocHeader. Above it a pointerful
// object carries an eight-byte malloc header.
const headerThreshold = 512

// maxSmallSize is runtime.maxSmallSize, and gc.MallocHeaderSize is 8, so a
// request above maxSmallSize-8 becomes a large allocation.
const maxSmallSize = 32768
const mallocHeaderSize = 8
const firstLargeSize = maxSmallSize - mallocHeaderSize + 1

// pageSize is runtime.pageSize on this target: 1 << gc.PageShift.
const pageSize = 8192

type referent struct {
	value int64
}

var largeSink [][]byte
var pointerfulSink [][]*referent

func allocateLarge(size int, tag byte) []byte {
	block := make([]byte, size)
	for index := range block {
		if block[index] != 0 {
			panic("a large allocation was not zeroed")
		}
	}
	block[0] = tag
	block[size/2] = tag + 1
	block[size-1] = tag + 2
	largeSink = append(largeSink, block)
	return block
}

func checkLarge(block []byte, size int, tag byte) {
	if len(block) != size {
		panic("a large allocation has the wrong length")
	}
	if block[0] != tag || block[size/2] != tag+1 || block[size-1] != tag+2 {
		panic("two large allocations overlap or one was reused")
	}
}

func allocatePointerful(words int, tag int64) []*referent {
	object := make([]*referent, words)
	object[0] = &referent{value: tag}
	object[words/2] = &referent{value: tag + 1}
	object[words-1] = &referent{value: tag + 2}
	pointerfulSink = append(pointerfulSink, object)
	return object
}

func checkPointerful(object []*referent, words int, tag int64) {
	if len(object) != words {
		panic("a pointerful allocation has the wrong length")
	}
	if object[0] == nil || object[words/2] == nil || object[words-1] == nil {
		panic("a pointer word in a large object was cleared")
	}
	if object[0].value != tag || object[words/2].value != tag+1 || object[words-1].value != tag+2 {
		panic("a pointer word in a large object no longer names its referent")
	}
	for index, element := range object {
		if index == 0 || index == words/2 || index == words-1 {
			continue
		}
		if element != nil {
			panic("an untouched pointer word in a large object is not nil")
		}
	}
}

func main() {
	largeSizes := []int{
		firstLargeSize,
		firstLargeSize + 1,
		2 * pageSize,
		5*pageSize + 17,
		64 * pageSize,
	}
	for index, size := range largeSizes {
		for repetition := 0; repetition < 4; repetition++ {
			tag := byte(index*8 + repetition + 1)
			block := allocateLarge(size, tag)
			if uintptr(unsafe.Pointer(&block[0]))%pageSize != 0 {
				println("large allocation size")
				println(size)
				panic("a large allocation is not page aligned")
			}
			checkLarge(block, size, tag)
		}
	}

	// Pointerful objects above the header threshold, both in the header-carrying
	// small classes and in the large family.
	pointerfulWords := []int{
		headerThreshold/8 + 1,
		headerThreshold/8 + 2,
		1024,
		4096,
		firstLargeSize/8 + 8,
		8 * pageSize / 8,
	}
	for index, words := range pointerfulWords {
		for repetition := 0; repetition < 4; repetition++ {
			tag := int64(index*64 + repetition*8 + 1)
			object := allocatePointerful(words, tag)
			checkPointerful(object, words, tag)
		}
	}

	// The referents are reachable only through the pointer words inside the
	// large objects, so a wrong pointer bitmap or a wrong header loses them.
	runtime.GC()
	runtime.GC()
	churn := make([][]byte, 0, 1024)
	for repetition := 0; repetition < 1024; repetition++ {
		block := make([]byte, 1024)
		for position := range block {
			block[position] = 0xff
		}
		churn = append(churn, block)
	}

	position := 0
	for index, size := range largeSizes {
		for repetition := 0; repetition < 4; repetition++ {
			checkLarge(largeSink[position], size, byte(index*8+repetition+1))
			position++
		}
	}
	if position != len(largeSink) {
		panic("the retained large allocation list does not match what was allocated")
	}

	position = 0
	for index, words := range pointerfulWords {
		for repetition := 0; repetition < 4; repetition++ {
			checkPointerful(pointerfulSink[position], words, int64(index*64+repetition*8+1))
			position++
		}
	}
	if position != len(pointerfulSink) {
		panic("the retained pointerful allocation list does not match what was allocated")
	}
	runtime.KeepAlive(churn)
}
