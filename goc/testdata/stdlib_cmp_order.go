package main

import "cmp"

func main() {
	if cmp.Compare("alpha", "beta") >= 0 {
		panic("cmp string order mismatch")
	}
	if cmp.Compare(42, 17) <= 0 {
		panic("cmp int order mismatch")
	}
	if cmp.Or("", "fallback", "ignored") != "fallback" {
		panic("cmp Or mismatch")
	}
}
