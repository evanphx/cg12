package main

import (
	"runtime"
	"time"
)

func main() {
	timers := make([]*time.Timer, 0, 8)
	for index := 0; index < 8; index++ {
		timers = append(timers, time.NewTimer(time.Millisecond))
	}

	for attempt := 0; attempt < 16; attempt++ {
		runtime.GC()
	}

	fired := 0
	for _, timer := range timers {
		<-timer.C
		fired++
	}
	if fired != 8 {
		panic("timers did not survive GC")
	}
}
