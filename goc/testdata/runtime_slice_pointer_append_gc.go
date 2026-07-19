package main

import "runtime"

type record struct {
	value int
}

func main() {
	values := make([]*record, 0, 4)
	for index := 0; index < 128; index++ {
		values = append(values, &record{value: index})
	}

	runtime.GC()

	total := 0
	for index := 0; index < len(values); index += 9 {
		total += values[index].value
	}

	if total != 945 {
		panic("slice pointer append GC result mismatch")
	}
	runtime.KeepAlive(values)
}
