package main

import "time"

func main() {
	never := make(chan int)
	total := 0

	for iteration := 0; iteration < 24; iteration++ {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-never:
			panic("nil progress channel became ready")
		case <-timer.C:
			total += iteration
		}

		timer.Reset(time.Millisecond)
		select {
		case <-never:
			panic("nil progress channel became ready after reset")
		case <-timer.C:
			total += iteration * 2
		}
		timer.Stop()
	}

	if total != 828 {
		panic("timer select churn produced wrong total")
	}
}
