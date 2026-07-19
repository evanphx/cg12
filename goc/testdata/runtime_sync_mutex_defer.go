package main

import "sync"

func main() {
	var mutex sync.Mutex
	total := 0

	for index := 0; index < 5; index++ {
		func(value int) {
			mutex.Lock()
			defer mutex.Unlock()
			total += value
		}(index)
	}

	if total != 10 {
		panic("sync mutex with deferred unlock produced wrong total")
	}
}
