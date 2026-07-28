// Stack-passed pointer arguments must be GC roots at every safepoint in the
// callee, not only at the stack-growth prologue's call to morestack.
//
// cg12 gives a function more integer-class argument registers than it has
// parameters here on purpose: every callee below takes eight integer parameters
// first, which fills the register set, so everything after them is passed in the
// caller's outgoing argument area and is described only by the callee's argument
// stack map. The callee then reaches a safepoint -- runtime.GC(), and a deep
// recursion that grows and copies the goroutine stack -- with those arguments
// still live, and uses them afterwards.
//
// The metadata this pins: internal/gometa writes the argument pointer map at
// every stack-map index, and the runtime selects the argument and locals maps
// with one shared PCDATA_StackMapIndex value. Writing it at index 0 alone left
// every safepoint outside the prologue reading an all-zero argument bitmap.
package main

import (
	"runtime"
	"unsafe"
)

type payload struct {
	value int
	next  *payload
}

type speaker interface {
	spoken() int
}

func (p *payload) spoken() int {
	return p.value
}

var collected = make(chan string, 8)

//go:noinline
func newPayload(value int, name string) *payload {
	object := &payload{value: value}
	runtime.AddCleanup(object, func(label string) {
		collected <- label
	}, name)
	return object
}

//go:noinline
func newText(seed int) string {
	buffer := make([]byte, 48)
	for index := range buffer {
		buffer[index] = byte('a' + (index+seed)%26)
	}
	text := string(buffer)
	runtime.AddCleanup(unsafe.StringData(text), func(label string) {
		collected <- label
	}, "text")
	return text
}

//go:noinline
func expectedText(seed int) string {
	buffer := make([]byte, 48)
	for index := range buffer {
		buffer[index] = byte('a' + (index+seed)%26)
	}
	return string(buffer)
}

// grow recurses far enough to force several stack growths, so the frames below
// it -- including the argument frames holding the stack-passed arguments -- are
// copied to a new stack while those arguments are live.
//
//go:noinline
func grow(depth int) int {
	if depth == 0 {
		return 0
	}
	return grow(depth-1) + 1
}

//go:noinline
func drainCollected() {
	for {
		select {
		case label := <-collected:
			println("collected while live:", label)
			panic("a live stack-passed argument was collected")
		default:
			return
		}
	}
}

// stacked takes eight integer parameters before its pointer-bearing ones, so
// object, text, boxed and items all arrive in the caller's outgoing argument
// area rather than in registers.
//
//go:noinline
func stacked(a0, a1, a2, a3, a4, a5, a6, a7 int, object *payload, text string, boxed speaker, items []*payload) int {
	for cycle := 0; cycle < 4; cycle++ {
		runtime.GC()
		runtime.Gosched()
		drainCollected()
	}
	depth := grow(20000)
	runtime.GC()
	drainCollected()

	if object.value != 0x2a {
		panic("stack-passed pointer argument lost its value")
	}
	if text != expectedText(3) {
		panic("stack-passed string argument lost its backing array")
	}
	if boxed.spoken() != 0x3b {
		panic("stack-passed interface argument lost its data word")
	}
	if len(items) != 2 || items[0].value != 0x4c || items[1].value != 0x5d {
		panic("stack-passed slice argument lost its backing array")
	}
	return depth + a0 + a7
}

func main() {
	items := []*payload{newPayload(0x4c, "items[0]"), newPayload(0x5d, "items[1]")}
	total := stacked(0, 1, 2, 3, 4, 5, 6, 7,
		newPayload(0x2a, "object"), newText(3), newPayload(0x3b, "boxed"), items)
	if total != 20007 {
		println("total", total)
		panic("wrong total")
	}
	println("stack argument roots ok")
}
