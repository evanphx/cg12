package main

import "sync"

func main() {
	var lock sync.RWMutex
	condition := sync.NewCond(&lock)
	ready := false
	total := 0

	done := make(chan int, 1)
	go func() {
		lock.Lock()
		for !ready {
			condition.Wait()
		}
		total += 17
		lock.Unlock()

		lock.RLock()
		value := total
		lock.RUnlock()
		done <- value + 25
	}()

	lock.Lock()
	ready = true
	condition.Signal()
	lock.Unlock()

	if value := <-done; value != 42 {
		panic("sync Cond/RWMutex result mismatch")
	}
}
