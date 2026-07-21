package main

import (
	"runtime/debug"
	"strings"
)

func main() {
	stack := string(debug.Stack())
	if !strings.Contains(stack, "goroutine") {
		panic("debug Stack missing goroutine header")
	}

	var stats debug.GCStats
	debug.ReadGCStats(&stats)
	if stats.NumGC < 0 {
		panic("debug GCStats NumGC invalid")
	}

	oldLimit := debug.SetMemoryLimit(64 << 20)
	if oldLimit == 0 {
		panic("debug SetMemoryLimit returned zero")
	}
	debug.SetMemoryLimit(oldLimit)
}
