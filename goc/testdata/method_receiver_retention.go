// A method that keeps its receiver, called on a local object.
//
// The escape walk used to answer every *immediately called* method selector
// with "does not escape" -- `x.m()` was free whatever m did with x. A method is
// a function whose first argument is the receiver, and `keep` publishes it, so
// the object has to outlive the call exactly as it would if it were passed to
// an ordinary function that stored it.
//
// Compiled before the rule that asks receiverDoesNotEscape, this program left
// `value` in `direct`'s frame with a package-level pointer into it and printed
// `false false false false` where the reference implementation printed `true`
// four times. gc says the same thing about the same function -- "leaking param:
// b" -- and puts the literal on the heap.
//
// The four fields exist so a partial answer cannot pass, and the recursion is
// there to write over the frame: an object read back out of a frame that has
// been reused is wrong in most of its words rather than in one. The GC call is
// the other half -- a package-level pointer into a stack span is what the mark
// phase reports as "found bad pointer in Go heap".
package main

import "runtime"

type retainedReceiver struct{ a, b, c, d int }

var retainedSink *retainedReceiver
var retainedChurn int

func (box *retainedReceiver) keep() int {
	retainedSink = box
	return box.a
}

//go:noinline
func retainReceiverInAFrame() {
	value := &retainedReceiver{a: 0x1111, b: 0x2222, c: 0x3333, d: 0x4444}
	if value.keep() != 0x1111 {
		panic("the method did not see its receiver")
	}
}

//go:noinline
func overwriteTheFrame(depth int) int {
	var junk [200]int
	for index := range junk {
		junk[index] = 0x77770000 + index
	}
	if depth > 0 {
		return junk[3] + overwriteTheFrame(depth-1)
	}
	return junk[5]
}

func main() {
	retainReceiverInAFrame()
	retainedChurn = overwriteTheFrame(200)
	runtime.GC()
	retainedChurn += overwriteTheFrame(200)
	runtime.GC()
	if retainedSink.a != 0x1111 || retainedSink.b != 0x2222 {
		panic("the retained receiver did not survive its frame")
	}
	if retainedSink.c != 0x3333 || retainedSink.d != 0x4444 {
		panic("the retained receiver did not survive its frame")
	}
	if retainedChurn == 0 {
		panic("the frame was not overwritten")
	}
	println("method-receiver-retention ok")
}
