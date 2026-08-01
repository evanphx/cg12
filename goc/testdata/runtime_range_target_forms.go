package main

import "fmt"

// A `range` clause that assigns with `=` may name any assignable operand, not
// just an identifier: `for x.f = range s` and `for a[i], b.g = range m` are
// ordinary Go. cg12 resolved each side of the clause to the variable object it
// named and silently dropped everything else, so the loop ran the right number
// of times and the assignment never happened. A package-level identifier was
// dropped in the same way, because the clause gave the global a fresh frame
// slot instead of writing the symbol.
//
// This reducer walks the cross product of range subject against target form.
// Every case both accumulates what the body observed and reports the value the
// target holds after the loop, because those are two different ways for the
// assignment to go missing.

type inner struct {
	index int
	text  string
}

type outer struct {
	nested inner
	index  int
	text   string
	letter rune
	value  any
}

var globalIndex int
var globalText string

func main() {
	sliceTargets()
	arrayTargets()
	stringTargets()
	mapTargets()
	channelTargets()
	integerTargets()
	iteratorTargets()
	fmt.Println("range target forms ok")
}

func sliceTargets() {
	letters := []string{"a", "b", "c"}

	var box outer
	observed := ""
	for box.index = range letters {
		observed += fmt.Sprintf("%d", box.index)
	}
	expect("slice key into a struct field", observed, "012")
	expect("slice key into a struct field, after the loop", fmt.Sprintf("%d", box.index), "2")

	observed = ""
	for box.nested.index = range letters {
		observed += fmt.Sprintf("%d", box.nested.index)
	}
	expect("slice key into a nested struct field", observed, "012")

	destination := make([]int, 2)
	observed = ""
	for destination[1] = range letters {
		observed += fmt.Sprintf("%d", destination[1])
	}
	expect("slice key into a slice element", observed, "012")
	expect("slice key into a slice element, after the loop", fmt.Sprint(destination), "[0 2]")

	var array [2]int
	observed = ""
	for array[1] = range letters {
		observed += fmt.Sprintf("%d", array[1])
	}
	expect("slice key into an array element", observed, "012")
	expect("slice key into an array element, after the loop", fmt.Sprint(array), "[0 2]")

	counts := map[string]int{}
	observed = ""
	for counts["k"] = range letters {
		observed += fmt.Sprintf("%d", counts["k"])
	}
	expect("slice key into a map element", observed, "012")
	expect("slice key into a map element, after the loop", fmt.Sprintf("%d/%d", counts["k"], len(counts)), "2/1")

	pointer := new(int)
	observed = ""
	for *pointer = range letters {
		observed += fmt.Sprintf("%d", *pointer)
	}
	expect("slice key through a pointer", observed, "012")

	observed = ""
	for globalIndex = range letters {
		observed += fmt.Sprintf("%d", globalIndex)
	}
	expect("slice key into a package-level variable", observed, "012")
	expect("slice key into a package-level variable, read elsewhere", fmt.Sprintf("%d", readGlobalIndex()), "2")

	observed = ""
	for box.index, box.text = range letters {
		observed += fmt.Sprintf("%d%s", box.index, box.text)
	}
	expect("slice key and element into struct fields", observed, "0a1b2c")

	var key int
	observed = ""
	for key, box.text = range letters {
		observed += fmt.Sprintf("%d%s", key, box.text)
	}
	expect("slice identifier key with a struct field element", observed, "0a1b2c")

	var element string
	observed = ""
	for box.index, element = range letters {
		observed += fmt.Sprintf("%d%s", box.index, element)
	}
	expect("slice struct field key with an identifier element", observed, "0a1b2c")

	pointerToBox := &outer{}
	observed = ""
	for pointerToBox.index, pointerToBox.text = range letters {
		observed += fmt.Sprintf("%d%s", pointerToBox.index, pointerToBox.text)
	}
	expect("slice key and element through a pointer receiver", observed, "0a1b2c")

	records := []inner{{1, "a"}, {2, "b"}, {3, "c"}}
	observed = ""
	for _, box.nested = range records {
		observed += fmt.Sprintf("%d%s|", box.nested.index, box.nested.text)
	}
	expect("slice struct element into a struct field", observed, "1a|2b|3c|")

	boxed := []any{1, "x", true}
	observed = ""
	for _, box.value = range boxed {
		observed += fmt.Sprintf("%v|", box.value)
	}
	expect("slice interface element into a struct field", observed, "1|x|true|")

	numbers := []int{4, 5, 6}
	observed = ""
	for _, box.value = range numbers {
		observed += fmt.Sprintf("%v|", box.value)
	}
	expect("slice int element into an interface field", observed, "4|5|6|")

	var holder struct{ elements []int }
	groups := [][]int{{1}, {2, 3}, {4, 5, 6}}
	observed = ""
	for _, holder.elements = range groups {
		observed += fmt.Sprintf("%v|", holder.elements)
	}
	expect("slice slice element into a struct field", observed, "[1]|[2 3]|[4 5 6]|")

	var narrow struct{ value uint8 }
	observed = ""
	for _, narrow.value = range []byte{7, 8, 9} {
		observed += fmt.Sprintf("%d", narrow.value)
	}
	expect("byte element into a uint8 struct field", observed, "789")
}

func arrayTargets() {
	letters := [3]string{"a", "b", "c"}

	var box outer
	observed := ""
	for box.index, box.text = range letters {
		observed += fmt.Sprintf("%d%s", box.index, box.text)
	}
	expect("array key and element into struct fields", observed, "0a1b2c")

	pointer := new(string)
	observed = ""
	for _, *pointer = range letters {
		observed += *pointer
	}
	expect("array element through a pointer", observed, "abc")

	pointerToArray := &letters
	observed = ""
	for box.index, box.text = range pointerToArray {
		observed += fmt.Sprintf("%d%s", box.index, box.text)
	}
	expect("pointer-to-array key and element into struct fields", observed, "0a1b2c")

	observed = ""
	for globalIndex = range pointerToArray {
		observed += fmt.Sprintf("%d", globalIndex)
	}
	expect("pointer-to-array key into a package-level variable", observed, "012")
	expect("pointer-to-array key into a package-level variable, read elsewhere",
		fmt.Sprintf("%d", readGlobalIndex()), "2")
}

func stringTargets() {
	var box outer
	observed := ""
	for box.index = range "añb" {
		observed += fmt.Sprintf("%d", box.index)
	}
	expect("string key into a struct field", observed, "013")

	observed = ""
	for box.index, box.letter = range "añb" {
		observed += fmt.Sprintf("%d:%d|", box.index, box.letter)
	}
	expect("string key and rune into struct fields", observed, "0:97|1:241|3:98|")

	pointer := new(rune)
	observed = ""
	for _, *pointer = range "añb" {
		observed += fmt.Sprintf("%d|", *pointer)
	}
	expect("string rune through a pointer", observed, "97|241|98|")

	observed = ""
	for globalIndex = range "añb" {
		observed += fmt.Sprintf("%d", globalIndex)
	}
	expect("string key into a package-level variable, read elsewhere",
		fmt.Sprintf("%s/%d", observed, readGlobalIndex()), "013/3")
}

func mapTargets() {
	single := map[int]string{7: "x"}

	var box outer
	observed := ""
	for box.index, box.text = range single {
		observed += fmt.Sprintf("%d%s", box.index, box.text)
	}
	expect("map key and element into struct fields", observed, "7x")

	weights := map[int]int{1: 10, 2: 20, 3: 30}
	total := 0
	for box.index, box.nested.index = range weights {
		total += box.index * box.nested.index
	}
	expect("map key and element into struct fields, summed", fmt.Sprintf("%d", total), "140")

	source := map[int]int{7: 9}
	destination := map[int]int{}
	for destination[0], destination[1] = range source {
		_ = destination
	}
	expect("map key and element into map elements",
		fmt.Sprintf("%d/%d/%d", destination[0], destination[1], len(destination)), "7/9/2")

	for _, globalText = range single {
		_ = globalText
	}
	expect("map element into a package-level variable, read elsewhere", readGlobalText(), "x")
}

func channelTargets() {
	var box outer
	stream := make(chan int, 3)
	stream <- 4
	stream <- 5
	stream <- 6
	close(stream)
	observed := ""
	for box.index = range stream {
		observed += fmt.Sprintf("%d", box.index)
	}
	expect("channel element into a struct field", observed, "456")

	pointer := new(int)
	stream = make(chan int, 2)
	stream <- 7
	stream <- 8
	close(stream)
	observed = ""
	for *pointer = range stream {
		observed += fmt.Sprintf("%d", *pointer)
	}
	expect("channel element through a pointer", observed, "78")

	texts := make(chan string, 2)
	texts <- "a"
	texts <- "b"
	close(texts)
	destination := make([]string, 1)
	observed = ""
	for destination[0] = range texts {
		observed += destination[0]
	}
	expect("channel element into a slice element", observed, "ab")

	numbers := make(chan int, 2)
	numbers <- 1
	numbers <- 2
	close(numbers)
	observed = ""
	for globalIndex = range numbers {
		observed += fmt.Sprintf("%d", globalIndex)
	}
	expect("channel element into a package-level variable, read elsewhere",
		fmt.Sprintf("%s/%d", observed, readGlobalIndex()), "12/2")
}

func integerTargets() {
	var box outer
	observed := ""
	for box.index = range 3 {
		observed += fmt.Sprintf("%d", box.index)
	}
	expect("integer key into a struct field", observed, "012")

	destination := make([]int, 1)
	observed = ""
	for destination[0] = range 3 {
		observed += fmt.Sprintf("%d", destination[0])
	}
	expect("integer key into a slice element", observed, "012")

	observed = ""
	for globalIndex = range 3 {
		observed += fmt.Sprintf("%d", globalIndex)
	}
	expect("integer key into a package-level variable, read elsewhere",
		fmt.Sprintf("%s/%d", observed, readGlobalIndex()), "012/2")

	box.index = 42
	for box.index = range 0 {
		_ = box.index
	}
	expect("integer range with no iterations leaves the target alone",
		fmt.Sprintf("%d", box.index), "42")
}

func pairs(yield func(int, string) bool) {
	for index, text := range []string{"a", "b", "c"} {
		if !yield(index, text) {
			return
		}
	}
}

func doubles(yield func(int) bool) {
	for index := 0; index < 3; index++ {
		if !yield(index * 2) {
			return
		}
	}
}

// iteratorTargets accumulates into a string, exactly as every other subject
// above does. It used to accumulate into a slice instead: the yield function a
// range-over-function body is lowered into is a closure, and `observed += ...`
// from inside a closure left the enclosing frame's variable addressing the
// yield function's dead frame (RUNTIME_PLAN.md 5.10). Four cases here were
// rewritten around that, which made this the one subject in the file that did
// not assert what it meant to. The lowered assignment is fixed, so they assert
// it again -- and the string accumulator is now itself a captured-variable
// case, which is why it is the form worth keeping.
func iteratorTargets() {
	var box outer
	observed := ""
	for box.index, box.text = range pairs {
		observed += fmt.Sprintf("%d%s", box.index, box.text)
	}
	expect("iterator key and element into struct fields", observed, "0a1b2c")
	expect("iterator key and element into struct fields, after the loop",
		fmt.Sprintf("%d%s", box.index, box.text), "2c")

	pointer := new(string)
	observed = ""
	for _, *pointer = range pairs {
		observed += *pointer
	}
	expect("iterator element through a pointer", observed, "abc")

	observed = ""
	for globalIndex = range doubles {
		observed += fmt.Sprintf("%d", globalIndex)
	}
	expect("iterator key into a package-level variable", observed, "024")
	expect("iterator key into a package-level variable, read elsewhere",
		fmt.Sprintf("%d", readGlobalIndex()), "4")

	destination := make([]int, 1)
	observed = ""
	for destination[0] = range doubles {
		observed += fmt.Sprintf("%d", destination[0])
	}
	expect("iterator key into a slice element", observed, "024")
}

func readGlobalIndex() int {
	return globalIndex
}

func readGlobalText() string {
	return globalText
}

func expect(what, got, want string) {
	if got != want {
		panic(what + ": got " + got + ", want " + want)
	}
}
