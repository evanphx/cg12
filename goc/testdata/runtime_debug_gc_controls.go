package main

import (
	"runtime"
	"runtime/debug"
)

func main() {
	previousPercent := debug.SetGCPercent(25)
	debug.SetGCPercent(previousPercent)

	previousLimit := debug.SetMemoryLimit(64 << 20)
	debug.SetMemoryLimit(previousLimit)

	values := make([]*int, 0, 128)
	for index := 0; index < 128; index++ {
		value := index
		values = append(values, &value)
	}

	runtime.GC()
	debug.FreeOSMemory()

	total := 0
	for index := 0; index < len(values); index += 17 {
		total += *values[index]
	}

	if total != 476 {
		panic("debug GC controls result mismatch")
	}
	runtime.KeepAlive(values)
}
