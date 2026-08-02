package main

import "runtime"

type bucket struct {
	name   string
	values []int
}

var kept map[string]*bucket

// build is runtime_core_types.go's shape moved into a callee. In the corpus
// program the same map is built in main, whose frame outlives everything, so a
// backing array left in that frame is never observably dead. Here build
// returns first.
//
// cmd/compile heap-allocates both []int{...} literals. goc's census records no
// allocation for either, and opt.FrameEscapes records their addresses being
// stored into memory reached through runtime.newobject.
//
//go:noinline
func build() {
	kept = map[string]*bucket{
		"left": {
			name:   "left",
			values: []int{7, 11},
		},
		"right": {
			name:   "right",
			values: []int{13},
		},
	}
}

//go:noinline
func scribble(depth int) int {
	var pad [256]int
	for index := range pad {
		pad[index] = depth*1000 + index + 1
	}
	if depth == 0 {
		return pad[3]
	}
	return scribble(depth-1) + pad[7]
}

func main() {
	build()
	scribble(48)
	for round := 0; round < 8; round++ {
		runtime.GC()
	}
	scribble(48)
	println(kept["left"].values[0], kept["left"].values[1], kept["right"].values[0])
}
