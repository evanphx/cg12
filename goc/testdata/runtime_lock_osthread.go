package main

import "runtime"

func main() {
	done := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- 42
	}()

	if value := <-done; value != 42 {
		panic("LockOSThread goroutine result mismatch")
	}
}
