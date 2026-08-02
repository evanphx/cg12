package main

import "unsafe"

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

func hold(done chan<- string) {
	var anchor [2]int
	before := uintptr(unsafe.Pointer(&anchor))
	grow(400)
	after := uintptr(unsafe.Pointer(&anchor))
	if before == after {
		done <- "STACK DID NOT MOVE"
		return
	}
	done <- "stack moved"
}

func main() {
	done := make(chan string, 1)
	go hold(done)
	println(<-done)
}
