package main

import (
	"runtime"
	"time"
)

type keepAliveBox struct {
	value int
	pad   [64]byte
}

func main() {
	done := make(chan int, 1)
	allocateAndKeep(done)

	for attempt := 0; attempt < 1000; attempt++ {
		_ = make([]byte, 1024)
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case value := <-done:
			if value != 42 {
				panic("finalizer observed wrong value")
			}
			return
		default:
		}
	}

	panic("finalizer did not run after object became unreachable")
}

//go:noinline
func allocateAndKeep(done chan int) {
	box := &keepAliveBox{value: 42}
	runtime.SetFinalizer(box, func(item *keepAliveBox) {
		done <- item.value
	})

	for attempt := 0; attempt < 16; attempt++ {
		runtime.GC()
		runtime.Gosched()
		select {
		case <-done:
			panic("finalizer ran before KeepAlive")
		default:
		}
	}

	runtime.KeepAlive(box)
	box = nil
}
