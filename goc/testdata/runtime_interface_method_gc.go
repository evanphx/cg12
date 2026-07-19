package main

import "runtime"

type scorer interface {
	score() int
}

type scoreBox struct {
	value int
	next  *scoreBox
}

func (box *scoreBox) score() int {
	total := 0
	for current := box; current != nil; current = current.next {
		total += current.value
	}
	return total
}

func main() {
	value := scorer(&scoreBox{
		value: 17,
		next:  &scoreBox{value: 25},
	})

	runtime.GC()
	if value.score() != 42 {
		panic("interface method receiver was not preserved")
	}
}
