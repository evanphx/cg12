// Covers zero-sized allocations and zero-sized fields.
//
// RUNTIME_PLAN.md section 6 asks for zero-sized allocations and zero-sized
// fields. runtime.mallocgc short-circuits a zero-byte request to the address of
// runtime.zerobase without touching a span at all, so the family is a distinct
// path rather than a size class, and the interesting properties are layout
// ones.
//
// The layout rule that matters for the collector is the trailing zero-sized
// field. A struct whose last field is zero-sized is padded by one element of
// alignment, because otherwise &s.tail would point one byte past the object and
// the collector would resolve it to the following object in the span. This
// program asserts that padding directly, and then asserts that a pointer to a
// trailing zero-sized field keeps its containing object alive across a
// collection rather than keeping the next object alive.
//
// The Go specification says pointers to distinct zero-size variables may or may
// not be equal, so nothing here asserts either way about their addresses.
package main

import (
	"runtime"
	"unsafe"
)

type empty struct{}

// trailingEmpty ends with a zero-sized field, so its size must be padded past
// the last real field.
type trailingEmpty struct {
	value int64
	tail  empty
}

// leadingEmpty starts with one, which needs no padding.
type leadingEmpty struct {
	head  empty
	value int64
}

// middleEmpty has one between two real fields.
type middleEmpty struct {
	first  int32
	middle empty
	second int32
}

// emptyOnly has nothing but zero-sized fields.
type emptyOnly struct {
	first  empty
	second empty
}

// tailAnchor is what the trailing-zero-sized-field retention check keeps alive.
type tailAnchor struct {
	payload [64]byte
	tail    empty
}

var zeroSink []*empty
var tailSink []*empty

func main() {
	if unsafe.Sizeof(empty{}) != 0 {
		panic("an empty struct is not zero-sized")
	}
	if unsafe.Alignof(empty{}) != 1 {
		panic("an empty struct does not have alignment one")
	}
	if unsafe.Sizeof(emptyOnly{}) != 0 {
		panic("a struct of only zero-sized fields is not zero-sized")
	}
	if unsafe.Sizeof(leadingEmpty{}) != 8 {
		panic("a leading zero-sized field changed the size of its struct")
	}
	if unsafe.Sizeof(middleEmpty{}) != 8 {
		panic("an interior zero-sized field changed the size of its struct")
	}
	// The trailing field is padded to the struct's alignment so that its
	// address is not one past the end of the object.
	if unsafe.Sizeof(trailingEmpty{}) != 16 {
		println("size of a struct with a trailing zero-sized field")
		println(int(unsafe.Sizeof(trailingEmpty{})))
		panic("a trailing zero-sized field was not padded")
	}
	if unsafe.Offsetof(trailingEmpty{}.tail) != 8 {
		panic("a trailing zero-sized field is at the wrong offset")
	}
	// tailAnchor's alignment is one, so its padding is a single byte.
	if unsafe.Sizeof(tailAnchor{}) != 65 {
		println("size of the trailing-zero-sized-field anchor")
		println(int(unsafe.Sizeof(tailAnchor{})))
		panic("the trailing zero-sized field anchor was not padded")
	}
	if unsafe.Offsetof(tailAnchor{}.tail) != 64 {
		panic("the anchor's trailing zero-sized field is at the wrong offset")
	}

	// Zero-sized heap allocations must be usable: non-nil, storable, and safe
	// to hold across a collection.
	for repetition := 0; repetition < 1024; repetition++ {
		object := new(empty)
		if object == nil {
			panic("a zero-sized allocation returned nil")
		}
		zeroSink = append(zeroSink, object)
	}

	// A slice of zero-sized elements has a real length and capacity and can be
	// indexed and ranged, but its backing store occupies nothing.
	zeroSlice := make([]empty, 512)
	if len(zeroSlice) != 512 || cap(zeroSlice) != 512 {
		panic("a slice of zero-sized elements has the wrong shape")
	}
	visited := 0
	for range zeroSlice {
		visited++
	}
	if visited != 512 {
		panic("a slice of zero-sized elements did not range over every element")
	}
	zeroSlice = append(zeroSlice, empty{})
	if len(zeroSlice) != 513 {
		panic("appending to a slice of zero-sized elements did not grow it")
	}
	zeroArray := new([256]empty)
	if unsafe.Sizeof(*zeroArray) != 0 {
		panic("an array of zero-sized elements is not zero-sized")
	}

	// A pointer to a trailing zero-sized field must retain the object that
	// contains it. Only the field pointers are retained, so if the padding were
	// missing the address would be one past the end of the anchor, the
	// collector would resolve it to the following object in the span, and the
	// anchors would be freed and reused. The payload is read back through the
	// field pointer, which stays inside the anchor, so the check sees exactly
	// the memory the retained pointer is meant to have kept.
	for repetition := 0; repetition < 256; repetition++ {
		anchor := &tailAnchor{}
		for index := range anchor.payload {
			anchor.payload[index] = byte(repetition + index)
		}
		tailSink = append(tailSink, &anchor.tail)
	}

	runtime.GC()
	runtime.GC()
	reuse := make([][]byte, 0, 4096)
	for repetition := 0; repetition < 4096; repetition++ {
		block := make([]byte, 65)
		for index := range block {
			block[index] = 0xff
		}
		reuse = append(reuse, block)
	}

	if len(tailSink) != 256 {
		panic("the trailing-field pointer list was disturbed")
	}
	for index, tail := range tailSink {
		payload := (*[64]byte)(unsafe.Add(unsafe.Pointer(tail), -int(unsafe.Offsetof(tailAnchor{}.tail))))
		for position := range payload {
			if payload[position] != byte(index+position) {
				panic("an object retained only by a pointer to its trailing zero-sized field was reused")
			}
		}
	}
	runtime.KeepAlive(reuse)
	for _, object := range zeroSink {
		if object == nil {
			panic("a zero-sized allocation became nil across a collection")
		}
	}
	runtime.KeepAlive(zeroSlice)
	runtime.KeepAlive(zeroArray)
}
