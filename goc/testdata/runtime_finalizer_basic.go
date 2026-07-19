package main

import (
	"runtime"
	"time"
)

type finalizable struct {
	value   int
	second  int
	padding [32]byte
}

func main() {
	done := make(chan int, 1)

	func() {
		value := &finalizable{
			value:  42,
			second: 42,
		}
		runtime.SetFinalizer(value, func(item *finalizable) {
			done <- item.value + item.second
		})
	}()

	for attempt := 0; attempt < 64; attempt++ {
		_ = make([]byte, 4096)
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case value := <-done:
			if value != 84 {
				panic("finalizer received the wrong value")
			}
			return
		default:
		}
	}

	panic("finalizer did not run")
}
