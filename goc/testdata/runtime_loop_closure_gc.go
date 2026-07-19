package main

import "runtime"

func main() {
	functions := make([]func() int, 0, 8)
	for index := 0; index < 8; index++ {
		value := index * index
		functions = append(functions, func() int {
			return value
		})
	}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for _, function := range functions {
		total += function()
	}
	if total != 140 {
		panic("loop closure values corrupted")
	}
}
