// Minimal reproducer for the cleanup/finalizer over-retention bug described in
// RUNTIME_PLAN.md 5.3.
//
// register returns, so nothing live refers to b any more, yet a stale pointer
// word left in register's abandoned frame keeps the object reachable and the
// cleanup never runs. GODEBUG=checkfinalizers=1 reports "queue: 0 finalizers +
// 0 cleanups" on every cycle, confirming the object is never queued rather than
// queued and dropped.
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
