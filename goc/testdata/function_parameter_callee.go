// A call through a function-typed *parameter*, which is not resolvable.
//
// The escape walk resolves a call through a function-typed local to the one
// function assigned to it, which is what lets `var f func(*T) = callee; f(x)`
// ask callee's summary about x. A parameter is not a local: its first value
// comes from the caller, so an assignment to it inside the body says nothing
// about what the calls before that assignment reached.
//
// Without the "declared in this body" test, `viaParameter` below had exactly one
// assignment to `g` -- the `g = dropsBox` after the call -- and the walk resolved
// `g(box)` to `dropsBox`, which keeps nothing. `keepsBox` is what actually runs,
// and it keeps the box, so the box was left in a frame with a package-level
// pointer into it.
//
// The four fields and the recursion are the same instrument
// method_receiver_retention.go uses: an object read back out of a reused frame
// is wrong in most of its words rather than in one.
package main

import "runtime"

type parameterCalleeBox struct{ a, b, c, d int }

var parameterCalleeSink *parameterCalleeBox
var parameterCalleeChurn int

//go:noinline
func keepsBox(box *parameterCalleeBox) { parameterCalleeSink = box }

//go:noinline
func dropsBox(box *parameterCalleeBox) {
	if box.a == 0 {
		panic("empty box")
	}
}

//go:noinline
func viaParameter(call func(*parameterCalleeBox)) {
	box := &parameterCalleeBox{a: 0x1111, b: 0x2222, c: 0x3333, d: 0x4444}
	call(box)
	call = dropsBox
	call(box)
}

//go:noinline
func overwriteTheCallerFrame(depth int) int {
	var junk [200]int
	for index := range junk {
		junk[index] = 0x66660000 + index
	}
	if depth > 0 {
		return junk[3] + overwriteTheCallerFrame(depth-1)
	}
	return junk[5]
}

func main() {
	viaParameter(keepsBox)
	parameterCalleeChurn = overwriteTheCallerFrame(200)
	runtime.GC()
	parameterCalleeChurn += overwriteTheCallerFrame(200)
	runtime.GC()
	if parameterCalleeSink.a != 0x1111 || parameterCalleeSink.b != 0x2222 {
		panic("the box did not survive the frame it was passed from")
	}
	if parameterCalleeSink.c != 0x3333 || parameterCalleeSink.d != 0x4444 {
		panic("the box did not survive the frame it was passed from")
	}
	if parameterCalleeChurn == 0 {
		panic("the frame was not overwritten")
	}
	println("function-parameter-callee ok")
}
