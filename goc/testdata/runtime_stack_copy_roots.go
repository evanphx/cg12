// Growing a goroutine's stack copies every live frame to a new allocation and
// rewrites every word that pointed into the old one. A word that the frame's
// pointer map claims and copystack misses is left addressing an abandoned stack;
// a word the map does not claim but that does point into the stack is left
// unadjusted and silently wrong.
//
// This program forces many growth and shrink cycles with pointer-bearing frames
// live across them, and is meant to run under GODEBUG=cg12checkstackcopy=1,
// which walks the copied stack afterwards and throws at any word still pointing
// into the old range that a stack map or the AAPCS saved-frame-pointer slot
// claims. That check turns a stale pointer into a throw naming the frame instead
// of a wrong answer somewhere later.
//
// The shapes are the ones whose stack words are adjusted by different code:
// ordinary locals and arguments, a frame holding a pointer into its own frame,
// a goroutine blocked on a channel while another grows it (adjustsudogs and the
// sghi bound), defer records live across the copy (adjustdefers), and a shrink
// triggered by runtime.GC() once the deep frames have returned.
package main

import (
	"runtime"
	"sync"
)

type link struct {
	value int
	label string
	next  *link
}

//go:noinline
func makeLink(seed int, next *link) *link {
	return &link{value: seed, label: "link-" + string(rune('a'+seed%26)), next: next}
}

//go:noinline
func verify(where string, object *link, seed int) {
	want := "link-" + string(rune('a'+seed%26))
	if object == nil || object.value != seed || object.label != want {
		println("in", where, "at", seed)
		panic("a stack copy lost a root")
	}
}

// deep recurses far enough to grow the stack several times, keeping a pointer
// live in every frame and an interior pointer to a frame-local array alongside
// it. The interior pointer is the one copystack has to rewrite: it addresses the
// frame itself, not the heap.
//
//go:noinline
func deep(depth int, chain *link) int {
	var scratch [24]uintptr
	scratch[0] = uintptr(depth)
	scratch[23] = uintptr(chain.value)
	interior := &scratch[12]
	*interior = uintptr(depth * 3)

	local := makeLink(depth%26, chain)

	if depth == 0 {
		runtime.GC()
		verify("deep bottom", local, 0)
		return int(*interior)
	}

	result := deep(depth-1, local)

	verify("deep return", local, depth%26)
	if *interior != uintptr(depth*3) {
		println("interior pointer at depth", depth, "reads", int(*interior))
		panic("a frame-interior pointer was not adjusted by the stack copy")
	}
	if scratch[23] != uintptr(chain.value) {
		panic("a scalar stack slot was corrupted by the stack copy")
	}
	return result + 1
}

// deferred keeps a defer record and its captured pointer live across a stack
// growth, so adjustdefers has to rewrite the link and the closure's captures.
//
//go:noinline
func deferred(depth int) int {
	object := makeLink(depth%26, nil)
	var seen int
	defer func() {
		verify("deferred", object, depth%26)
		seen++
	}()
	defer func() {
		if object.next != nil {
			panic("the deferred closure's capture was rewritten")
		}
	}()
	if depth == 0 {
		runtime.GC()
		return 0
	}
	result := deferred(depth - 1)
	verify("deferred return", object, depth%26)
	return result + 1
}

// blockedWhileGrown parks on a channel with a pointer live in its frame and a
// sudog on its own stack, then the other side grows and shrinks its own stack
// and collects before waking it. adjustsudogs and the sghi bound are what keep
// the parked goroutine's element pointer valid.
//
//go:noinline
func blockedWhileGrown(ready *sync.WaitGroup, channel chan *link, done *sync.WaitGroup) {
	object := makeLink(7, makeLink(8, nil))
	ready.Done()
	received := <-channel
	verify("blocked receiver", object, 7)
	verify("blocked receiver payload", received, 9)
	if object.next == nil || object.next.value != 8 {
		panic("a parked goroutine's chained root was lost across a stack copy")
	}
	done.Done()
}

//go:noinline
func shrinkCycles(rounds int) {
	for round := 0; round < rounds; round++ {
		if got := deep(400, makeLink(1, nil)); got != 400 {
			println("deep returned", got)
			panic("deep returned the wrong depth")
		}
		// The deep frames are gone, so the next collection is free to shrink
		// the stack back down; the following growth reallocates it.
		runtime.GC()
		runtime.GC()
	}
}

func main() {
	if got := deep(1200, makeLink(1, nil)); got != 1200 {
		println("deep returned", got)
		panic("deep returned the wrong depth")
	}
	if got := deferred(600); got != 600 {
		println("deferred returned", got)
		panic("deferred returned the wrong depth")
	}

	var ready sync.WaitGroup
	var done sync.WaitGroup
	channel := make(chan *link)
	ready.Add(1)
	done.Add(1)
	go blockedWhileGrown(&ready, channel, &done)
	ready.Wait()

	if got := deep(900, makeLink(2, nil)); got != 900 {
		panic("deep returned the wrong depth while a goroutine was parked")
	}
	runtime.GC()
	channel <- makeLink(9, nil)
	done.Wait()

	shrinkCycles(3)
	println("stack copy roots ok")
}
