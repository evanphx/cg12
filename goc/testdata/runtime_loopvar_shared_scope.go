package main

import "fmt"

// Per-iteration scoping applies only to variables a loop declares. A variable
// declared outside the loop, and a range clause that assigns to existing
// variables with `=` rather than declaring with `:=`, keep one instance for the
// whole loop. This reducer pins the boundary from the other side, so a fix for
// the per-iteration case cannot quietly rescope everything.
func main() {
	var captured []func() int

	shared := 0
	for ; shared < 3; shared++ {
		captured = append(captured, func() int { return shared })
	}
	if values := collect(captured); values != "3 3 3" {
		panic("a variable declared outside the loop was rescoped: " + values)
	}

	captured = nil
	var index int
	var letter string
	for index, letter = range []string{"a", "b", "c"} {
		captured = append(captured, func() int { return index + len(letter) })
	}
	if values := collect(captured); values != "3 3 3" {
		panic("an assigning range clause was rescoped: " + values)
	}
	if index != 2 || letter != "c" {
		panic("an assigning range clause left the wrong final values: " + fmt.Sprint(index, letter))
	}

	// The loop counter is private storage, so writing to the iteration variable
	// inside the body cannot disturb the iteration.
	visited := ""
	for position := range []int{0, 1, 2} {
		if position == 0 {
			position = 5
		}
		visited += fmt.Sprint(position)
	}
	if visited != "512" {
		panic("writing the range key disturbed the iteration: " + visited)
	}

	visited = ""
	for position := range 3 {
		if position == 0 {
			position = 9
		}
		visited += fmt.Sprint(position)
	}
	if visited != "912" {
		panic("writing the integer range key disturbed the iteration: " + visited)
	}

	visited = ""
	for offset, letter := range "abc" {
		if offset == 0 {
			offset = 7
		}
		visited += fmt.Sprint(offset) + string(letter)
	}
	if visited != "7a1b2c" {
		panic("writing the string range key disturbed the iteration: " + visited)
	}
}

func collect(closures []func() int) string {
	text := ""
	for _, closure := range closures {
		if text != "" {
			text += " "
		}
		text += fmt.Sprint(closure())
	}
	return text
}
