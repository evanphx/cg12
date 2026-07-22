package main

import (
	"runtime"
	"time"
)

type finalizerDependencyNode struct {
	value int
	next  *finalizerDependencyNode
}

func registerDependentFinalizers(events chan int) {
	child := &finalizerDependencyNode{value: 1}
	parent := &finalizerDependencyNode{
		value: 2,
		next:  child,
	}
	runtime.SetFinalizer(child, func(value *finalizerDependencyNode) {
		events <- value.value
	})
	runtime.SetFinalizer(parent, func(value *finalizerDependencyNode) {
		if value.next == nil || value.next.value != 1 {
			panic("parent finalizer lost its child")
		}
		events <- value.value
	})
}

func waitForFinalizer(events chan int) int {
	for attempt := 0; attempt < 1000; attempt++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case event := <-events:
			return event
		default:
		}
	}
	panic("dependent finalizer did not run")
}

func main() {
	events := make(chan int, 2)
	registerDependentFinalizers(events)

	if waitForFinalizer(events) != 2 {
		panic("child finalizer ran before parent finalizer")
	}
	if waitForFinalizer(events) != 1 {
		panic("child finalizer did not run after parent finalizer")
	}
}
