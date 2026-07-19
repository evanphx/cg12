package main

import "runtime"

type closureRootNode struct {
	value int
	next  *closureRootNode
}

func makeClosureRootChecker() func() int {
	values := make([]*closureRootNode, 0, 64)
	lookup := make(map[int]*closureRootNode)
	for index := 0; index < 64; index++ {
		node := &closureRootNode{
			value: index,
			next:  &closureRootNode{value: index * 2},
		}
		values = append(values, node)
		lookup[index] = node.next
	}

	return func() int {
		total := 0
		for index, node := range values {
			total += node.value
			total += lookup[index].value
		}
		return total
	}
}

func main() {
	check := makeClosureRootChecker()
	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}
	if check() != 6048 {
		panic("closure captured roots were corrupted")
	}
}
