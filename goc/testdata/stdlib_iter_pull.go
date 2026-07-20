package main

import "iter"

func count(limit int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for value := 0; value < limit; value++ {
			if !yield(value) {
				return
			}
		}
	}
}

func main() {
	next, stop := iter.Pull(count(4))
	defer stop()

	sum := 0
	for {
		value, ok := next()
		if !ok {
			break
		}
		sum += value
	}
	if sum != 6 {
		panic("iter Pull sum mismatch")
	}
}
