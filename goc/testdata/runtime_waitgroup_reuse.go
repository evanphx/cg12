package main

import "sync"

func runWaitGroupRound(round int) int {
	var wait sync.WaitGroup
	results := make(chan int, 4)
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func(value int) {
			defer wait.Done()
			results <- round*10 + value
		}(index)
	}
	wait.Wait()
	close(results)

	total := 0
	for value := range results {
		total += value
	}
	return total
}

func main() {
	if runWaitGroupRound(1) != 46 {
		panic("first WaitGroup round failed")
	}
	if runWaitGroupRound(2) != 86 {
		panic("second WaitGroup round failed")
	}
}
