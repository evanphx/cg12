package main

import "sort"

func main() {
	values := []int{9, 1, 5, 3, 7}
	sort.Ints(values)
	for index, value := range []int{1, 3, 5, 7, 9} {
		if values[index] != value {
			panic("sort.Ints returned wrong order")
		}
	}

	position := sort.SearchInts(values, 7)
	if position != 3 {
		panic("SearchInts returned wrong index")
	}

	records := []struct {
		name  string
		score int
	}{
		{name: "beta", score: 25},
		{name: "alpha", score: 17},
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].name < records[right].name
	})
	if records[0].name != "alpha" || records[1].score != 25 {
		panic("sort.Slice returned wrong order")
	}
}
