// Reducer for the interior-pointer escape hole described in RUNTIME_PLAN.md
// section 6, "The field-address escape hole".
//
// Storing the address of a *field* of a local variable, without ever storing
// the variable itself, has to promote that variable to the heap: the field
// address is an interior pointer that keeps the whole object alive, and Go
// gives every iteration of the loop its own object.
//
// cg12's escape analysis used to treat any field selection as a non-escaping
// use of the variable, so the object stayed a frame allocation. Two things went
// wrong at once, and this program checks both:
//
//   - one frame slot was reused by every iteration, so all of the recorded
//     addresses were the same address and only the last iteration's value
//     survived. That is a wrong answer in ordinary Go.
//   - a package-level slice ended up holding a goroutine stack address, which
//     is the invariant section 5.8 exists to protect. runtime.SetFinalizer
//     rejects such a pointer with "pointer not in allocated block", so calling
//     it is a direct assertion that the object really is a heap object.
//
// The address of a slice element, &v[i], was already handled; the field case
// was not. Both are checked here so the two stay together.
package main

import "runtime"

type payload struct {
	bytes [64]byte
}

type record struct {
	first payload
	tag   int64
}

const records = 64

// Only the interior pointers are retained. Nothing holds the containing
// objects, so if they were not heap-allocated these globals would hold stack
// addresses.
var fieldAddresses []*payload
var elementAddresses []*record
var tagAddresses []*int64

func recordFieldAddresses() {
	for index := 0; index < records; index++ {
		object := &record{}
		object.first.bytes[0] = byte(index)
		object.tag = int64(index) * 3
		fieldAddresses = append(fieldAddresses, &object.first)
		tagAddresses = append(tagAddresses, &object.tag)
	}
}

func recordElementAddresses() {
	for index := 0; index < records; index++ {
		objects := &[2]record{}
		objects[1].first.bytes[0] = byte(index)
		elementAddresses = append(elementAddresses, &objects[1])
	}
}

func main() {
	recordFieldAddresses()
	recordElementAddresses()

	if len(fieldAddresses) != records || len(tagAddresses) != records || len(elementAddresses) != records {
		panic("the recorded address lists have the wrong length")
	}

	// Every iteration must have produced its own object.
	for index := 1; index < records; index++ {
		if fieldAddresses[index] == fieldAddresses[index-1] {
			panic("two loop iterations shared one object behind a field address")
		}
		if elementAddresses[index] == elementAddresses[index-1] {
			panic("two loop iterations shared one object behind an element address")
		}
	}

	// Each object must still hold the value its own iteration wrote.
	for index := 0; index < records; index++ {
		if fieldAddresses[index].bytes[0] != byte(index) {
			panic("an object reached through a field address holds another iteration's value")
		}
		if *tagAddresses[index] != int64(index)*3 {
			panic("an object reached through a scalar field address holds another iteration's value")
		}
		if elementAddresses[index].first.bytes[0] != byte(index) {
			panic("an object reached through an element address holds another iteration's value")
		}
	}

	// A stack address is not in an allocated block, so this throws rather than
	// returning if the objects were left on the frame. Only the field addresses
	// are usable here: they name offset zero of their object, which is what
	// SetFinalizer requires, while &objects[1] is an interior pointer that
	// SetFinalizer rejects on any toolchain.
	for index := 0; index < records; index++ {
		runtime.SetFinalizer(fieldAddresses[index], func(*payload) {})
	}

	// The objects are reachable only through the interior pointers, so a
	// collection must keep them and must not reuse their memory.
	runtime.GC()
	runtime.GC()
	reuse := make([][]byte, 0, 2048)
	for repetition := 0; repetition < 2048; repetition++ {
		block := make([]byte, 80)
		for position := range block {
			block[position] = 0xff
		}
		reuse = append(reuse, block)
	}
	for index := 0; index < records; index++ {
		if fieldAddresses[index].bytes[0] != byte(index) {
			panic("an object retained only by a field address was reused")
		}
		if *tagAddresses[index] != int64(index)*3 {
			panic("an object retained only by a scalar field address was reused")
		}
		if elementAddresses[index].first.bytes[0] != byte(index) {
			panic("an object retained only by an element address was reused")
		}
	}
	runtime.KeepAlive(reuse)
}
