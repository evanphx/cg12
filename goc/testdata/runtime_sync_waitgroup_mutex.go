package main

import "sync"

func main() {
	var lock sync.Mutex
	var once sync.Once
	var wait sync.WaitGroup

	total := 0
	initialized := 0

	for index := 0; index < 6; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()

			once.Do(func() {
				initialized = 100
			})

			lock.Lock()
			total += index + 1
			lock.Unlock()
		}()
	}

	wait.Wait()

	if initialized != 100 || total != 21 {
		panic("sync WaitGroup/Mutex/Once result mismatch")
	}
}
