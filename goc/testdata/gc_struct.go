package main

import "runtime"

type record struct {
	id     int
	weight int
	left   *record
	right  *record
}

var garbage *record

func allocateGarbage() {
	for i := 0; i < 100000; i++ {
		garbage = &record{
			id:     i,
			weight: i * 3,
		}
	}
	garbage = nil
}

func Test() int {
	root := new(record)
	root.id = 7
	root.weight = 11
	root.left = &record{id: 13, weight: 17}
	root.right = &record{id: 19, weight: 23}

	allocateGarbage()
	runtime.GC()

	result := root.id * root.weight
	result += root.left.id * root.left.weight
	result += root.right.id * root.right.weight
	runtime.KeepAlive(root)
	return result
}

func main() {
	if Test() != 735 {
		panic("garbage collector corrupted a live object")
	}
}
