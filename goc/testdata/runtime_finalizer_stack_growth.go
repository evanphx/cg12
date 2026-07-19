package main

import (
	"runtime"
	"time"
)

type finalizerStackBox struct {
	value int
	next  *finalizerStackBox
}

func main() {
	done := make(chan int, 1)
	allocateFinalizerAfterStackGrowth(done, 96, nil)

	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case value := <-done:
			if value != 1 {
				panic("finalizer saw wrong stack-grown value")
			}
			return
		default:
		}
	}

	panic("stack-growth finalizer did not run")
}

//go:noinline
func allocateFinalizerAfterStackGrowth(done chan int, depth int, chain *finalizerStackBox) {
	node := &finalizerStackBox{
		value: depth,
		next:  chain,
	}
	if depth == 0 {
		runtime.SetFinalizer(chain, func(item *finalizerStackBox) {
			done <- item.value
		})
		runtime.KeepAlive(node)
		return
	}
	allocateFinalizerAfterStackGrowth(done, depth-1, node)
	runtime.KeepAlive(node)
}
