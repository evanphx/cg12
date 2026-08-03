// The other half of variadic_backing.go. There the callee keeps nothing and the
// `[N]any` backing array belongs in the caller's frame; here the callee keeps an
// *element*, which is the boxed payload the element's data word points at and
// not the array. Both are true of the same call, and a compiler that cannot say
// them separately has to be wrong about one of them.
//
// Getting it wrong in the cheap direction costs an allocation. Getting it wrong
// in the other direction is what this program is for: the payload is left in a
// frame that returns, a package-level variable keeps pointing at it, and the
// value read back later is whatever the stack has since been used for. The
// churn below is what makes that observable rather than lucky -- a deep
// recursion that writes over the returned frame and grows the stack far enough
// to be copied, so a pointer into a dead frame is both overwritten and
// relocated before it is read.
//
// The retained values are past runtime.staticuint64s (which stops at 255) on
// purpose. A value inside the table boxes to a pointer into read-only memory
// whatever the escape analysis decides, so the program would print the right
// answer without measuring anything.
package main

var kept any
var keptSecond any
var noticed int

//go:noinline
func keepFirstElement(args ...any) { kept = args[0] }

//go:noinline
func keepBothElements(args ...any) {
	kept = args[0]
	keptSecond = args[1]
}

//go:noinline
func retainNothing(args ...any) int { return len(args) }

//go:noinline
func handOverOne() { keepFirstElement(0x5eed) }

//go:noinline
func handOverTwo() { keepBothElements(0xbeef, 0xcafe) }

//go:noinline
func handOverNothing() int {
	local := 0x11ff
	return retainNothing(local, "and a string")
}

// churn overwrites the frames handOverOne and handOverTwo returned from and
// grows the stack past its initial segment, so the runtime has to copy it.
//
//go:noinline
func churn(depth int) int {
	var scratch [128]int
	for i := range scratch {
		scratch[i] = i*7 + depth
	}
	if depth > 0 {
		return scratch[11] + churn(depth-1)
	}
	return scratch[13]
}

func main() {
	handOverOne()
	handOverTwo()
	noticed = handOverNothing()
	noticed += churn(64)

	first, firstIsInt := kept.(int)
	second, secondIsInt := keptSecond.(int)
	if !firstIsInt || !secondIsInt {
		println("retained values lost their type")
		return
	}
	println("first ", first)
	println("second", second)
	println("length", noticed > 0)
}
