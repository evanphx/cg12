package main

import "runtime"

type resurrectedObject struct {
	value int
	next  *resurrectedObject
}

var resurrected *resurrectedObject

func main() {
	object := &resurrectedObject{
		value: 41,
		next:  &resurrectedObject{value: 1},
	}
	runtime.SetFinalizer(object, func(value *resurrectedObject) {
		resurrected = value
	})
	object = nil

	for attempt := 0; attempt < 128 && resurrected == nil; attempt++ {
		runtime.GC()
		runtime.Gosched()
	}

	if resurrected == nil {
		panic("finalizer did not run")
	}
	if resurrected.value+resurrected.next.value != 42 {
		panic("finalizer resurrected corrupted object")
	}
}
