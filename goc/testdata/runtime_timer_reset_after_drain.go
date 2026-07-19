package main

import "time"

func main() {
	timer := time.NewTimer(time.Millisecond)
	<-timer.C

	timer.Reset(time.Millisecond)
	<-timer.C

	timer.Reset(time.Millisecond)
	select {
	case <-timer.C:
	case <-time.After(200 * time.Millisecond):
		panic("timer did not fire after reset from drained state")
	}
}
