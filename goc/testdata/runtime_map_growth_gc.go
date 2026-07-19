package main

import "runtime"

type entry struct {
	key   int
	value int
	next  *entry
}

func main() {
	values := make(map[int]*entry)
	for index := 0; index < 192; index++ {
		values[index] = &entry{
			key:   index,
			value: index * 2,
			next:  &entry{value: index + 1},
		}
	}

	runtime.GC()

	total := 0
	for index := 0; index < 192; index += 17 {
		current := values[index]
		total += current.key
		total += current.value
		total += current.next.value
	}

	if total != 4500 {
		panic("map growth GC result mismatch")
	}
	runtime.KeepAlive(values)
}
