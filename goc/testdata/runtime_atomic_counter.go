package main

import (
	"sync"
	"sync/atomic"
)

func main() {
	var wait sync.WaitGroup
	var counter int64

	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 128; iteration++ {
				atomic.AddInt64(&counter, 1)
			}
		}()
	}

	wait.Wait()

	if atomic.LoadInt64(&counter) != 2048 {
		panic("atomic counter result mismatch")
	}
}
