package main

import (
	"fmt"
	"runtime"
)

// A closure that assigns to a captured string variable used to leave the
// enclosing frame's variable addressing the closure's dead frame
// (RUNTIME_PLAN.md 5.10).
//
// A local string variable's slot held the address of a sixteen-byte header, and
// assigning a computed string copied that header into an alloca belonging to
// the function doing the assigning. When that function was a closure that had
// captured the variable by reference, the alloca died with the closure's frame
// and the caller's variable dangled. Depending on what the dead frame happened
// to hold afterwards the program printed an empty string, printed garbage,
// faulted in concatstrings, or died with `fatal error: runtime: out of memory`.
//
// Every case here writes through a closure, then calls clobber to overwrite the
// frame the closure left behind, and only then reads the variable. That order
// is the whole point: two of the reducers found while measuring this bug passed
// without it and failed with it, so a case that reads the variable straight
// after the call proves nothing.

type label string

func main() {
	assignmentForms()
	closureShapes()
	rangeOverFunction()
	controls()
	fmt.Println("closure captured string ok")
}

// clobber overwrites the stack a returning closure just gave up, so a variable
// left pointing into that frame reads something other than the value it held.
func clobber(depth int) int {
	if depth == 0 {
		return 0
	}
	var pad [64]int
	for index := range pad {
		pad[index] = depth + index
	}
	return pad[0] + clobber(depth-1)
}

func assignmentForms() {
	computed := "a"
	appendComputed := func(suffix string) {
		computed = computed + suffix
	}
	appendComputed("z")
	clobber(20)
	expect("computed with =", computed, "az")

	added := "a"
	addTo := func(suffix string) {
		added += suffix
	}
	addTo("z")
	clobber(20)
	expect("computed with +=", added, "az")

	literal := "a"
	assignLiteral := func() {
		literal = "az"
	}
	assignLiteral()
	clobber(20)
	expect("string literal", literal, "az")

	copied := "a"
	copyParameter := func(value string) {
		copied = value
	}
	copyParameter("z")
	clobber(20)
	expect("parameter copy", copied, "z")

	fromCall := "a"
	assignCall := func(value int) {
		fromCall = fmt.Sprint(value)
	}
	assignCall(7)
	clobber(20)
	expect("call result", fromCall, "7")

	fromBytes := "a"
	assignBytes := func(value []byte) {
		fromBytes = string(value)
	}
	assignBytes([]byte{'a', 'z'})
	clobber(20)
	expect("bytes conversion", fromBytes, "az")

	tupleText := "a"
	tupleCount := 0
	assignTuple := func(suffix string) {
		tupleCount, tupleText = 1, tupleText+suffix
	}
	assignTuple("z")
	clobber(20)
	expect("tuple assignment", fmt.Sprintf("%d%s", tupleCount, tupleText), "1az")

	first := "one"
	second := "two"
	swap := func() {
		first, second = second, first
	}
	swap()
	clobber(20)
	expect("swap", first+" "+second, "two one")

	var named label = "a"
	appendNamed := func(suffix label) {
		named = named + suffix
	}
	appendNamed("z")
	clobber(20)
	expect("named string type", string(named), "az")

	collected := ""
	collect := func(value int) {
		collected = collected + fmt.Sprint(value)
	}
	for value := 0; value < 5; value++ {
		collect(value)
	}
	clobber(20)
	expect("repeated calls", collected, "01234")

	surviving := "a"
	appendSurviving := func(suffix string) {
		surviving = surviving + suffix
	}
	appendSurviving("z")
	runtime.GC()
	clobber(20)
	expect("collected after the write", surviving, "az")
}

func closureShapes() {
	immediate := "a"
	func(suffix string) {
		immediate = immediate + suffix
	}("z")
	clobber(20)
	expect("immediately invoked literal", immediate, "az")

	nested := "a"
	outer := func(suffix string) {
		inner := func(value string) {
			nested = nested + value
		}
		inner(suffix)
	}
	outer("z")
	clobber(20)
	expect("literal inside a literal", nested, "az")

	generic := "a"
	applyString("z", func(suffix string) {
		generic = generic + suffix
	})
	clobber(20)
	expect("literal passed to a generic function", generic, "az")

	deferred := "a"
	func() {
		defer func() {
			deferred = deferred + "z"
		}()
	}()
	clobber(20)
	expect("deferred literal inside a literal", deferred, "az")

	expect("captured parameter", appendToParameter("a", "z"), "az")
	expect("captured named result", appendToNamedResult("z"), "az")
}

// appendToParameter captures its own parameter, which is a frame slot like any
// other local and was wrong in the same way.
func appendToParameter(text, suffix string) string {
	appendSuffix := func(value string) {
		text = text + value
	}
	appendSuffix(suffix)
	clobber(20)
	return text
}

func appendToNamedResult(suffix string) (text string) {
	text = "a"
	appendSuffix := func(value string) {
		text = text + value
	}
	appendSuffix(suffix)
	clobber(20)
	return
}

func applyString[T any](value T, apply func(T)) {
	apply(value)
}

func counter(yield func(int) bool) {
	for value := 0; value < 3; value++ {
		if !yield(value) {
			return
		}
	}
}

// rangeOverFunction is the same defect without a function literal in the
// source: the loop body is lowered into a yield function, which captures the
// accumulator by reference exactly as a closure does.
func rangeOverFunction() {
	assigned := ""
	for value := range counter {
		assigned = assigned + fmt.Sprint(value)
	}
	clobber(20)
	expect("range over function with =", assigned, "012")

	added := ""
	for value := range counter {
		added += fmt.Sprint(value)
	}
	clobber(20)
	expect("range over function with +=", added, "012")
}

// controls are the neighbouring shapes that were already correct. They are here
// because the fix changes how a captured variable is stored, and the cheapest
// way for that to go wrong is to disturb one of them.
func controls() {
	readOnly := "a"
	measure := func() int {
		return len(readOnly)
	}
	length := measure()
	clobber(20)
	expect("read-only capture", fmt.Sprintf("%d%s", length, readOnly), "1a")

	escaping := "a"
	done := make(chan struct{})
	go func(suffix string) {
		escaping = escaping + suffix
		close(done)
	}("z")
	<-done
	clobber(20)
	expect("escaping closure", escaping, "az")

	returnedText := "a"
	appendReturned, readReturned := stringPair(&returnedText)
	appendReturned("z")
	clobber(20)
	expect("returned closures share the variable", readReturned(), "az")

	texts := []string{"a"}
	appendText := func(suffix string) {
		texts = append(texts, suffix)
	}
	appendText("z")
	clobber(20)
	expect("captured slice", fmt.Sprint(texts), "[a z]")

	type record struct {
		text  string
		count int
	}
	var state record
	state.text = "a"
	updateState := func(suffix string) {
		state.text = state.text + suffix
		state.count++
	}
	updateState("z")
	clobber(20)
	expect("field of a captured struct", fmt.Sprintf("%s%d", state.text, state.count), "az1")

	elements := []string{"a"}
	updateElement := func(suffix string) {
		elements[0] = elements[0] + suffix
	}
	updateElement("z")
	clobber(20)
	expect("element of a captured slice", elements[0], "az")
}

func stringPair(seed *string) (func(string), func() string) {
	text := *seed
	return func(suffix string) { text = text + suffix }, func() string { return text }
}

func expect(what, got, want string) {
	if got != want {
		panic(what + ": got " + got + ", want " + want)
	}
}
