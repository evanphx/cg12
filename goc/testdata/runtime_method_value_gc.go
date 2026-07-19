package main

import "runtime"

type methodValueCounter struct {
	value int
	pad   [16]uintptr
}

func (counter *methodValueCounter) add(delta int) int {
	counter.value += delta
	return counter.value
}

func main() {
	counter := &methodValueCounter{value: 17}
	call := counter.add
	counter = nil

	runtime.GC()
	if call(25) != 42 {
		panic("method value receiver was not preserved")
	}
}
