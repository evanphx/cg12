package main

import "runtime"

func worker(done chan int) {
	defer func() {
		done <- 42
	}()
	runtime.Goexit()
	done <- 1
}

func main() {
	done := make(chan int, 1)
	go worker(done)

	if value := <-done; value != 42 {
		panic("runtime.Goexit did not run deferred cleanup")
	}
}
