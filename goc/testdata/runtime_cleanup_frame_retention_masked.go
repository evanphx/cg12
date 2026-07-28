// The negative results from reducing the over-retention bug in
// RUNTIME_PLAN.md 5.3. Each shape here is one hypothesis that was tested and
// eliminated: all of them release the object and run their cleanup, while the
// single-cleanup shape in runtime_cleanup_frame_retention.go does not.
//
// Keeping them executable stops the next investigation from re-litigating
// causes already ruled out: the cleanup argument shape, allocation pressure,
// the number of cleanups, and whether finalizers work at all.
//
// The positive proof of the diagnosis -- that overwriting the abandoned frame
// is what makes the object collectable -- lives in
// runtime_cleanup_frame_retention_scribble.go rather than here. It was tried in
// this file first and did not reproduce, because appending it to four earlier
// variants changes the stack it would have to overwrite. That is the same
// layout sensitivity described in runtime_cleanup_frame_retention.go, and it is
// the reason each shape is kept at the frame depth where it was verified
// instead of being folded together for convenience.
//
// If one of these starts failing, the boundary of the bug has moved and
// RUNTIME_PLAN.md 5.3 needs revisiting.
package main

import (
	"runtime"
	"time"
	"unsafe"
)

type maskedBox struct {
	value   int
	pointer unsafe.Pointer
}

var sink *maskedBox

func waitForCleanup(done chan struct{}, label string) {
	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-done:
			return
		default:
		}
	}
	panic(label + ": cleanup did not run")
}

// A heap allocation after the registration overwrites the stale word.
func registerWithTrailingAllocation(done chan struct{}) {
	box := &maskedBox{value: 1}
	runtime.AddCleanup(box, func(struct{}) { done <- struct{}{} }, struct{}{})
	sink = &maskedBox{value: 2}
}

// runtime.KeepAlive on an unrelated value shifts the frame enough to release it.
func registerWithKeepAlive(done chan struct{}) {
	box := &maskedBox{value: 1}
	runtime.AddCleanup(box, func(struct{}) { done <- struct{}{} }, struct{}{})
	runtime.KeepAlive(done)
}

// Two tracked objects: both cleanups run, so this is not about cleanup count.
func registerTwoObjects(done chan struct{}) {
	first := &maskedBox{value: 1}
	runtime.AddCleanup(first, func(struct{}) { done <- struct{}{} }, struct{}{})

	second := &maskedBox{value: 2}
	runtime.AddCleanup(second, func(struct{}) {}, struct{}{})
}

// A finalizer alone releases the object, so finalizers themselves are not broken.
func registerFinalizerOnly(done chan struct{}) {
	box := &maskedBox{value: 1}
	runtime.SetFinalizer(box, func(*maskedBox) { done <- struct{}{} })
}

func main() {
	trailing := make(chan struct{}, 1)
	registerWithTrailingAllocation(trailing)
	waitForCleanup(trailing, "trailing allocation")

	keepAlive := make(chan struct{}, 1)
	registerWithKeepAlive(keepAlive)
	waitForCleanup(keepAlive, "keepalive")

	two := make(chan struct{}, 1)
	registerTwoObjects(two)
	waitForCleanup(two, "two objects")

	finalizer := make(chan struct{}, 1)
	registerFinalizerOnly(finalizer)
	waitForCleanup(finalizer, "finalizer only")
}
