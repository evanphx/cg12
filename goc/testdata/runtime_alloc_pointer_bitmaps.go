// Checks that the pointer words of a heap object are exactly where the type
// says they are, for aggregates that straddle the interesting boundaries.
//
// RUNTIME_PLAN.md section 6 asks for pointer-containing aggregates, interior
// pointers, and objects crossing span boundaries. The allocator writes each
// object's pointer/scalar bitmap from the type descriptor, so a wrong bitmap
// has two symptoms and this program looks for both:
//
//   - a bit that should be set but is not makes the collector treat a live
//     pointer as scalar, so its referent is freed and its memory reused. Each
//     referent below is reachable only through the word under test, and its
//     value is read back after a collection and a round of reuse.
//   - a bit that should not be set makes the collector treat a scalar as a
//     pointer. The scalar fields here are given values that are plausible
//     addresses of the object's own span, so following one would be a
//     retention or a fault rather than being ignored.
//
// The shapes are chosen so the bitmap has to be right in a different place each
// time: pointer first, pointer last, pointer alone in the middle, alternating,
// a pointer at word 63 (the last word a header-free object can have on a 64-bit
// target), and a pointer past word 64 in a header-carrying object.
package main

import (
	"runtime"
	"unsafe"
)

type referent struct {
	value int64
}

type pointerFirst struct {
	pointer *referent
	scalars [7]int64
}

type pointerLast struct {
	scalars [7]int64
	pointer *referent
}

type pointerMiddle struct {
	before  [3]int64
	pointer *referent
	after   [3]int64
}

type alternating struct {
	a *referent
	b int64
	c *referent
	d int64
	e *referent
	f int64
}

// lastNoHeaderWord is exactly runtime.minSizeForMallocHeader bytes, so its last
// pointer sits in word 63 and it is still served without a malloc header.
type lastNoHeaderWord struct {
	scalars [63]int64
	pointer *referent
}

// firstHeaderWord is one word larger, so it carries a malloc header and its
// pointer bitmap comes from the header's type rather than from the span.
type firstHeaderWord struct {
	scalars [64]int64
	pointer *referent
}

// plausibleScalar is a value that looks like an address but is not one the
// collector may follow. It is deliberately in the range the heap uses, so a
// bitmap bit set on a scalar word is much more likely to fault than to be
// ignored.
const plausibleScalar = int64(0x00c000000000)

var firstSink []*pointerFirst
var lastSink []*pointerLast
var middleSink []*pointerMiddle
var alternatingSink []*alternating
var noHeaderSink []*lastNoHeaderWord
var headerSink []*firstHeaderWord

const objects = 128

func fillScalars(scalars []int64, tag int64) {
	for index := range scalars {
		scalars[index] = plausibleScalar + tag + int64(index)
	}
}

func checkScalars(scalars []int64, tag int64) {
	for index := range scalars {
		if scalars[index] != plausibleScalar+tag+int64(index) {
			panic("a scalar word of a heap object was disturbed")
		}
	}
}

func build() {
	for index := 0; index < objects; index++ {
		tag := int64(index)

		first := &pointerFirst{pointer: &referent{value: tag}}
		fillScalars(first.scalars[:], tag)
		firstSink = append(firstSink, first)

		last := &pointerLast{pointer: &referent{value: tag + 1}}
		fillScalars(last.scalars[:], tag)
		lastSink = append(lastSink, last)

		middle := &pointerMiddle{pointer: &referent{value: tag + 2}}
		fillScalars(middle.before[:], tag)
		fillScalars(middle.after[:], tag+100)
		middleSink = append(middleSink, middle)

		mixed := &alternating{
			a: &referent{value: tag + 3},
			b: plausibleScalar + tag,
			c: &referent{value: tag + 4},
			d: plausibleScalar + tag + 1,
			e: &referent{value: tag + 5},
			f: plausibleScalar + tag + 2,
		}
		alternatingSink = append(alternatingSink, mixed)

		noHeader := &lastNoHeaderWord{pointer: &referent{value: tag + 6}}
		fillScalars(noHeader.scalars[:], tag)
		noHeaderSink = append(noHeaderSink, noHeader)

		header := &firstHeaderWord{pointer: &referent{value: tag + 7}}
		fillScalars(header.scalars[:], tag)
		headerSink = append(headerSink, header)
	}
}

func verify() {
	for index := 0; index < objects; index++ {
		tag := int64(index)

		first := firstSink[index]
		if first.pointer == nil || first.pointer.value != tag {
			panic("the leading pointer word of a heap object lost its referent")
		}
		checkScalars(first.scalars[:], tag)

		last := lastSink[index]
		if last.pointer == nil || last.pointer.value != tag+1 {
			panic("the trailing pointer word of a heap object lost its referent")
		}
		checkScalars(last.scalars[:], tag)

		middle := middleSink[index]
		if middle.pointer == nil || middle.pointer.value != tag+2 {
			panic("an interior pointer word of a heap object lost its referent")
		}
		checkScalars(middle.before[:], tag)
		checkScalars(middle.after[:], tag+100)

		mixed := alternatingSink[index]
		if mixed.a == nil || mixed.c == nil || mixed.e == nil {
			panic("an alternating pointer word of a heap object was cleared")
		}
		if mixed.a.value != tag+3 || mixed.c.value != tag+4 || mixed.e.value != tag+5 {
			panic("an alternating pointer word of a heap object lost its referent")
		}
		if mixed.b != plausibleScalar+tag ||
			mixed.d != plausibleScalar+tag+1 ||
			mixed.f != plausibleScalar+tag+2 {
			panic("an alternating scalar word of a heap object was disturbed")
		}

		noHeader := noHeaderSink[index]
		if noHeader.pointer == nil || noHeader.pointer.value != tag+6 {
			panic("the last word of a header-free object lost its referent")
		}
		checkScalars(noHeader.scalars[:], tag)

		header := headerSink[index]
		if header.pointer == nil || header.pointer.value != tag+7 {
			panic("the pointer word of a header-carrying object lost its referent")
		}
		checkScalars(header.scalars[:], tag)
	}
}

func main() {
	if unsafe.Sizeof(lastNoHeaderWord{}) != 512 {
		println("size of the header-free shape")
		println(int(unsafe.Sizeof(lastNoHeaderWord{})))
		panic("the header-free shape is not exactly the header threshold")
	}
	if unsafe.Sizeof(firstHeaderWord{}) != 520 {
		println("size of the header-carrying shape")
		println(int(unsafe.Sizeof(firstHeaderWord{})))
		panic("the header-carrying shape is not one word above the header threshold")
	}
	if unsafe.Offsetof(lastNoHeaderWord{}.pointer) != 504 {
		panic("the header-free shape's pointer is not in its last word")
	}

	build()
	verify()

	// Nothing but the pointer words under test reaches the referents.
	runtime.GC()
	runtime.GC()
	churn := make([][]int64, 0, 4096)
	for repetition := 0; repetition < 4096; repetition++ {
		block := make([]int64, 1)
		block[0] = -1
		churn = append(churn, block)
	}
	verify()
	runtime.KeepAlive(churn)
}
