// Compiler-level reduction of the GC-assist allocation recursion described in
// RUNTIME_PLAN.md 5.2.1.
//
// conditionalCapture has exactly the shape of runtime.gcAssistAlloc's synctest
// block: a variable declared in an if-statement's init, captured by a deferred
// function literal that is only created when the branch is taken. The branch is
// never taken here, yet cg12 used to heap-lift the variable, so the function
// allocated on the path where no closure is ever built. That single allocation
// is what made runtime.gcAssistAlloc allocate, and therefore what made
// mallocgc -> deductAssistCredit -> gcAssistAlloc -> newobject -> mallocgc
// recurse without bound during marking.
//
// deferCapture is the same trigger without the branch: cg12 used to allocate
// twice there, a heap cell for the captured variable plus the closure. Neither
// closure outlives its frame, so neither may allocate. immediateCapture and
// deferNoCapture are the controls that show the trigger was specifically
// "defer" plus "capture": both always allocated nothing.
//
// All four counts must be 0, which is what the host toolchain reports:
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

	unconditional := int(testing.AllocsPerRun(50, deferCapture))
	println("deferCapture allocs", unconditional)

	immediate := int(testing.AllocsPerRun(50, immediateCapture))
	println("immediateCapture allocs", immediate)

	noCapture := int(testing.AllocsPerRun(50, deferNoCapture))
	println("deferNoCapture allocs", noCapture)

	if conditional != 0 {
		panic("conditional deferred capture allocated on the untaken branch")
	}
	if unconditional != 0 {
		panic("unconditional deferred capture allocated")
	}
	if immediate != 0 {
		panic("immediately invoked capture allocated")
	}
	if noCapture != 0 {
		panic("deferred literal without a capture allocated")
	}
}
