package main

import "runtime"

type sliceStructValue struct {
	value int
	next  *sliceStructValue
}

func main() {
	values := make([]sliceStructValue, 0, 80)
	for index := 0; index < 80; index++ {
		values = append(values, sliceStructValue{
			value: index,
			next:  &sliceStructValue{value: index * 2},
		})
	}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for index := range values {
		total += values[index].value
		total += values[index].next.value
	}
	if total != 9480 {
		panic("slice struct pointer fields were corrupted by GC")
	}
}
