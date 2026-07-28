package main

import "fmt"

// The order in which a `range` clause evaluates and stores its targets is the
// part of this lowering that is easy to get wrong once the targets are no
// longer plain identifiers. Go assigns in two phases: the operands of the
// destinations' index expressions and pointer indirections are evaluated
// first, and only then are the values stored, left to right. A clause runs
// that assignment once per iteration, before the body, and not at all when the
// loop body never runs.
//
// Every expectation here was taken from the host toolchain before the fix, not
// derived from the specification by hand.

var trace []string

func note(event string) {
	trace = append(trace, event)
}

func recordedIndex() int {
	note("index")
	return 0
}

func recordedKey() string {
	note("key")
	return fmt.Sprintf("k%d", len(trace))
}

func main() {
	indexOperandRunsEveryIteration()
	indexOperandNeverRunsWithoutIterations()
	mapKeyOperandRunsEveryIteration()
	nilMapTargetIsNotAssignedWithoutIterations()
	pointerTargetFollowsTheBody()
	elementTargetSeesThePreviousKey()
	targetIsAssignedBeforeTheBody()
	breakAndContinueLeaveTheLastValue()
	targetAliasingTheRangeExpression()
	fmt.Println("range target order ok")
}

func indexOperandRunsEveryIteration() {
	trace = nil
	destination := make([]int, 1)
	for destination[recordedIndex()] = range []string{"a", "b"} {
		note("body")
	}
	expect("the index operand of a range target runs once per iteration",
		fmt.Sprint(trace), "[index body index body]")
	expect("the index operand's target held the last key", fmt.Sprint(destination), "[1]")
}

func indexOperandNeverRunsWithoutIterations() {
	trace = nil
	destination := make([]int, 1)
	var empty []string
	for destination[recordedIndex()] = range empty {
		note("body")
	}
	expect("a range target is not evaluated when the loop does not run",
		fmt.Sprint(trace), "[]")
}

func mapKeyOperandRunsEveryIteration() {
	trace = nil
	counts := map[string]int{}
	for counts[recordedKey()] = range []string{"a", "b"} {
		note("body")
	}
	expect("the key operand of a map range target runs once per iteration",
		fmt.Sprint(trace), "[key body key body]")
	expect("each iteration wrote its own map entry",
		fmt.Sprintf("%d/%d/%d", len(counts), counts["k1"], counts["k3"]), "2/0/1")
}

func nilMapTargetIsNotAssignedWithoutIterations() {
	var counts map[int]int
	var empty []int
	for counts[0] = range empty {
		_ = counts
	}
	// Assigning to a nil map panics, so reaching this line proves the clause
	// performed no assignment at all.
	expect("a nil map range target with no iterations does not panic",
		fmt.Sprintf("%d", len(counts)), "0")
}

func pointerTargetFollowsTheBody() {
	first, second := -1, -1
	pointer := &first
	for *pointer = range []string{"x", "y"} {
		pointer = &second
	}
	expect("the pointer indirection of a range target is re-evaluated per iteration",
		fmt.Sprintf("%d/%d", first, second), "0/1")
}

func elementTargetSeesThePreviousKey() {
	runes := map[int]rune{}
	var key int
	for key, runes[key] = range "abc" {
		_ = key
	}
	expect("a range element target is indexed with the key the clause is about to overwrite",
		fmt.Sprintf("%d/%d/%d/%d/%d", len(runes), key, runes[0], runes[1], runes[2]),
		"2/2/98/99/0")
}

func targetIsAssignedBeforeTheBody() {
	var box struct{ index int }
	box.index = 99
	observed := ""
	for box.index = range []string{"a"} {
		observed += fmt.Sprintf("%d", box.index)
	}
	expect("the body observes this iteration's key, not the target's old value", observed, "0")
}

func breakAndContinueLeaveTheLastValue() {
	var box struct{ index int }
	for box.index = range []string{"a", "b", "c"} {
		if box.index == 1 {
			break
		}
	}
	expect("break leaves the key the last executed iteration assigned",
		fmt.Sprintf("%d", box.index), "1")

	observed := ""
	for box.index = range []string{"a", "b", "c"} {
		if box.index == 1 {
			continue
		}
		observed += fmt.Sprintf("%d", box.index)
	}
	expect("continue keeps assigning the key",
		fmt.Sprintf("%s/%d", observed, box.index), "02/2")
}

func targetAliasingTheRangeExpression() {
	numbers := []int{9, 9, 9}
	observed := ""
	for numbers[0] = range numbers {
		observed += fmt.Sprintf("%v|", numbers)
	}
	expect("a key target inside the range expression does not change the iteration",
		observed, "[0 9 9]|[1 9 9]|[2 9 9]|")

	numbers = []int{9, 8, 7}
	observed = ""
	for numbers[0], numbers[1] = range numbers {
		observed += fmt.Sprintf("%v|", numbers)
	}
	expect("the element is read before the key target overwrites it",
		observed, "[0 9 7]|[1 9 7]|[2 7 7]|")

	letters := []string{"a", "b", "c"}
	observed = ""
	for index, letter := range letters {
		letters[index] = "z"
		observed += letter
	}
	expect("an element variable holds a copy, not a window into the range expression",
		observed, "abc")
}

func expect(what, got, want string) {
	if got != want {
		panic(what + ": got " + got + ", want " + want)
	}
}
