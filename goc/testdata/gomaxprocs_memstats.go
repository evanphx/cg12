package main

import "runtime"

func main() {
	previous := runtime.GOMAXPROCS(1)
	println("previous", previous)

	var statistics runtime.MemStats
	runtime.ReadMemStats(&statistics)
	println("mallocs", statistics.Mallocs)

	runtime.GOMAXPROCS(previous)
	println("ok")
}
