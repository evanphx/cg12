package main

import "time"

func main() {
	done := make(chan int, 1)
	timer := time.AfterFunc(time.Millisecond, func() {
		done <- 42
	})
	defer timer.Stop()

	select {
	case value := <-done:
		if value != 42 {
			panic("afterfunc value mismatch")
		}
	case <-time.After(200 * time.Millisecond):
		panic("afterfunc timeout")
	}
}
