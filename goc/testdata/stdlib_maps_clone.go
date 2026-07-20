package main

import "maps"

func main() {
	original := map[string]int{
		"alpha": 1,
		"beta":  2,
	}
	clone := maps.Clone(original)
	clone["gamma"] = 3
	if maps.Equal(original, clone) {
		panic("maps Clone alias mismatch")
	}
	if original["gamma"] != 0 {
		panic("maps Clone modified original")
	}
}
