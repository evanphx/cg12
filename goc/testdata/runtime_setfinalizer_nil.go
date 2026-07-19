package main

import (
	"runtime"
	"time"
)

type clearedFinalizerBox struct {
	value int
}

func main() {
	done := make(chan int, 1)
	box := &clearedFinalizerBox{value: 42}
	runtime.SetFinalizer(box, func(item *clearedFinalizerBox) {
		done <- item.value
	})
	runtime.SetFinalizer(box, nil)
	box = nil

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-done:
			panic("cleared finalizer ran")
		default:
		}
	}
}
