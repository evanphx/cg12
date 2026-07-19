package main

import "sync"

func main() {
	var mutex sync.RWMutex
	var wait sync.WaitGroup
	results := make(chan int, 4)

	mutex.RLock()
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func(value int) {
			defer wait.Done()
			mutex.RLock()
			results <- value + 10
			mutex.RUnlock()
		}(index)
	}
	mutex.RUnlock()
	wait.Wait()
	close(results)

	total := 0
	for value := range results {
		total += value
	}
	if total != 46 {
		panic("RWMutex readers did not complete")
	}
}
