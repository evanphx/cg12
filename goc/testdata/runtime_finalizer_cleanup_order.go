package main

import (
	"runtime"
	"time"
	"unsafe"
)

type finalizerCleanupBox struct {
	value   int
	pointer unsafe.Pointer
}

func registerFinalizerAndCleanup(events chan int) {
	box := &finalizerCleanupBox{value: 42}
	runtime.AddCleanup(box, func(value int) {
		if value != 42 {
			panic("cleanup received the wrong argument")
		}
		events <- 2
	}, box.value)
	runtime.SetFinalizer(box, func(value *finalizerCleanupBox) {
		if value.value != 42 {
			panic("finalizer received the wrong object")
		}
		events <- 1
	})
}

func waitForEvent(events chan int) int {
	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case event := <-events:
			return event
		default:
		}
	}
	panic("finalizer or cleanup did not run")
}

func main() {
	events := make(chan int, 2)
	registerFinalizerAndCleanup(events)

	if waitForEvent(events) != 1 {
		panic("cleanup ran before finalizer")
	}
	if waitForEvent(events) != 2 {
		panic("cleanup did not run after finalizer")
	}
}
