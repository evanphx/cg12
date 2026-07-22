package main

import (
	"runtime"
	"time"
	"unsafe"
)

type cleanupStopBox struct {
	value   int
	pointer unsafe.Pointer
}

func registerStoppedCleanup(stopped chan struct{}, active chan struct{}) {
	stoppedBox := &cleanupStopBox{value: 1}
	cleanup := runtime.AddCleanup(stoppedBox, func(struct{}) {
		stopped <- struct{}{}
	}, struct{}{})
	cleanup.Stop()

	activeBox := &cleanupStopBox{value: 2}
	runtime.AddCleanup(activeBox, func(struct{}) {
		active <- struct{}{}
	}, struct{}{})
}

func main() {
	stopped := make(chan struct{}, 1)
	active := make(chan struct{}, 1)
	registerStoppedCleanup(stopped, active)

	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-stopped:
			panic("stopped cleanup ran")
		case <-active:
			for extraCycle := 0; extraCycle < 16; extraCycle++ {
				runtime.GC()
				runtime.Gosched()
			}
			select {
			case <-stopped:
				panic("stopped cleanup ran")
			default:
			}
			return
		default:
		}
	}

	panic("active cleanup did not run")
}
