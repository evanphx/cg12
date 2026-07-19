package main

import "time"

func main() {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	count := 0
	deadline := time.After(200 * time.Millisecond)
	for count < 3 {
		select {
		case <-ticker.C:
			count++
		case <-deadline:
			panic("ticker did not deliver enough ticks")
		}
	}
}
