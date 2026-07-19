package main

import "runtime"

type mapPointerBox struct {
	value int
	next  *mapPointerBox
}

func main() {
	values := make(map[int]*mapPointerBox)
	for index := 0; index < 128; index++ {
		values[index] = &mapPointerBox{
			value: index,
			next:  &mapPointerBox{value: index * 2},
		}
	}
	for index := 0; index < 128; index += 3 {
		delete(values, index)
	}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for key, box := range values {
		total += key
		total += box.value
		total += box.next.value
	}
	if total != 21676 {
		panic("map pointer values corrupted across GC")
	}
}
