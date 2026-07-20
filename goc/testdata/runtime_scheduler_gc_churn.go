package main

import (
	"runtime"
	"sync"
)

type schedulerGCNode struct {
	value int
	next  *schedulerGCNode
}

func main() {
	const workers = 8
	const rounds = 16

	results := make(chan int, workers)
	var wait sync.WaitGroup
	wait.Add(workers)

	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer wait.Done()
			total := 0
			for round := 0; round < rounds; round++ {
				head := &schedulerGCNode{
					value: worker + round,
					next:  &schedulerGCNode{value: round},
				}
				runtime.Gosched()
				runtime.GC()
				total += head.value + head.next.value
			}
			results <- total
		}()
	}

	wait.Wait()
	close(results)

	total := 0
	for value := range results {
		total += value
	}
	if total != 2368 {
		panic("scheduler GC churn produced wrong total")
	}
}
