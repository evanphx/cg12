package main

import "reflect"

func main() {
	left := make(chan int, 1)
	right := make(chan int, 1)
	right <- 42

	cases := []reflect.SelectCase{
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(left),
		},
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(right),
		},
	}

	chosen, value, ok := reflect.Select(cases)
	if chosen != 1 || !ok || int(value.Int()) != 42 {
		panic("reflect select result mismatch")
	}
}
