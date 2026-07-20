package main

import "unique"

type uniqueRecord struct {
	Name  string
	Count int
}

func main() {
	first := unique.Make(uniqueRecord{Name: "alpha", Count: 17})
	second := unique.Make(uniqueRecord{Name: "alpha", Count: 17})
	third := unique.Make(uniqueRecord{Name: "beta", Count: 25})

	if first != second {
		panic("unique handles for equal values differ")
	}
	if first == third {
		panic("unique handles for different values match")
	}
	if first.Value().Name != "alpha" || first.Value().Count != 17 {
		panic("unique value mismatch")
	}
}
