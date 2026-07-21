package main

import "sort"

func main() {
	values := []int{1, 3, 5, 7, 9}
	index, found := sort.Find(len(values), func(index int) int {
		if values[index] == 7 {
			return 0
		}
		if values[index] > 7 {
			return -1
		}
		return 1
	})
	if !found || index != 3 {
		panic("sort.Find did not find existing value")
	}

	index, found = sort.Find(len(values), func(index int) int {
		if values[index] == 6 {
			return 0
		}
		if values[index] > 6 {
			return -1
		}
		return 1
	})
	if found || index != 3 {
		panic("sort.Find returned wrong insertion point")
	}
}
