// Compiler-level reduction of the GC-assist allocation recursion described in
// RUNTIME_PLAN.md 5.2.1.
//
// conditionalCapture has exactly the shape of runtime.gcAssistAlloc's synctest
// block: a variable declared in an if-statement's init, captured by a deferred
// function literal that is only created when the branch is taken. The branch is
// never taken here, yet cg12 still heap-lifts the variable, so the function
// allocates on the path where no closure is ever built. That single allocation
// is what makes runtime.gcAssistAlloc allocate, and therefore what makes
// mallocgc -> deductAssistCredit -> gcAssistAlloc -> newobject -> mallocgc
// recurse without bound during marking.
//
// Only conditionalCapture is asserted. deferCapture allocates twice under cg12
// (a heap cell for the captured variable plus the closure) where the standard
// compiler allocates nothing, but that is the deliberate heap-lift recorded in
// RUNTIME_PLAN.md 5.1, so this program reports its count as a diagnostic rather
// than requiring it to be zero. immediateCapture and deferNoCapture are the
// controls that show the trigger is specifically "defer" plus "capture": both
// already allocate nothing.
//
// Expected output on the host toolchain:
//
//	conditionalCapture allocs 0
//	deferCapture allocs 0
//	immediateCapture allocs 0
//	deferNoCapture allocs 0
package main

import "testing"

type capturedBubble struct {
	value int
}

type capturedG struct {
	bubble *capturedBubble
}

var currentG capturedG

var captureSink int

//go:noinline
func getCurrentG() *capturedG {
	return &currentG
}

// conditionalCapture never enters its branch, because currentG.bubble is
// always nil, so it must not allocate.
//
//go:noinline
func conditionalCapture() {
	if g := getCurrentG(); g.bubble != nil {
		saved := g.bubble
		g.bubble = nil
		defer func() {
			g.bubble = saved
		}()
	}
	captureSink++
}

//go:noinline
func deferCapture() {
	g := getCurrentG()
	defer func() {
		if g.bubble != nil {
			captureSink += g.bubble.value
		}
	}()
	captureSink++
}

//go:noinline
func immediateCapture() {
	g := getCurrentG()
	func() {
		if g.bubble != nil {
			captureSink += g.bubble.value
		}
	}()
	captureSink++
}

//go:noinline
func deferNoCapture() {
	defer func() {
		captureSink++
	}()
	captureSink++
}

func main() {
	conditional := int(testing.AllocsPerRun(50, conditionalCapture))
	println("conditionalCapture allocs", conditional)
	println("deferCapture allocs", int(testing.AllocsPerRun(50, deferCapture)))
	println("immediateCapture allocs", int(testing.AllocsPerRun(50, immediateCapture)))
	println("deferNoCapture allocs", int(testing.AllocsPerRun(50, deferNoCapture)))

	if conditional != 0 {
		panic("conditional deferred capture allocated on the untaken branch")
	}
}
