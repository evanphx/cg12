package main

import (
	"math/rand"
	"slices"
)

func main() {
	random := rand.New(rand.NewSource(12345))
	values := random.Perm(8)
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	if !slices.Equal(sorted, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		panic("rand Perm mismatch")
	}

	random.Shuffle(len(values), func(i int, j int) {
		values[i], values[j] = values[j], values[i]
	})
	slices.Sort(values)
	if !slices.Equal(values, sorted) {
		panic("rand Shuffle mismatch")
	}
}
