package main

import (
	"runtime"
	"time"
	"unsafe"
)

type cleanupBox struct {
	value   int
	pointer unsafe.Pointer
}

func registerCleanup(done chan int) {
	box := &cleanupBox{value: 42}
	runtime.AddCleanup(box, func(value int) {
		done <- value
	}, box.value)
}

func main() {
	done := make(chan int, 1)
	registerCleanup(done)

	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case value := <-done:
			if value != 42 {
				panic("cleanup received the wrong argument")
			}
			return
		default:
		}
	}

	panic("cleanup did not run")
}
