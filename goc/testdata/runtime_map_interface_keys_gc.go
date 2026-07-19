package main

import "runtime"

type key struct {
	name string
	id   int
}

func main() {
	values := make(map[any]int)
	for index := 0; index < 32; index++ {
		values[key{name: "key", id: index}] = index * 3
	}

	runtime.GC()

	total := 0
	for index := 0; index < 32; index += 5 {
		total += values[key{name: "key", id: index}]
	}

	if total != 315 {
		panic("map interface keys GC result mismatch")
	}
	runtime.KeepAlive(values)
}
