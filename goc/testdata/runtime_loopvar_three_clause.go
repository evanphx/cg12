package main

import "fmt"

// Go 1.22 gives the variables a three-clause for declares in its init statement
// one instance per iteration, so a closure created in the body keeps observing
// the value that iteration held.
func main() {
	var captured []func() int
	for index := 0; index < 3; index++ {
		captured = append(captured, func() int { return index })
	}
	if values := collect(captured); values != "0 1 2" {
		panic("three-clause loop variable shared one slot: " + values)
	}

	captured = nil
	for first, second := 0, 10; first < 3; first, second = first+1, second-1 {
		captured = append(captured, func() int { return first*100 + second })
	}
	if values := collect(captured); values != "10 109 208" {
		panic("multi-variable init shared one slot: " + values)
	}

	// Assigning to the variable inside the body changes only that iteration's
	// instance, and the post statement then runs on the updated value.
	captured = nil
	for index := 0; index < 3; index++ {
		index *= 2
		captured = append(captured, func() int { return index })
	}
	if values := collect(captured); values != "0 2" {
		panic("body assignment to loop variable lost: " + values)
	}

	// A closure that writes the variable writes its own iteration's instance.
	captured = nil
	for index := 0; index < 3; index++ {
		captured = append(captured, func() int { index++; return index })
	}
	for _, closure := range captured {
		closure()
	}
	if values := collect(captured); values != "2 3 4" {
		panic("closure writes crossed iterations: " + values)
	}

	// A loop with no post statement still gets a fresh instance per iteration.
	captured = nil
	for index := 0; index < 3; {
		index++
		captured = append(captured, func() int { return index })
	}
	if values := collect(captured); values != "1 2 3" {
		panic("post-less loop shared one slot: " + values)
	}

	// So does a loop with no condition.
	captured = nil
	for index := 0; ; index++ {
		if index == 3 {
			break
		}
		captured = append(captured, func() int { return index })
	}
	if values := collect(captured); values != "0 1 2" {
		panic("condition-less loop shared one slot: " + values)
	}

	// continue must still carry the iteration's value into the post statement.
	captured = nil
	for index := 0; index < 6; index++ {
		if index%2 == 1 {
			continue
		}
		captured = append(captured, func() int { return index })
	}
	if values := collect(captured); values != "0 2 4" {
		panic("continue lost the loop variable: " + values)
	}

	// A labeled continue from a nested loop behaves the same.
	captured = nil
outer:
	for index := 0; index < 3; index++ {
		for inner := 0; inner < 3; inner++ {
			if inner == 1 {
				captured = append(captured, func() int { return index*10 + inner })
				continue outer
			}
		}
	}
	if values := collect(captured); values != "1 11 21" {
		panic("labeled continue lost the loop variable: " + values)
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
