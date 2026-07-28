package main

import "fmt"

type coordinate struct {
	x int
	y int
}

// A per-iteration variable needs an instance shaped like the storage its type
// normally uses: an indirect cell for structs and arrays, an inline header for
// strings, slices and interfaces, and a plain cell for scalars. This reducer
// runs every shape through a three-clause loop and through a range clause.
func main() {
	var captured []func() string

	for text := "a"; len(text) < 4; text += "b" {
		captured = append(captured, func() string { return text })
	}
	if values := collect(captured); values != "a ab abb" {
		panic("string loop variable shared one slot: " + values)
	}

	captured = nil
	for boxed := any(0); boxed.(int) < 3; boxed = boxed.(int) + 1 {
		captured = append(captured, func() string { return fmt.Sprint(boxed) })
	}
	if values := collect(captured); values != "0 1 2" {
		panic("interface loop variable shared one slot: " + values)
	}

	captured = nil
	for numbers := []int{0}; len(numbers) < 4; numbers = append(numbers, len(numbers)) {
		captured = append(captured, func() string { return fmt.Sprint(numbers) })
	}
	if values := collect(captured); values != "[0] [0 1] [0 1 2]" {
		panic("slice loop variable shared one slot: " + values)
	}

	captured = nil
	for point := (coordinate{x: 0, y: 9}); point.x < 3; point.x++ {
		captured = append(captured, func() string { return fmt.Sprint(point.x, point.y) })
	}
	if values := collect(captured); values != "0 9 1 9 2 9" {
		panic("struct loop variable shared one slot: " + values)
	}

	captured = nil
	for pair := ([2]int{0, 5}); pair[0] < 3; pair[0]++ {
		captured = append(captured, func() string { return fmt.Sprint(pair[0], pair[1]) })
	}
	if values := collect(captured); values != "0 5 1 5 2 5" {
		panic("array loop variable shared one slot: " + values)
	}

	captured = nil
	for _, point := range []coordinate{{1, 2}, {3, 4}} {
		captured = append(captured, func() string { return fmt.Sprint(point.x, point.y) })
	}
	if values := collect(captured); values != "1 2 3 4" {
		panic("struct range variable shared one slot: " + values)
	}

	captured = nil
	for _, boxed := range []any{1, "two", true} {
		captured = append(captured, func() string { return fmt.Sprint(boxed) })
	}
	if values := collect(captured); values != "1 two true" {
		panic("interface range variable shared one slot: " + values)
	}

	captured = nil
	for _, numbers := range [][]int{{1}, {2, 3}} {
		captured = append(captured, func() string { return fmt.Sprint(numbers) })
	}
	if values := collect(captured); values != "[1] [2 3]" {
		panic("slice range variable shared one slot: " + values)
	}

	captured = nil
	for _, pair := range [][2]int{{1, 2}, {3, 4}} {
		captured = append(captured, func() string { return fmt.Sprint(pair[0], pair[1]) })
	}
	if values := collect(captured); values != "1 2 3 4" {
		panic("array range variable shared one slot: " + values)
	}
}

func collect(closures []func() string) string {
	text := ""
	for _, closure := range closures {
		if text != "" {
			text += " "
		}
		text += closure()
	}
	return text
}
