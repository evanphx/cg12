package main

import "runtime"

type appendGrowthNode struct {
	value int
	next  *appendGrowthNode
}

func main() {
	values := make([]*appendGrowthNode, 0, 1)
	for index := 0; index < 64; index++ {
		values = append(values, &appendGrowthNode{
			value: index,
			next:  &appendGrowthNode{value: 1},
		})
	}

	runtime.GC()
	total := 0
	for _, value := range values {
		total += value.value + value.next.value
	}
	if total != 2080 {
		panic("append growth pointer elements were corrupted")
	}
}
