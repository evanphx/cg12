package main

import (
	"bytes"
	"runtime"
)

func main() {
	buffer := bytes.NewBuffer(bytes.Repeat([]byte{'x'}, 100))
	contents := bytes.Repeat([]byte{'y'}, 10000)
	write := func() {
		buffer.Grow(len(contents))
		buffer.Write(contents)
	}
	write()

	var before runtime.MemStats
	var after runtime.MemStats
	runtime.ReadMemStats(&before)
	for iteration := 0; iteration < 100; iteration++ {
		write()
	}
	runtime.ReadMemStats(&after)

	println("mallocs", before.Mallocs, after.Mallocs, after.Mallocs-before.Mallocs)
	println("frees", before.Frees, after.Frees, after.Frees-before.Frees)
	println("objects", before.HeapObjects, after.HeapObjects)
	println("bytes", before.TotalAlloc, after.TotalAlloc, after.TotalAlloc-before.TotalAlloc)
	println("capacity", buffer.Cap())
}
