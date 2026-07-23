package main

import (
	"runtime"
	"time"
	"unsafe"
)

type multipleCleanupBox struct {
	value   int
	pointer unsafe.Pointer
}

func registerMultipleCleanups(done chan int) {
	box := &multipleCleanupBox{value: 42}
	cleanup := func(value int) {
		done <- value
	}
	runtime.AddCleanup(box, cleanup, 1)
	runtime.AddCleanup(box, cleanup, 2).Stop()
	runtime.AddCleanup(box, cleanup, 3)
}

func main() {
	done := make(chan int, 3)
	registerMultipleCleanups(done)

	seen := 0
	for attempt := 0; attempt < 1000 && seen != 2; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		draining := true
		for draining {
			select {
			case value := <-done:
				if value == 2 {
					panic("stopped cleanup ran")
				}
				if value != 1 && value != 3 {
					panic("cleanup received an unexpected argument")
				}
				seen++
			default:
				draining = false
			}
		}
	}

	if seen != 2 {
		panic("multiple cleanups did not run")
	}
}
