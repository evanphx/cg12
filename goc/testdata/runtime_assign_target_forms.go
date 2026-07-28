package main

import "fmt"

// The `range` clause is not the only statement that assigns to a destination it
// did not compute from an expression. Tuple assignment, a two-result map
// lookup, a two-result channel receive, a two-result type assertion and a
// `select` receive all do, and each of them used to resolve its destinations
// with its own rules. That produced three separate defects: a map element on
// the left of a tuple assignment was written through an address computed as if
// the map were a slice, `m[k] += v` discarded the operator, and a package-level
// string or slice assigned from a receive or a `select` was left pointing at
// the wrong header.
//
// They share one cause and are fixed by one shared destination helper, so they
// share this reducer.

type record struct {
	index int
	text  string
	value any
	found bool
}

var globalText string
var globalNumbers []int
var globalRecord record
var globalValue any

func main() {
	tupleAssignmentTargets()
	mapElementOperatorAssignment()
	tupleAssignmentEvaluationOrder()
	twoResultTargets()
	selectTargets()
	packageLevelTargets()
	fmt.Println("assign target forms ok")
}

func tupleAssignmentTargets() {
	var box record
	counts := map[string]int{}
	box.index, counts["k"] = 4, 5
	expect("a map element on the left of a tuple assignment",
		fmt.Sprintf("%d/%d/%d", box.index, counts["k"], len(counts)), "4/5/1")

	counts = map[string]int{}
	box.index, counts["k"] = twoNumbers()
	expect("a map element assigned from a two-result call",
		fmt.Sprintf("%d/%d/%d", box.index, counts["k"], len(counts)), "6/7/1")

	destination := make([]int, 2)
	box.index, destination[1] = 8, 9
	expect("a slice element on the left of a tuple assignment",
		fmt.Sprintf("%d/%v", box.index, destination), "8/[0 9]")
}

func mapElementOperatorAssignment() {
	counts := map[string]int{"k": 10}
	counts["k"] += 5
	expect("an assignment operator on a map element", fmt.Sprintf("%d", counts["k"]), "15")

	counts["k"] *= 2
	expect("a multiplying assignment operator on a map element",
		fmt.Sprintf("%d", counts["k"]), "30")

	counts["fresh"] += 3
	expect("an assignment operator on a missing map element",
		fmt.Sprintf("%d/%d", counts["fresh"], len(counts)), "3/2")
}

func tupleAssignmentEvaluationOrder() {
	index := 0
	destination := make([]int, 2)
	index, destination[index] = 3, 4
	expect("a tuple assignment indexes with the value it is about to overwrite",
		fmt.Sprintf("%d/%v", index, destination), "3/[4 0]")

	// Go evaluates the left operands' index expressions before the right-hand
	// expressions. cg12 evaluated each destination immediately before storing
	// into it, which put the right-hand calls first.
	slots := make([]int, 4)
	trace = nil
	slots[traced("index", 0)], slots[traced("index", 1)] = traced("value", 5), traced("value", 6)
	expect("the left operands are evaluated before the right-hand expressions",
		fmt.Sprint(trace), "[index0 index1 value5 value6]")

	trace = nil
	slots[traced("index", 2)], slots[traced("index", 3)] = tracedPair()
	expect("the left operands are evaluated before a two-result call",
		fmt.Sprint(trace), "[index2 index3 call]")
	expect("the two-result call reached its destinations", fmt.Sprint(slots), "[5 6 7 8]")
}

func twoResultTargets() {
	var box record
	counts := map[string]int{"a": 3}
	box.index, box.found = counts["a"]
	expect("a map lookup into non-identifier targets",
		fmt.Sprintf("%d/%v", box.index, box.found), "3/true")

	box.index, box.found = counts["missing"]
	expect("a missing map lookup into non-identifier targets",
		fmt.Sprintf("%d/%v", box.index, box.found), "0/false")

	stream := make(chan int, 1)
	stream <- 8
	box.index, box.found = <-stream
	expect("a channel receive into non-identifier targets",
		fmt.Sprintf("%d/%v", box.index, box.found), "8/true")
	close(stream)
	box.index, box.found = <-stream
	expect("a closed channel receive into non-identifier targets",
		fmt.Sprintf("%d/%v", box.index, box.found), "0/false")

	var boxed any = 11
	box.index, box.found = boxed.(int)
	expect("a type assertion into non-identifier targets",
		fmt.Sprintf("%d/%v", box.index, box.found), "11/true")
	var other any = "s"
	box.index, box.found = other.(int)
	expect("a failing type assertion into non-identifier targets",
		fmt.Sprintf("%d/%v", box.index, box.found), "0/false")

	texts := map[int]string{1: "one"}
	destination := make([]string, 1)
	destination[0], box.found = texts[1]
	expect("a map lookup into a slice element",
		fmt.Sprintf("%s/%v", destination[0], box.found), "one/true")
}

func selectTargets() {
	var box record
	stream := make(chan int, 1)
	stream <- 12
	select {
	case box.index = <-stream:
	}
	expect("a select receive into a struct field", fmt.Sprintf("%d", box.index), "12")

	stream <- 13
	select {
	case box.index, box.found = <-stream:
	}
	expect("a two-result select receive into struct fields",
		fmt.Sprintf("%d/%v", box.index, box.found), "13/true")

	destination := make([]int, 1)
	stream <- 21
	select {
	case destination[0] = <-stream:
	}
	expect("a select receive into a slice element", fmt.Sprint(destination), "[21]")

	numbers := make(chan int, 1)
	numbers <- 22
	select {
	case box.value = <-numbers:
	}
	expect("a select receive into an interface field", fmt.Sprintf("%v", box.value), "22")
}

func packageLevelTargets() {
	texts := make(chan string, 1)
	texts <- "hi"
	received := false
	globalText, received = <-texts
	expect("a two-result receive into a package-level string",
		fmt.Sprintf("%s/%s/%v", globalText, readGlobalText(), received), "hi/hi/true")

	texts <- "there"
	select {
	case globalText = <-texts:
	}
	expect("a select receive into a package-level string",
		fmt.Sprintf("%s/%s", globalText, readGlobalText()), "there/there")

	groups := make(chan []int, 1)
	groups <- []int{1, 2}
	globalNumbers, received = <-groups
	expect("a two-result receive into a package-level slice",
		fmt.Sprintf("%v/%v/%v", globalNumbers, readGlobalNumbers(), received), "[1 2]/[1 2]/true")

	records := make(chan record, 1)
	records <- record{index: 3, text: "q"}
	globalRecord, received = <-records
	got := readGlobalRecord()
	expect("a two-result receive into a package-level struct",
		fmt.Sprintf("%d%s/%d%s/%v", globalRecord.index, globalRecord.text, got.index, got.text, received),
		"3q/3q/true")

	values := make(chan any, 1)
	values <- 5
	globalValue, received = <-values
	expect("a two-result receive into a package-level interface",
		fmt.Sprintf("%v/%v/%v", globalValue, readGlobalValue(), received), "5/5/true")
}

func twoNumbers() (int, int) {
	return 6, 7
}

var trace []string

// traced records that it ran and returns what it was given, so an assignment's
// evaluation order is observable.
func traced(what string, value int) int {
	trace = append(trace, fmt.Sprintf("%s%d", what, value))
	return value
}

func tracedPair() (int, int) {
	trace = append(trace, "call")
	return 7, 8
}

func readGlobalText() string {
	return globalText
}

func readGlobalNumbers() []int {
	return globalNumbers
}

func readGlobalRecord() record {
	return globalRecord
}

func readGlobalValue() any {
	return globalValue
}

func expect(what, got, want string) {
	if got != want {
		panic(what + ": got " + got + ", want " + want)
	}
}
