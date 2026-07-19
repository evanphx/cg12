package main

import "runtime"

type mapPointerKey struct {
	value int
}

func main() {
	first := &mapPointerKey{value: 17}
	second := &mapPointerKey{value: 25}
	values := map[*mapPointerKey]int{
		first:  first.value,
		second: second.value,
	}

	for attempt := 0; attempt < 32; attempt++ {
		runtime.GC()
	}

	if values[first] != 17 {
		panic("first pointer key lookup failed")
	}
	if values[second] != 25 {
		panic("second pointer key lookup failed")
	}
	if values[&mapPointerKey{value: 17}] != 0 {
		panic("distinct pointer key matched existing key")
	}
}
