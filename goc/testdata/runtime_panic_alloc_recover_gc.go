package main

import "runtime"

type panicAllocNode struct {
	value int
	next  *panicAllocNode
}

func explode() {
	node := &panicAllocNode{
		value: 19,
		next:  &panicAllocNode{value: 23},
	}
	runtime.GC()
	panic(node)
}

func main() {
	defer func() {
		value := recover()
		node, ok := value.(*panicAllocNode)
		if !ok {
			panic("recovered wrong panic payload type")
		}
		runtime.GC()
		if node.value+node.next.value != 42 {
			panic("panic payload was corrupted across recover GC")
		}
	}()
	explode()
}
