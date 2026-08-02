package main

import "runtime"

type holder struct {
	data []byte
}

var kept *holder

// stash converts a string to []byte and stores the result in an object that
// outlives the call. goc gives runtime.stringtoslicebyte a 32-byte stack
// buffer when its escape analysis says the conversion does not escape, and the
// text here is 16 bytes, so it fits in that buffer.
//
//go:noinline
func stash(text string) {
	kept = &holder{data: []byte(text)}
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
	stash("0123456789abcdef")
	scribble(48)
	for round := 0; round < 8; round++ {
		runtime.GC()
	}
	scribble(48)
	println(string(kept.data))
}
