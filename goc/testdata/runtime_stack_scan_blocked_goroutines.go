// A goroutine that is parked is scanned by suspendG/scanstack rather than by
// the stop-the-world path, and its stack map is read at the PC of whatever call
// parked it. Every blocking primitive parks at a different call, so each one
// selects a different stack map, and a map that is wrong for the parking PC
// loses roots that no running goroutine holds.
//
// Every worker below holds the only reference to its objects in its own frame,
// blocks, and is left blocked while the main goroutine collects several times.
// Nothing else in the program can reach those objects: they are created inside
// the worker, are not stored anywhere, and the labels used to check them are
// rebuilt from scratch rather than captured.
//
// The parking sites covered are an unbuffered channel receive and send, a full
// buffered channel send, a select over two channels, a sync.Mutex, a
// sync.WaitGroup, and a sync.Cond -- which is to say chanrecv, chansend,
// selectgo, semacquire and notifyListWait.
//
// Run under GODEBUG=cg12scanroots=1: each blocked worker's frame must name its
// objects on every cycle, and the frame that stops naming them is the defect.
package main

import (
	"runtime"
	"sync"
)

type held struct {
	value int
	tail  *held
	name  string
}

//go:noinline
func makeHeld(seed int) *held {
	return &held{
		value: seed,
		name:  "held-" + string(rune('a'+seed%26)),
		tail:  &held{value: seed * 3, name: "tail"},
	}
}

//go:noinline
func expectedName(seed int) string {
	return "held-" + string(rune('a'+seed%26))
}

//go:noinline
func verify(where string, object *held, seed int) {
	if object == nil {
		println("nil object in", where)
		panic("a blocked goroutine's frame lost its object entirely")
	}
	if object.value != seed || object.name != expectedName(seed) {
		println("in", where, "value", object.value, "name", object.name, "want", seed)
		panic("a blocked goroutine's frame lost a root")
	}
	if object.tail == nil || object.tail.value != seed*3 || object.tail.name != "tail" {
		println("in", where, "tail lost")
		panic("a blocked goroutine's frame lost an indirect root")
	}
}

// churn allocates and drops garbage so that any object the collector wrongly
// reclaimed has its memory handed out again before it is checked.
//
//go:noinline
func churn() {
	var sink []*held
	for index := 0; index < 2048; index++ {
		sink = append(sink, &held{value: index, name: "churn"})
	}
	if len(sink) != 2048 {
		panic("churn lost its slice")
	}
}

//go:noinline
func collectWhileBlocked() {
	for cycle := 0; cycle < 3; cycle++ {
		runtime.GC()
		churn()
		runtime.GC()
	}
}

func main() {
	var started sync.WaitGroup
	var finished sync.WaitGroup

	unbufferedReceive := make(chan int)
	unbufferedSend := make(chan int)
	fullBuffer := make(chan int, 1)
	fullBuffer <- 0
	selectLeft := make(chan int)
	selectRight := make(chan int)

	var mutex sync.Mutex
	mutex.Lock()

	var gate sync.WaitGroup
	gate.Add(1)

	condLock := sync.Mutex{}
	condition := sync.NewCond(&condLock)
	condReady := false

	// chanrecv on an unbuffered channel.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(1)
		started.Done()
		<-unbufferedReceive
		verify("unbuffered receive", object, 1)
		finished.Done()
	}()

	// chansend on an unbuffered channel with no receiver yet.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(2)
		started.Done()
		unbufferedSend <- 2
		verify("unbuffered send", object, 2)
		finished.Done()
	}()

	// chansend on a buffered channel that is already full.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(3)
		started.Done()
		fullBuffer <- 3
		verify("full buffered send", object, 3)
		finished.Done()
	}()

	// selectgo over two channels, neither ready.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(4)
		started.Done()
		select {
		case <-selectLeft:
		case <-selectRight:
		}
		verify("select", object, 4)
		finished.Done()
	}()

	// semacquire through a contended mutex.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(5)
		started.Done()
		mutex.Lock()
		verify("mutex", object, 5)
		mutex.Unlock()
		finished.Done()
	}()

	// semacquire through WaitGroup.Wait.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(6)
		started.Done()
		gate.Wait()
		verify("waitgroup", object, 6)
		finished.Done()
	}()

	// notifyListWait through sync.Cond.
	started.Add(1)
	finished.Add(1)
	go func() {
		object := makeHeld(7)
		condLock.Lock()
		started.Done()
		for !condReady {
			condition.Wait()
		}
		condLock.Unlock()
		verify("cond", object, 7)
		finished.Done()
	}()

	started.Wait()
	collectWhileBlocked()

	// Release them one at a time, collecting between each, so that the
	// goroutines still blocked are scanned again after the stack layout around
	// them has changed.
	unbufferedReceive <- 1
	runtime.GC()
	if got := <-unbufferedSend; got != 2 {
		panic("unbuffered send delivered the wrong value")
	}
	runtime.GC()
	if got := <-fullBuffer; got != 0 {
		panic("full buffered channel delivered the wrong value")
	}
	runtime.GC()
	selectLeft <- 4
	runtime.GC()
	mutex.Unlock()
	runtime.GC()
	gate.Done()
	runtime.GC()
	condLock.Lock()
	condReady = true
	condition.Broadcast()
	condLock.Unlock()

	finished.Wait()
	if got := <-fullBuffer; got != 3 {
		panic("the released sender did not deliver its value")
	}
	println("blocked goroutine roots ok")
}
