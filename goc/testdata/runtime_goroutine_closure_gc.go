package main

import "runtime"

func run(callback func() int, done chan int) {
	runtime.GC()
	done <- callback()
}

func start(value int) int {
	done := make(chan int, 1)
	go run(func() int {
		return value + 2
	}, done)
	return <-done
}

func main() {
	total := 0
	for index := 0; index < 64; index++ {
		total += start(index)
	}

	if total != 2144 {
		panic("goroutine closure GC result mismatch")
	}
}
