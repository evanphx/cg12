package main

import "runtime"

type appendVariadicNode struct {
	value int
	next  *appendVariadicNode
}

func main() {
	source := make([]*appendVariadicNode, 0, 80)
	for index := 0; index < 80; index++ {
		source = append(source, &appendVariadicNode{
			value: index,
			next:  &appendVariadicNode{value: index + 100},
		})
	}

	destination := make([]*appendVariadicNode, 0, 4)
	destination = append(destination, source...)
	source = nil

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for _, node := range destination {
		total += node.value
		total += node.next.value
	}
	if total != 14320 {
		panic("variadic append pointer elements were corrupted")
	}
}
