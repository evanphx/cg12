package main

import "maps"

func main() {
	scores := map[string]int{
		"beta":  2,
		"alpha": 1,
		"gamma": 3,
	}

	seen := map[string]bool{}
	for key := range maps.Keys(scores) {
		seen[key] = true
	}

	if len(seen) != 3 {
		panic("maps Keys length mismatch")
	}
	if !seen["alpha"] || !seen["beta"] || !seen["gamma"] {
		panic("maps Keys content mismatch")
	}
}
