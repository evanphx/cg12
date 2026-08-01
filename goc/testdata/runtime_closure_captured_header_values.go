package main

import "fmt"

// The closure-capture defect in RUNTIME_PLAN.md 5.10 was reported against
// strings, but a string is not the only value cg12 keeps behind a pointer in a
// frame slot. Three types share that representation -- a string, an interface
// value, and a complex128 -- and all three were wrong in the same way when a
// closure assigned to a captured variable: the value was copied into the
// closure's frame and the enclosing variable was left addressing it.
//
// This reducer covers the two that are not strings. An interface failed
// visibly, printing `<invalid reflect.Value>` or panicking inside reflect with
// `can't call pointer on a non-pointer Value`; a complex128 failed quietly,
// with one half of the pair reading as a denormal.
//
// As in runtime_closure_captured_string.go every case clobbers the abandoned
// frame between the write and the read, because reading straight after the call
// can find the value still intact.

type pair struct {
	count int
	text  string
}

func main() {
	interfaceValues()
	complexValues()
	controls()
	fmt.Println("closure captured header values ok")
}

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

func interfaceValues() {
	var scalar any = 1
	assignScalar := func(value int) {
		scalar = value + 1
	}
	assignScalar(41)
	clobber(20)
	expect("interface holding an int", fmt.Sprint(scalar), "42")

	var text any = "a"
	assignText := func(suffix string) {
		text = "a" + suffix
	}
	assignText("z")
	clobber(20)
	expect("interface holding a string", fmt.Sprint(text), "az")

	var aggregate any = pair{1, "a"}
	assignAggregate := func(suffix string) {
		aggregate = pair{2, "a" + suffix}
	}
	assignAggregate("z")
	clobber(20)
	expect("interface holding a struct", fmt.Sprint(aggregate), "{2 az}")

	var failure error
	assignFailure := func(suffix string) {
		failure = fmt.Errorf("boom%s", suffix)
	}
	assignFailure("z")
	clobber(20)
	expect("error interface", failure.Error(), "boomz")

	var counted any = 0
	for value := range counter {
		counted = value
	}
	clobber(20)
	expect("interface assigned by a range-over-function body", fmt.Sprint(counted), "2")
}

func complexValues() {
	value := complex(1.0, 2.0)
	assignValue := func() {
		value = complex(3.0, 4.0)
	}
	assignValue()
	clobber(20)
	expect("complex128 assigned by a closure",
		fmt.Sprintf("%g %g", real(value), imag(value)), "3 4")

	escaping := complex(1.0, 2.0)
	done := make(chan struct{})
	go func() {
		escaping = complex(5.0, 6.0)
		close(done)
	}()
	<-done
	clobber(20)
	expect("complex128 assigned by an escaping closure",
		fmt.Sprintf("%g %g", real(escaping), imag(escaping)), "5 6")
}

func counter(yield func(int) bool) {
	for value := 0; value < 3; value++ {
		if !yield(value) {
			return
		}
	}
}

func controls() {
	var readOnly any = 42
	measure := func() string {
		return fmt.Sprint(readOnly)
	}
	observed := measure()
	clobber(20)
	expect("read-only interface capture", observed+"/"+fmt.Sprint(readOnly), "42/42")

	var escaping any = 1
	done := make(chan struct{})
	go func(value int) {
		escaping = value + 1
		close(done)
	}(41)
	<-done
	clobber(20)
	expect("interface assigned by an escaping closure", fmt.Sprint(escaping), "42")

	var uncaptured any = 1
	uncaptured = "plain"
	clobber(20)
	expect("interface never captured", fmt.Sprint(uncaptured), "plain")

	plain := complex(1.0, 2.0)
	plain = complex(7.0, 8.0)
	clobber(20)
	expect("complex128 never captured",
		fmt.Sprintf("%g %g", real(plain), imag(plain)), "7 8")
}

func expect(what, got, want string) {
	if got != want {
		panic(what + ": got " + got + ", want " + want)
	}
}
