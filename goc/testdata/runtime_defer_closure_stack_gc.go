package main

import "runtime"

type deferredNode struct {
	value int
	next  *deferredNode
}

func recurse(depth int, head *deferredNode) (total int) {
	local := &deferredNode{
		value: depth,
		next:  head,
	}
	defer func() {
		runtime.GC()
		if local.value == depth {
			total++
		}
	}()
	if depth == 0 {
		return 0
	}
	return recurse(depth-1, local)
}

func main() {
	if recurse(64, nil) != 65 {
		panic("deferred closure stack pointer was corrupted")
	}
}
