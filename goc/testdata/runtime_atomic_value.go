package main

import (
	"sync/atomic"
)

type payload struct {
	value int
}

func main() {
	var value atomic.Value
	value.Store(&payload{value: 17})
	value.Store(&payload{value: 42})

	loaded := value.Load().(*payload)
	if loaded.value != 42 {
		panic("atomic.Value load result mismatch")
	}
}
