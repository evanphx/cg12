package main

import "runtime"

type evaluator interface {
	Evaluate() int
}

type node struct {
	left  int
	right int
}

func (item *node) Evaluate() int {
	return item.left + item.right
}

func recurse(depth int, value evaluator) int {
	var scratch [64]uintptr
	scratch[0] = uintptr(depth)

	if depth == 0 {
		runtime.GC()
		return value.Evaluate()
	}

	return recurse(depth-1, value) + int(scratch[0]&1)
}

func main() {
	value := &node{
		left:  17,
		right: 25,
	}

	got := recurse(255, value)
	if got != 170 {
		panic("interface stack GC result mismatch")
	}
	runtime.KeepAlive(value)
}
