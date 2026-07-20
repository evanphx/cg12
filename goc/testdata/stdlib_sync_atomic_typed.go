package main

import "sync/atomic"

type atomicPayload struct {
	value int
}

func main() {
	var counter atomic.Int64
	counter.Store(10)
	if counter.Add(5) != 15 {
		panic("atomic.Int64 Add mismatch")
	}
	if !counter.CompareAndSwap(15, 42) {
		panic("atomic.Int64 CompareAndSwap failed")
	}
	if counter.Load() != 42 {
		panic("atomic.Int64 Load mismatch")
	}

	first := &atomicPayload{value: 17}
	second := &atomicPayload{value: 25}
	var pointer atomic.Pointer[atomicPayload]
	pointer.Store(first)
	if pointer.Load().value != 17 {
		panic("atomic.Pointer Load mismatch")
	}
	if !pointer.CompareAndSwap(first, second) {
		panic("atomic.Pointer CompareAndSwap failed")
	}
	if pointer.Load().value != 25 {
		panic("atomic.Pointer swapped value mismatch")
	}
}
