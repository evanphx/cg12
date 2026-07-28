// The positive proof for the over-retention diagnosis in RUNTIME_PLAN.md 5.3.
//
// This is runtime_cleanup_frame_retention.go with one addition: before
// collecting, scribbleDeadStack overwrites the stack region that
// registerRetainedCleanup abandoned when it returned. That single change makes
// the object collectable and the cleanup runs, which is what attributes the
// retention to a stale pointer word in dead stack rather than to anything in
// mcleanup.go. Under GODEBUG=checkfinalizers=1 this program reports
// "queue: 0 finalizers + 1 cleanups" where the unscribbled one reports 0.
//
// This lives in its own file, at the frame depth where it was verified, because
// folding it in alongside other variants stops it reproducing -- the earlier
// calls change the stack it would have to overwrite. Keep it standalone.
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
