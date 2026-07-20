package main

import "math/rand/v2"

func main() {
	random := rand.New(rand.NewPCG(1, 2))
	first := random.Uint64()
	second := random.Uint64()
	if first == second {
		panic("rand/v2 repeated Uint64")
	}

	value := random.IntN(100)
	if value < 0 || value >= 100 {
		panic("rand/v2 bounded value mismatch")
	}
}
