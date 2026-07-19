package main

import "runtime"

func main() {
	done := make(chan int, 1)
	value := 0

	go func() {
		for value == 0 {
			runtime.Gosched()
		}
		done <- value
	}()

	for attempt := 0; attempt < 8; attempt++ {
		runtime.Gosched()
	}
	value = 42
	runtime.Gosched()

	if <-done != 42 {
		panic("goroutine did not observe progress")
	}
}
