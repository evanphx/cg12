package main

import "sync"

func main() {
	var once sync.Once
	var wait sync.WaitGroup
	counter := 0

	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			once.Do(func() {
				counter++
			})
		}()
	}
	wait.Wait()

	if counter != 1 {
		panic("sync.Once function ran wrong number of times")
	}
}
