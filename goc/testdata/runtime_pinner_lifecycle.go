package main

import (
	"runtime"
	"time"
)

type pinnedBox struct {
	value int
	pad   [32]byte
}

func pinBox(pinner *runtime.Pinner, done chan int, value int) {
	box := &pinnedBox{value: value}
	runtime.SetFinalizer(box, func(finalized *pinnedBox) {
		done <- finalized.value
	})
	pinner.Pin(box)
}

func collectUntil(done chan int, expected int) {
	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case value := <-done:
			if value != expected {
				panic("finalizer received the wrong pinned object")
			}
			return
		default:
		}
	}
	panic("unpinned object was not finalized")
}

func assertStillPinned(done chan int) {
	for cycle := 0; cycle < 32; cycle++ {
		runtime.GC()
		runtime.Gosched()
	}
	select {
	case <-done:
		panic("pinned object was finalized")
	default:
	}
}

func main() {
	done := make(chan int, 2)
	var pinner runtime.Pinner

	pinBox(&pinner, done, 41)
	assertStillPinned(done)
	pinner.Unpin()
	collectUntil(done, 41)

	pinBox(&pinner, done, 42)
	pinBox(&pinner, done, 42)
	assertStillPinned(done)
	pinner.Unpin()
	collectUntil(done, 42)
}
