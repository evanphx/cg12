// The call shapes a method selected on a type-parameter-typed value can take.
//
// runtime_type_param_method_dispatch.go covers the crypto/ecdsa shape. This
// program covers the remaining lowering paths for the same construct, because
// goc lowers each of them separately: a void statement call, a single-result
// expression call, a multi-value assignment, a multi-value call in return
// position, a method value, a chained call whose receiver is itself a
// type-parameter-typed result, a value-receiver method reached through a
// pointer type argument, and a type argument that is itself a generic
// instantiation.
package main

import "strconv"

type pointerCounterA struct {
	count int
}

type pointerCounterB struct {
	count int
}

func (counter *pointerCounterA) Bump() {
	counter.count++
}

func (counter *pointerCounterA) Value() int {
	return counter.count
}

func (counter *pointerCounterA) Pair() (int, string) {
	return counter.count, "a"
}

func (counter *pointerCounterA) Next() *pointerCounterA {
	return &pointerCounterA{count: counter.count + 1}
}

func (counter *pointerCounterB) Bump() {
	counter.count += 10
}

func (counter *pointerCounterB) Value() int {
	return counter.count
}

func (counter *pointerCounterB) Pair() (int, string) {
	return counter.count, "b"
}

func (counter *pointerCounterB) Next() *pointerCounterB {
	return &pointerCounterB{count: counter.count + 10}
}

type counterLike[P any] interface {
	*pointerCounterA | *pointerCounterB

	Bump()
	Value() int
	Pair() (int, string)
	Next() P
}

func bumpTwice[P counterLike[P]](construct func() P) int {
	counter := construct()
	counter.Bump()
	counter.Bump()
	return counter.Value()
}

func pairInReturn[P counterLike[P]](construct func() P) (int, string) {
	return construct().Pair()
}

func pairInAssignment[P counterLike[P]](construct func() P) string {
	counter := construct()
	counter.Bump()
	count, label := counter.Pair()
	return label + ":" + strconv.Itoa(count)
}

func chained[P counterLike[P]](construct func() P) int {
	return construct().Next().Next().Value()
}

func throughMethodValue[P counterLike[P]](construct func() P) int {
	counter := construct()
	read := counter.Value
	counter.Bump()
	return read()
}

type scoreValueA struct {
	base int
}

type scoreValueB struct {
	base int
}

func (value scoreValueA) Score() int {
	return value.base * 2
}

func (value scoreValueB) Score() int {
	return value.base * 3
}

// valueScorer's type set holds value types, so the receiver is passed by value.
type valueScorer interface {
	scoreValueA | scoreValueB

	Score() int
}

func scoreValue[V valueScorer](value V) int {
	return value.Score()
}

// pointerScorer's type set holds pointer types whose methods are declared on
// the pointed-to value type, so the receiver has to be loaded through the
// pointer before the call.
type pointerScorer interface {
	*scoreValueA | *scoreValueB

	Score() int
}

func scorePointer[P pointerScorer](value P) int {
	return value.Score()
}

type cell[T any] struct {
	count int
}

func (c *cell[T]) Count() int {
	return c.count
}

// cellLike's type set holds instantiations of a generic type, so the resolved
// method is itself an instantiated method.
type cellLike interface {
	*cell[int] | *cell[string]

	Count() int
}

func countCell[P cellLike](c P) int {
	return c.Count()
}

type embeddedBase struct {
	name string
}

func (base *embeddedBase) Name() string {
	return base.name
}

// Both wrappers satisfy nameable only through the embedded field, and the
// field sits at a different offset in each, so the receiver has to be advanced
// by the offset belonging to the type argument.
type narrowWrapper struct {
	lead int
	embeddedBase
}

type wideWrapper struct {
	lead [5]int
	embeddedBase
}

type nameable interface {
	*narrowWrapper | *wideWrapper

	Name() string
}

func nameOf[P nameable](value P) string {
	return value.Name()
}

func main() {
	if bumpTwice(func() *pointerCounterA { return &pointerCounterA{} }) != 2 {
		panic("void statement call dispatched wrongly for counter A")
	}
	if bumpTwice(func() *pointerCounterB { return &pointerCounterB{} }) != 20 {
		panic("void statement call dispatched wrongly for counter B")
	}

	countA, labelA := pairInReturn(func() *pointerCounterA { return &pointerCounterA{count: 3} })
	if countA != 3 || labelA != "a" {
		panic("multi-value return call dispatched wrongly for counter A")
	}
	countB, labelB := pairInReturn(func() *pointerCounterB { return &pointerCounterB{count: 3} })
	if countB != 3 || labelB != "b" {
		panic("multi-value return call dispatched wrongly for counter B")
	}

	if pairInAssignment(func() *pointerCounterA { return &pointerCounterA{count: 5} }) != "a:6" {
		panic("multi-value assignment dispatched wrongly for counter A")
	}
	if pairInAssignment(func() *pointerCounterB { return &pointerCounterB{count: 5} }) != "b:15" {
		panic("multi-value assignment dispatched wrongly for counter B")
	}

	if chained(func() *pointerCounterA { return &pointerCounterA{count: 1} }) != 3 {
		panic("chained type-parameter call dispatched wrongly for counter A")
	}
	if chained(func() *pointerCounterB { return &pointerCounterB{count: 1} }) != 21 {
		panic("chained type-parameter call dispatched wrongly for counter B")
	}

	if throughMethodValue(func() *pointerCounterA { return &pointerCounterA{count: 4} }) != 5 {
		panic("method value dispatched wrongly for counter A")
	}
	if throughMethodValue(func() *pointerCounterB { return &pointerCounterB{count: 4} }) != 14 {
		panic("method value dispatched wrongly for counter B")
	}

	if scoreValue(scoreValueA{base: 6}) != 12 {
		panic("value receiver dispatched wrongly for score A")
	}
	if scoreValue(scoreValueB{base: 6}) != 18 {
		panic("value receiver dispatched wrongly for score B")
	}

	if scorePointer(&scoreValueA{base: 6}) != 12 {
		panic("value receiver through pointer dispatched wrongly for score A")
	}
	if scorePointer(&scoreValueB{base: 6}) != 18 {
		panic("value receiver through pointer dispatched wrongly for score B")
	}

	if countCell(&cell[int]{count: 7}) != 7 {
		panic("generic type argument dispatched wrongly for cell[int]")
	}
	if countCell(&cell[string]{count: 9}) != 9 {
		panic("generic type argument dispatched wrongly for cell[string]")
	}

	narrow := &narrowWrapper{lead: 1}
	narrow.name = "narrow"
	if nameOf(narrow) != "narrow" {
		panic("promoted method dispatched wrongly for the narrow wrapper")
	}
	wide := &wideWrapper{}
	wide.name = "wide"
	if nameOf(wide) != "wide" {
		panic("promoted method dispatched wrongly for the wide wrapper")
	}
}
