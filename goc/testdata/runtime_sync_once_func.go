package main

import "sync"

func main() {
	value := 0
	run := sync.OnceFunc(func() {
		value = 42
	})
	run()
	run()
	if value != 42 {
		panic("sync.OnceFunc did not share closure capture")
	}
}
