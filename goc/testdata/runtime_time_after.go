package main

import "time"

func main() {
	start := time.Now()
	<-time.After(time.Millisecond)
	if time.Since(start) < 0 {
		panic("time moved backwards")
	}
}
