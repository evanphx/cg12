package main

func main() {
	values := []int{1, 2, 3, 4, 5}
	window := values[1:3:4]
	if len(window) != 2 || cap(window) != 3 {
		panic("three-index slice produced wrong descriptor")
	}

	window = append(window, 99)
	if values[3] != 99 {
		panic("three-index slice used wrong capacity")
	}

	limited := values[1:3:3]
	limited = append(limited, 77)
	if values[3] == 77 {
		panic("three-index slice failed to limit capacity")
	}
}
