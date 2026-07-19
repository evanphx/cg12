package main

import "runtime"

type newCompositeNode struct {
	next  *newCompositeNode
	name  string
	items []int
}

func main() {
	head := new(newCompositeNode)
	head.name = "head"
	head.items = append(head.items, 3, 5, 7)

	head.next = &newCompositeNode{
		name:  "tail",
		items: []int{11, 13},
	}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := len(head.name) + len(head.next.name)
	for _, value := range head.items {
		total += value
	}
	for _, value := range head.next.items {
		total += value
	}
	if total != 47 {
		panic("new composite graph corrupted across GC")
	}
}
