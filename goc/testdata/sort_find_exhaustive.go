package main

import "sort"

func main() {
	got := findExhaustive()
	if got != 42 {
		println("got", got, "want", 42)
		panic("sort.Find result mismatch")
	}
}

func findExhaustive() int {
	for size := 0; size <= 100; size++ {
		for target := 1; target <= size*2+1; target++ {
			compare := func(index int) int {
				return target - (index+1)*2
			}
			position, found := sort.Find(size, compare)
			wantPosition := target / 2
			wantFound := false
			if target%2 == 0 {
				wantPosition--
				wantFound = true
			}
			if position != wantPosition || found != wantFound {
				return 1
			}
		}
	}
	return 42
}
