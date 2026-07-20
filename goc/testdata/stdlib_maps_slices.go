package main

import (
	"maps"
	"slices"
)

func main() {
	scores := map[string]int{
		"beta":  2,
		"alpha": 1,
		"gamma": 3,
	}
	clone := map[string]int{}
	maps.Copy(clone, scores)
	clone["delta"] = 4
	if maps.Equal(scores, clone) {
		panic("maps Equal mismatch")
	}
	delete(clone, "delta")
	if !maps.Equal(scores, clone) {
		panic("maps Clone mismatch")
	}

	values := []int{5, 1, 3, 2, 4}
	slices.Sort(values)
	if !slices.Equal(values, []int{1, 2, 3, 4, 5}) {
		panic("sorted values mismatch")
	}

	numbers := []int{1, 2, 3, 4}
	doubled := slices.Collect(func(yield func(int) bool) {
		for _, number := range numbers {
			if !yield(number * 2) {
				return
			}
		}
	})
	if !slices.Equal(doubled, []int{2, 4, 6, 8}) {
		panic("slices Collect mismatch")
	}
}
