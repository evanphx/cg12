package main

import "runtime"

type globalRootNode struct {
	value int
	next  *globalRootNode
}

var globalRootSlice []*globalRootNode
var globalRootMap map[int]*globalRootNode
var globalRootInterface interface{}

func main() {
	globalRootMap = make(map[int]*globalRootNode)
	for index := 0; index < 64; index++ {
		node := &globalRootNode{
			value: index,
			next:  &globalRootNode{value: index * 3},
		}
		globalRootSlice = append(globalRootSlice, node)
		globalRootMap[index] = node.next
	}
	globalRootInterface = globalRootSlice[17]

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for index, node := range globalRootSlice {
		total += node.value
		total += globalRootMap[index].value
	}
	total += globalRootInterface.(*globalRootNode).next.value

	if total != 8115 {
		panic("global roots were corrupted by GC")
	}
}
