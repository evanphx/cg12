package main

import "sync"

func callOnce(once *sync.Once, counter *int) {
	defer func() {
		_ = recover()
	}()
	once.Do(func() {
		*counter++
		panic("once panic")
	})
}

func main() {
	var once sync.Once
	counter := 0
	callOnce(&once, &counter)
	callOnce(&once, &counter)
	if counter != 1 {
		panic("sync.Once did not preserve panic completion state")
	}
}
