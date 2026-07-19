package main

import "runtime"

type marker struct {
	value int
}

func trigger(depth int, retained *marker) {
	var scratch [96]uintptr
	scratch[0] = uintptr(depth)
	scratch[1] = uintptr(retained.value)

	if depth == 0 {
		panic("intentional panic")
	}

	trigger(depth-1, retained)
	runtime.KeepAlive(scratch)
}

func main() {
	retained := &marker{value: 42}
	recovered := false

	defer func() {
		if recover() == nil {
			panic("missing panic")
		}
		recovered = true
		runtime.GC()
		if retained.value != 42 {
			panic("panic recovery corrupted a live heap value")
		}
	}()

	trigger(256, retained)
	if !recovered {
		panic("unreachable")
	}
}
