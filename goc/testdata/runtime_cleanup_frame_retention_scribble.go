// A regression test for the over-retention bug in RUNTIME_PLAN.md 5.3.
//
// NOTE: this file was originally committed as "the positive proof" that
// overwriting an abandoned frame is what releases the object. That reading was
// wrong, and the cg12scanroots diagnostic disproved it: this program still
// passes with the scribbleDeadStack call removed. The difference between it and
// runtime_cleanup_frame_retention.go is not the scribble at all -- it is that
// this file's helper is not inlined, so the box never enters main's frame,
// while in the minimal reducer registerRetainedCleanup IS inlined into main and
// the retained words are main's own locals. The real mechanism was
// gometa.FunctionStackMaps unioning the function-wide conservative map into
// every safepoint; see RUNTIME_PLAN.md 5.3.
//
// It is kept because it exercises a distinct shape (out-of-line registration
// plus an explicit dead-stack overwrite) and because deleting a passing GC
// reducer costs more than keeping it. Do not cite it as evidence for the
// stale-frame theory.
//
// This lives in its own file, at the frame depth where it was verified, because
// folding it in alongside other variants changes the frame layout. Keep it
// standalone.
package main

import (
	"runtime"
	"time"
	"unsafe"
)

type scribbledBox struct {
	value   int
	pointer unsafe.Pointer
}

var scribbleDone = make(chan struct{}, 1)

func registerScribbledCleanup() {
	box := &scribbledBox{value: 42}
	runtime.AddCleanup(box, func(struct{}) {
		scribbleDone <- struct{}{}
	}, struct{}{})
}

//go:noinline
func scribbleDeadStack(depth int) int {
	var pad [64]uintptr
	for index := range pad {
		pad[index] = 0xdeadbeef
	}
	if depth > 0 {
		pad[0] += uintptr(scribbleDeadStack(depth - 1))
	}
	return int(pad[0] & 1)
}

func main() {
	registerScribbledCleanup()
	_ = scribbleDeadStack(16)

	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-scribbleDone:
			return
		default:
		}
	}

	panic("cleanup did not run after scribbling the abandoned frame")
}
