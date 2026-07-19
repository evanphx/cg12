package main

import "runtime"

type payload struct {
	value int
	next  *payload
}

func main() {
	values := make(map[int]*payload)
	for index := 0; index < 80; index++ {
		values[index] = &payload{
			value: index,
			next:  &payload{value: index * 2},
		}
	}

	for index := 0; index < 80; index += 3 {
		delete(values, index)
	}

	runtime.GC()

	total := 0
	count := 0
	for key, value := range values {
		total += key
		total += value.value
		total += value.next.value
		count++
	}

	if count != 53 || total != 8428 {
		panic("map delete/range GC result mismatch")
	}
	runtime.KeepAlive(values)
}
