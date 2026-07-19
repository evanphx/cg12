package main

import "runtime"

type recursiveNode struct {
	value int
	next  *recursiveNode
	pad   [8]uintptr
}

func grow(depth int, head *recursiveNode) int {
	local := &recursiveNode{
		value: depth,
		next:  head,
	}
	if depth == 0 {
		runtime.GC()
		total := 0
		for node := local; node != nil; node = node.next {
			total += node.value
		}
		return total
	}
	return grow(depth-1, local)
}

func main() {
	total := grow(96, nil)
	if total != 4656 {
		panic("recursive stack chain was corrupted")
	}
}
