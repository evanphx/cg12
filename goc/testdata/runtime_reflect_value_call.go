package main

import "reflect"

func add(left int, right int) int {
	return left + right
}

func main() {
	function := reflect.ValueOf(add)
	arguments := []reflect.Value{
		reflect.ValueOf(17),
		reflect.ValueOf(25),
	}
	results := function.Call(arguments)
	if len(results) != 1 {
		panic("reflect call returned the wrong number of values")
	}
	if int(results[0].Int()) != 42 {
		panic("reflect call returned the wrong value")
	}
}
