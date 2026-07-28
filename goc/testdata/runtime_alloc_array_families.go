// Covers array allocation and the alignment contract, across element types
// that route to different allocation families.
//
// RUNTIME_PLAN.md section 6 asks for array and aligned allocations.
// runtime.newarray multiplies the element size by the count and hands the
// result to runtime.mallocgc, so an array picks its family from the total size
// while its pointer bitmap is a repetition of the element's. That makes arrays
// the case where a size class is reached with a bitmap that repeats, which is
// the branch of writeHeapBitsSmall that a single object never takes.
//
// Asserted properties:
//
//   - every allocation is aligned to at least its element type's alignment, and
//     an allocation containing an eight-byte field is eight-byte aligned, which
//     is what makes atomic access to it well defined;
//   - the elements of an array are exactly unsafe.Sizeof apart, so the
//     allocation really has the stride the type says;
//   - a pointerful array keeps every element's pointer across a collection, so
//     the repeated bitmap is right in every element rather than only the first;
//   - an array too large to allocate makes make panic rather than returning a
//     short array.
package main

import (
	"runtime"
	"unsafe"
)

type referent struct {
	value int64
}

// smallPointerful is one pointer word, so an array of it repeats a single-bit
// bitmap.
type smallPointerful struct {
	pointer *referent
}

// mixedPointerful has a scalar before and after its pointer, so an array of it
// repeats a bitmap with the pointer bit off the first word.
type mixedPointerful struct {
	before int64
	middle *referent
	after  int32
}

// wideScalar has an eight-byte field, so its allocations must be eight-byte
// aligned even though the struct starts with a byte.
type wideScalar struct {
	tag   byte
	value int64
}

var pointerArrays [][]smallPointerful
var mixedArrays [][]mixedPointerful

func checkStride(base unsafe.Pointer, second unsafe.Pointer, elementSize uintptr) {
	if uintptr(second)-uintptr(base) != elementSize {
		panic("array elements are not one element size apart")
	}
}

func allocatePointerArray(length int, tag int64) []smallPointerful {
	array := make([]smallPointerful, length)
	for index := range array {
		array[index].pointer = &referent{value: tag + int64(index)}
	}
	pointerArrays = append(pointerArrays, array)
	return array
}

func checkPointerArray(array []smallPointerful, length int, tag int64) {
	if len(array) != length {
		panic("a pointerful array has the wrong length")
	}
	for index := range array {
		if array[index].pointer == nil {
			panic("a pointer element of an array was cleared")
		}
		if array[index].pointer.value != tag+int64(index) {
			panic("a pointer element of an array no longer names its referent")
		}
	}
}

func allocateMixedArray(length int, tag int64) []mixedPointerful {
	array := make([]mixedPointerful, length)
	for index := range array {
		array[index].before = tag + int64(index)
		array[index].after = int32(index)
		array[index].middle = &referent{value: tag - int64(index)}
	}
	mixedArrays = append(mixedArrays, array)
	return array
}

func checkMixedArray(array []mixedPointerful, length int, tag int64) {
	if len(array) != length {
		panic("a mixed array has the wrong length")
	}
	for index := range array {
		if array[index].before != tag+int64(index) || array[index].after != int32(index) {
			panic("a scalar field of a mixed array element was disturbed")
		}
		if array[index].middle == nil {
			panic("a pointer field of a mixed array element was cleared")
		}
		if array[index].middle.value != tag-int64(index) {
			panic("a pointer field of a mixed array element no longer names its referent")
		}
	}
}

func makeTooLarge() (recovered bool) {
	defer func() {
		if recover() != nil {
			recovered = true
		}
	}()
	length := int(^uint(0) >> 1)
	oversized := make([]int64, length)
	runtime.KeepAlive(oversized)
	return false
}

func main() {
	// Alignment. Every allocation is at least eight-byte aligned on this
	// target, and an array's element stride is exactly unsafe.Sizeof.
	scalars := make([]wideScalar, 8)
	if uintptr(unsafe.Pointer(&scalars[0]))%8 != 0 {
		panic("an array with an eight-byte field is not eight-byte aligned")
	}
	checkStride(unsafe.Pointer(&scalars[0]), unsafe.Pointer(&scalars[1]), unsafe.Sizeof(wideScalar{}))
	if unsafe.Alignof(wideScalar{}) != 8 {
		panic("a struct with an eight-byte field does not have alignment eight")
	}

	bytes := make([]byte, 3)
	if unsafe.Alignof(bytes[0]) != 1 {
		panic("a byte does not have alignment one")
	}
	checkStride(unsafe.Pointer(&bytes[0]), unsafe.Pointer(&bytes[1]), 1)

	pointers := make([]*referent, 4)
	if uintptr(unsafe.Pointer(&pointers[0]))%8 != 0 {
		panic("a pointer array is not eight-byte aligned")
	}
	checkStride(unsafe.Pointer(&pointers[0]), unsafe.Pointer(&pointers[1]), 8)

	nested := new([4][3]int32)
	if unsafe.Sizeof(*nested) != 48 {
		panic("a two-dimensional array has the wrong size")
	}
	checkStride(unsafe.Pointer(&nested[0]), unsafe.Pointer(&nested[1]), 12)
	if uintptr(unsafe.Pointer(nested))%4 != 0 {
		panic("an int32 array is not four-byte aligned")
	}

	// Arrays across the allocation families: tiny, the header-free pointerful
	// classes, the header-carrying classes, and large.
	pointerLengths := []int{1, 2, 8, 64, 65, 1024, 8192}
	for index, length := range pointerLengths {
		tag := int64(index*100000 + 1)
		array := allocatePointerArray(length, tag)
		checkPointerArray(array, length, tag)
		if uintptr(unsafe.Pointer(&array[0]))%8 != 0 {
			panic("a pointerful array is not eight-byte aligned")
		}
		if length > 1 {
			checkStride(unsafe.Pointer(&array[0]), unsafe.Pointer(&array[1]), unsafe.Sizeof(smallPointerful{}))
		}
	}

	mixedLengths := []int{1, 3, 16, 129, 2048}
	for index, length := range mixedLengths {
		tag := int64(index*4096 + 3)
		array := allocateMixedArray(length, tag)
		checkMixedArray(array, length, tag)
		if length > 1 {
			checkStride(unsafe.Pointer(&array[0]), unsafe.Pointer(&array[1]), unsafe.Sizeof(mixedPointerful{}))
		}
	}

	runtime.GC()
	runtime.GC()
	churn := make([][]byte, 0, 2048)
	for repetition := 0; repetition < 2048; repetition++ {
		block := make([]byte, 96)
		for position := range block {
			block[position] = 0xff
		}
		churn = append(churn, block)
	}

	for index, length := range pointerLengths {
		checkPointerArray(pointerArrays[index], length, int64(index*100000+1))
	}
	for index, length := range mixedLengths {
		checkMixedArray(mixedArrays[index], length, int64(index*4096+3))
	}
	runtime.KeepAlive(churn)

	if !makeTooLarge() {
		panic("an impossible array allocation did not panic")
	}
}
