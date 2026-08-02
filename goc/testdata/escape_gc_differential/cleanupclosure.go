package main

import "runtime"

type payload struct {
	value int
}

var observed []string

// register is runtime_stack_argument_roots.go:41's shape: a function literal
// handed to runtime.AddCleanup, which runs it after the object dies -- long
// after register's frame is gone. cmd/compile says the literal escapes to the
// heap. goc's census records no allocation for it, and goc has a frame path
// for closures (gen.localAlloc), so this asks whether that path can be reached
// with a closure the runtime keeps.
//
//go:noinline
func register(label string) *payload {
	object := &payload{value: len(label)}
	runtime.AddCleanup(object, func(name string) {
		observed = append(observed, name)
	}, label)
	return object
}

//go:noinline
func scribble(depth int) int {
	var pad [256]int
	for index := range pad {
		pad[index] = depth*1000 + index + 1
	}
	if depth == 0 {
		return pad[3]
	}
	return scribble(depth-1) + pad[7]
}

func main() {
	object := register("cleanup-ran")
	println(object.value)
	object = nil
	scribble(48)
	for round := 0; round < 8; round++ {
		runtime.GC()
	}
	scribble(48)
	for round := 0; round < 8; round++ {
		runtime.GC()
	}
	if len(observed) == 0 {
		println("NO CLEANUP OBSERVED")
	} else {
		println(observed[0])
	}
}
