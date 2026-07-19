package main

import (
	"runtime"
	"sync"
)

type item struct {
	index int
	next  *item
}

func worker(index int, wait *sync.WaitGroup, results chan int) {
	defer wait.Done()

	root := &item{
		index: index,
		next:  &item{index: index + 1},
	}
	runtime.GC()
	results <- root.index + root.next.index
	runtime.KeepAlive(root)
}

func main() {
	const goroutines = 48

	var wait sync.WaitGroup
	results := make(chan int, goroutines)

	for index := 0; index < goroutines; index++ {
		wait.Add(1)
		go worker(index, &wait, results)
	}

	wait.Wait()
	close(results)

	total := 0
	for index := 0; index < goroutines; index++ {
		total += <-results
	}

	if total != 2304 {
		panic("many goroutines GC result mismatch")
	}
}
