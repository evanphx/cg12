package main

import "runtime"

type slicePayload struct {
	value int
}

func main() {
	channel := make(chan []*slicePayload, 1)
	values := []*slicePayload{
		{value: 17},
		{value: 25},
	}
	channel <- values
	values = nil

	runtime.GC()
	received := <-channel
	if len(received) != 2 {
		panic("received slice length mismatch")
	}
	if received[0].value+received[1].value != 42 {
		panic("channel slice payload was corrupted")
	}
}
