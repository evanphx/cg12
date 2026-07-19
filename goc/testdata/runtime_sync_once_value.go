package main

import "sync"

func main() {
	calls := 0
	once := sync.OnceValue(func() int {
		calls++
		return 42
	})

	if once() != 42 || once() != 42 {
		panic("OnceValue returned wrong value")
	}
	if calls != 1 {
		panic("OnceValue called function more than once")
	}
}
