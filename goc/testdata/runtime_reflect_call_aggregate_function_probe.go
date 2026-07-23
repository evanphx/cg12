package main

import "reflect"

type reflectAggregateFunctionInput struct {
	count int
	text  string
	data  []byte
}

func inspectReflectAggregateFunction(value reflectAggregateFunctionInput) int {
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

func Test() int {
	function := reflect.ValueOf(inspectReflectAggregateFunction)
	argument := reflectAggregateFunctionInput{
		count: 5,
		text:  "runtime",
		data:  []byte{10, 20, 30},
	}
	results := function.Call([]reflect.Value{reflect.ValueOf(argument)})
	return int(results[0].Int())
}

func main() {
	got := Test()
	if got != 42 {
		println("got", got)
	}
}
