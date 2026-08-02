package main

import "runtime"

type bucket struct {
	name   string
	values []int
}

//go:noinline
func grow(depth int) int {
	var pad [512]int
	for index := range pad {
		pad[index] = depth + index
	}
	if depth == 0 {
		return pad[1]
	}
	return grow(depth-1) + pad[2]
}

// hold runs on an ordinary goroutine stack, which starts small and is copied to
// a larger one as it grows. main's stack may be the system stack, which never
// moves; this one certainly does.
func hold(done chan<- string) {
	buckets := map[string]*bucket{
		"left": {
			name:   "left",
			values: []int{7, 11},
		},
		"right": {
			name:   "right",
			values: []int{13},
		},
	}

	grow(400)
	runtime.GC()

	left := buckets["left"].values
	right := buckets["right"].values
	if left[0] == 7 && left[1] == 11 && right[0] == 13 {
		done <- "intact"
		return
	}
	done <- "CORRUPTED"
}

func main() {
	done := make(chan string, 1)
	go hold(done)
	println(<-done)
}
