package main

import "time"

func main() {
	timer := time.NewTimer(time.Millisecond)
	<-timer.C

	deadline := time.After(time.Millisecond)
	<-deadline
}
