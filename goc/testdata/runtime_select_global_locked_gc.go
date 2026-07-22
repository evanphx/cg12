package main

import "runtime"

var enable = make(chan uint32)
var disable = make(chan uint32)
var acknowledged = make(chan struct{})

func signalMaskWorker(iterations int) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for range iterations {
		select {
		case value := <-enable:
			if value == 0 {
				panic("enable value was not delivered")
			}
		case value := <-disable:
			if value == 0 {
				panic("disable value was not delivered")
			}
		}
		acknowledged <- struct{}{}
	}
}

func main() {
	const iterations = 64
	go signalMaskWorker(iterations)
	for index := range iterations {
		if index%2 == 0 {
			enable <- uint32(index + 1)
		} else {
			disable <- uint32(index + 1)
		}
		<-acknowledged
		runtime.GC()
	}
}
