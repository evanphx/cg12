package main

import "time"

func main() {
	values := make(chan int)
	select {
	case <-values:
		panic("unready channel received before timeout")
	case <-time.After(time.Millisecond):
	}
}
