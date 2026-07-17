package main

import (
	"bytes"
	"runtime"
	"testing"
)

func measuredAllocsPerRun(runs int, function func()) (average float64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	function()
	var statistics runtime.MemStats
	runtime.ReadMemStats(&statistics)
	before := statistics
	allocations := 0 - statistics.Mallocs
	startingGCs := statistics.NumGC
	for run := 0; run < runs; run++ {
		function()
	}
	runtime.ReadMemStats(&statistics)
	allocations += statistics.Mallocs
	println("measured delta", allocations, "quotient", allocations/uint64(runs), "GCs", statistics.NumGC-startingGCs)
	for index := range statistics.BySize {
		classAllocs := statistics.BySize[index].Mallocs - before.BySize[index].Mallocs
		if classAllocs != 0 {
			println("class", statistics.BySize[index].Size, "allocations", classAllocs)
		}
	}
	return float64(allocations / uint64(runs))
}

func runCase(startLength int, growLength int) {
	xBytes := bytes.Repeat([]byte{'x'}, startLength)
	buffer := bytes.NewBuffer(xBytes)
	temporary := make([]byte, 72)
	buffer.Read(temporary)
	yBytes := bytes.Repeat([]byte{'y'}, growLength)
	function := func() {
		buffer.Grow(growLength)
		buffer.Write(yBytes)
	}
	measured := measuredAllocsPerRun(100, function)
	standard := testing.AllocsPerRun(100, function)
	println("case", startLength, growLength, "measured", measured, "standard", standard)
}

func main() {
	runCase(0, 10000)
	runCase(100, 10000)
}
