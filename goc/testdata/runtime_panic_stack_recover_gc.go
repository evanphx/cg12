package main

import "runtime"

type marker struct {
	value int
}

func descend(depth int, value *marker) {
	var scratch [80]uintptr
	scratch[0] = uintptr(depth)

	if depth == 0 {
		runtime.GC()
		panic(value)
	}

	descend(depth-1, value)
	runtime.KeepAlive(scratch)
}

func main() {
	value := &marker{value: 42}
	recovered := 0

	func() {
		defer func() {
			payload := recover().(*marker)
			recovered = payload.value
			runtime.GC()
		}()
		descend(384, value)
	}()

	if recovered != 42 {
		panic("stack panic recover GC result mismatch")
	}
	runtime.KeepAlive(value)
}
