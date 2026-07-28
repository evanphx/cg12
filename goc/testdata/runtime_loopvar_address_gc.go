package main

import (
	"fmt"
	"runtime"
)

type counter struct {
	value int
	label string
}

// Taking the address of a loop variable is the other way a per-iteration
// instance becomes observable: each iteration must yield a distinct address,
// and each of those cells must be a GC root the collector keeps alive.
func main() {
	var pointers []*int
	for index := 0; index < 4; index++ {
		pointers = append(pointers, &index)
	}
	for attempt := 0; attempt < 32; attempt++ {
		runtime.GC()
	}
	for index, pointer := range pointers {
		if *pointer != index {
			panic(fmt.Sprint("loop variable address ", index, " observed ", *pointer))
		}
		for other := index + 1; other < len(pointers); other++ {
			if pointer == pointers[other] {
				panic("two iterations produced the same loop variable address")
			}
		}
	}

	var values []*string
	for _, letter := range []string{"a", "b", "c"} {
		values = append(values, &letter)
	}
	for attempt := 0; attempt < 32; attempt++ {
		runtime.GC()
	}
	if *values[0]+*values[1]+*values[2] != "abc" {
		panic("range value addresses collapsed onto one cell")
	}

	// An aggregate loop variable keeps stable backing storage per iteration.
	var counters []*counter
	for item := (counter{value: 0, label: "zero"}); item.value < 3; item.value++ {
		item.label = fmt.Sprint("n", item.value)
		counters = append(counters, &item)
	}
	for attempt := 0; attempt < 32; attempt++ {
		runtime.GC()
	}
	for index, item := range counters {
		if item.value != index || item.label != fmt.Sprint("n", index) {
			panic(fmt.Sprint("aggregate loop variable ", index, " observed ", item.value, item.label))
		}
	}

	// Closures created in a loop and retained across collections keep their own
	// captured cell alive.
	var closures []func() string
	for index := 0; index < 8; index++ {
		text := fmt.Sprint("closure-", index)
		closures = append(closures, func() string { return text + "-" + fmt.Sprint(index) })
	}
	for attempt := 0; attempt < 32; attempt++ {
		runtime.GC()
	}
	for index, closure := range closures {
		want := fmt.Sprint("closure-", index, "-", index)
		if got := closure(); got != want {
			panic("closure capture corrupted: " + got + " want " + want)
		}
	}
}
