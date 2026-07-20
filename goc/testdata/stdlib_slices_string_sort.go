package main

import "slices"

func main() {
	keys := []string{"gamma", "alpha", "beta"}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"alpha", "beta", "gamma"}) {
		panic("string sort mismatch")
	}
}
