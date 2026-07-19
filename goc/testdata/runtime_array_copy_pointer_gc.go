package main

import "runtime"

type arrayCopyBox struct {
	value int
}

func main() {
	source := [4]*arrayCopyBox{
		{value: 3},
		{value: 5},
		{value: 7},
		{value: 11},
	}
	copy := source
	source = [4]*arrayCopyBox{}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for _, box := range copy {
		total += box.value
	}
	if total != 26 {
		panic("array pointer copy corrupted across GC")
	}
}
