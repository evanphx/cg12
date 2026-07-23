package main

import (
	"runtime"
	"time"
)

var tinyFinalized chan struct{}

const tinyFinalizerCount = 4096
const tinyFinalizerMinimum = 16

func finishTinyObject(*byte) {
	tinyFinalized <- struct{}{}
}

func registerTinyFinalizers() {
	for index := 0; index < tinyFinalizerCount; index++ {
		value := new(byte)
		*value = byte(index)
		runtime.SetFinalizer(value, finishTinyObject)
	}
}

func main() {
	tinyFinalized = make(chan struct{}, tinyFinalizerCount)
	registerTinyFinalizers()

	finalized := 0
	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		for {
			select {
			case <-tinyFinalized:
				finalized++
				if finalized == tinyFinalizerMinimum {
					return
				}
			default:
				goto drained
			}
		}
	drained:
	}

	panic("too few tiny-object finalizers ran")
}
