package main

import "reflect"

type reflectAggregateInput struct {
	count int
	text  string
	data  []byte
}

func inspectReflectAggregate(value reflectAggregateInput) int {
	if value.count != 5 {
		return 1
	}
	if value.text != "runtime" {
		return 2
	}
	if len(value.data) != 3 || value.data[0] != 10 || value.data[2] != 30 {
		return 3
	}
	return 42
}

func main() {
	function := reflect.ValueOf(inspectReflectAggregate)
	argument := reflectAggregateInput{
		count: 5,
		text:  "runtime",
		data:  []byte{10, 20, 30},
	}
	results := function.Call([]reflect.Value{reflect.ValueOf(argument)})
	if int(results[0].Int()) != 42 {
		panic("reflect aggregate call result mismatch")
	}
}
