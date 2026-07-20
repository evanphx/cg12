package main

import (
	"runtime"
	"weak"
)

type weakPayload struct {
	value int
}

func main() {
	var zero weak.Pointer[weakPayload]
	if zero.Value() != nil {
		panic("weak zero value mismatch")
	}

	payload := &weakPayload{value: 42}
	pointer := weak.Make(payload)
	if pointer.Value() != payload {
		panic("weak pointer value mismatch")
	}
	runtime.KeepAlive(payload)
}
