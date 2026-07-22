package main

import (
	"runtime"
	"time"
)

var tinyFinalized chan struct{}

func finishTinyObject(*byte) {
	tinyFinalized <- struct{}{}
}

func registerTinyFinalizers() {
	for index := 0; index < 4096; index++ {
		value := new(byte)
		*value = byte(index)
		runtime.SetFinalizer(value, finishTinyObject)
	}
}

func main() {
	tinyFinalized = make(chan struct{}, 4096)
	registerTinyFinalizers()

	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-tinyFinalized:
			return
		default:
		}
	}

	panic("no tiny-object finalizer ran")
}
