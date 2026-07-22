package main

import (
	"runtime"
	"time"
)

type resurrectedObject struct {
	value int
	next  *resurrectedObject
}

var resurrected *resurrectedObject

func main() {
	done := make(chan struct{}, 1)
	object := &resurrectedObject{
		value: 41,
		next:  &resurrectedObject{value: 1},
	}
	runtime.SetFinalizer(object, func(value *resurrectedObject) {
		resurrected = value
		done <- struct{}{}
	})
	object = nil

	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-done:
			if resurrected == nil {
				panic("finalizer published a nil object")
			}
			if resurrected.value+resurrected.next.value != 42 {
				panic("finalizer resurrected corrupted object")
			}
			return
		default:
		}
	}

	panic("finalizer did not run")
}
