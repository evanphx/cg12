package main

import "reflect"

type deepEqualItem struct {
	name   string
	values []int
}

func main() {
	left := map[string]deepEqualItem{
		"a": {name: "left", values: []int{1, 2, 3}},
		"b": {name: "right", values: []int{5, 8}},
	}
	right := map[string]deepEqualItem{
		"a": {name: "left", values: []int{1, 2, 3}},
		"b": {name: "right", values: []int{5, 8}},
	}
	if !reflect.DeepEqual(left, right) {
		panic("equal maps were not deeply equal")
	}

	right["b"] = deepEqualItem{name: "right", values: []int{5, 13}}
	if reflect.DeepEqual(left, right) {
		panic("different maps were deeply equal")
	}
}
