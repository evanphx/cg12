package main

import "time"

func main() {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	first := <-ticker.C
	second := <-ticker.C
	if second.Before(first) {
		panic("ticker moved backwards")
	}

	ticker.Stop()
	select {
	case <-ticker.C:
		panic("stopped ticker produced an immediate tick")
	default:
	}
}
