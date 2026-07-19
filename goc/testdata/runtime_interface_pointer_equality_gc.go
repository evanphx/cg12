package main

import "runtime"

type interfacePointerEqualityNode struct {
	value int
}

func main() {
	node := &interfacePointerEqualityNode{value: 42}
	var left interface{} = node
	var right interface{} = node

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	if left != right {
		panic("interfaces holding the same pointer were not equal")
	}
	if left.(*interfacePointerEqualityNode).value != 42 {
		panic("interface pointer payload was corrupted")
	}
}
