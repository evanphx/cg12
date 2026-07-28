// Minimal reproducer for the cleanup/finalizer over-retention bug described in
// RUNTIME_PLAN.md 5.3. Now fixed; kept as the regression test for it.
//
// registerRetainedCleanup returns, so nothing live refers to the box any more,
// yet the cleanup never ran. GODEBUG=checkfinalizers=1 reported
// "queue: 0 finalizers + 0 cleanups" on every cycle, confirming the object was
// never queued rather than queued and dropped.
//
// The original diagnosis -- a stale pointer word in the *abandoned* frame of a
// returned function -- was wrong. cg12 inlines registerRetainedCleanup into
// main, so there is no abandoned frame: the retained words are main's own
// locals, and main's frame lives for the whole collection loop. The actual bug
// was that gometa.FunctionStackMaps built each safepoint's map as the union of
// that safepoint's live roots with the function-wide conservative map, so every
// call reported every pointer the frame had ever held. The cg12scanroots
// GODEBUG diagnostic is what named the retaining frame.
//
// The bug is sensitive to register's frame layout, so keep this program exactly
// as it is: no extra statement after AddCleanup, and the collection loop inline
// in main rather than in a helper. Both perturbations mask it. See
// runtime_cleanup_frame_retention_masked.go for the shapes that do release, and
// treat this file as load-bearing -- editing it casually turns a real failure
// into a false pass.
package main

import (
	"runtime"
	"time"
	"unsafe"
)

type retainedBox struct {
	value   int
	pointer unsafe.Pointer
}

func registerRetainedCleanup(done chan struct{}) {
	box := &retainedBox{value: 42}
	runtime.AddCleanup(box, func(struct{}) {
		done <- struct{}{}
	}, struct{}{})
}

func main() {
	done := make(chan struct{}, 1)
	registerRetainedCleanup(done)

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

	panic("cleanup did not run")
}
