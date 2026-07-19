package main

import "runtime"

func capture(depth int) int {
	var pcs [16]uintptr
	count := runtime.Callers(depth, pcs[:])
	if count < 2 {
		panic("runtime.Callers returned too few frames")
	}
	if pcs[0] == 0 || pcs[1] == 0 {
		panic("runtime.Callers returned a zero program counter")
	}
	return count
}

func wrapper() int {
	return capture(0)
}

func main() {
	count := wrapper()
	if count < 2 {
		panic("runtime.Callers wrapper result mismatch")
	}
}
