package main

type methodExpressionCounter struct {
	base int
}

func (counter methodExpressionCounter) Add(value int) int {
	return counter.base + value
}

func (counter *methodExpressionCounter) AddPointer(value int) int {
	counter.base += value
	return counter.base
}

func main() {
	valueCall := methodExpressionCounter.Add
	pointerCall := (*methodExpressionCounter).AddPointer

	counter := methodExpressionCounter{base: 30}
	if valueCall(counter, 12) != 42 {
		panic("value method expression returned wrong result")
	}
	if pointerCall(&counter, 5) != 35 {
		panic("pointer method expression returned wrong result")
	}
}
