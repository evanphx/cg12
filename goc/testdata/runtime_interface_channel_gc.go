package main

import "runtime"

type payload struct {
	value int
	next  *payload
}

func send(values chan any) {
	root := &payload{
		value: 17,
		next:  &payload{value: 25},
	}
	runtime.GC()
	values <- root
	runtime.KeepAlive(root)
}

func main() {
	values := make(chan any, 1)
	go send(values)

	runtime.GC()
	received := (<-values).(*payload)
	if received.value+received.next.value != 42 {
		panic("interface channel GC result mismatch")
	}
	runtime.KeepAlive(received)
}
