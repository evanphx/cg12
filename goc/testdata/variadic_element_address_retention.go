// A frame address handed to a variadic callee that keeps the element.
//
// This is the other half of the question variadic_element_retention.go asks,
// and it is asked of the front end rather than of opt. `keepFirst` retains no
// pointer it was handed -- `args` itself is dropped -- and retains everything
// the elements point at, which is `caller`'s `local`. A summary consulted at
// the *parameter* says "does not escape" and is answering about the slice; the
// caller's object is an element of it. Acting on that answer leaves a
// package-level variable pointing into a frame that has returned.
//
// What makes it observable rather than latent is the collector: `sink` is a
// heap root, so a mark phase walks the pointer, finds it addresses a stack span
// rather than a heap object, and throws "found bad pointer in Go heap". The
// deep recursion before the collection is there to make the answer wrong in the
// quiet direction too -- the frame is written over, so the four fields would
// read back as churn's junk if the collector let the program get that far.
//
// The four fields exist so a partial answer cannot pass: an object placed in a
// frame and then read after that frame is reused is wrong in most of its words,
// not in one.
package main

import "runtime"

type addressBox struct{ a, b, c, d int }

var sink *addressBox
var accumulated int

//go:noinline
func keepFirst(args ...*addressBox) { sink = args[0] }

//go:noinline
func caller() {
	var local addressBox
	local.a, local.b, local.c, local.d = 0x1111, 0x2222, 0x3333, 0x4444
	keepFirst(&local)
}

//go:noinline
func churn(depth int) int {
	var junk [200]int
	for i := range junk {
		junk[i] = 0x77770000 + i
	}
	if depth > 0 {
		return junk[3] + churn(depth-1)
	}
	return junk[5]
}

func main() {
	caller()
	accumulated = churn(200)
	runtime.GC()
	accumulated += churn(200)
	runtime.GC()
	println(sink.a == 0x1111, sink.b == 0x2222, sink.c == 0x3333, sink.d == 0x4444)
	println("churned", accumulated > 0)
}
