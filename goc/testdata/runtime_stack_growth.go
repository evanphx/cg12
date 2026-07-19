package main

import "runtime"

type payload struct {
	value int
	next  *payload
}

func descend(depth int, root *payload) int {
	var scratch [96]uintptr
	scratch[0] = uintptr(depth)
	scratch[95] = uintptr(root.value)

	if depth == 0 {
		runtime.GC()
		return root.value + root.next.value + int(scratch[0])
	}

	return descend(depth-1, root) + int(scratch[0]&1)
}

func main() {
	root := &payload{
		value: 17,
		next:  &payload{value: 25},
	}

	got := descend(512, root)
	if got != 298 {
		panic("stack growth preserved the wrong live values")
	}
	runtime.KeepAlive(root)
}
