package main

import "time"

func main() {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		panic("fresh timer stopped after firing")
	}

	timer.Reset(time.Millisecond)
	<-timer.C

	timer.Reset(time.Millisecond)
	<-timer.C
}
