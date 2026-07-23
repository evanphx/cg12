package main

import (
	"os"
	"reflect"
)

type reflectAggregateProbeInput struct {
	count int
	text  string
	data  []byte
}

func inspectReflectAggregateProbe(value reflectAggregateProbeInput) int {
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

func failReflectAggregateProbe(message string) {
	println(message)
	os.Exit(1)
}

func main() {
	directArgument := reflectAggregateProbeInput{
		count: 5,
		text:  "runtime",
		data:  []byte{10, 20, 30},
	}
	directResult := inspectReflectAggregateProbe(directArgument)
	if directResult != 42 {
		println("direct result", directResult)
		failReflectAggregateProbe("direct aggregate call returned wrong value")
	}

	function := reflect.ValueOf(inspectReflectAggregateProbe)
	argument := reflectAggregateProbeInput{
		count: 5,
		text:  "runtime",
		data:  []byte{10, 20, 30},
	}
	results := function.Call([]reflect.Value{reflect.ValueOf(argument)})
	if len(results) != 1 {
		println("result count", len(results))
		failReflectAggregateProbe("reflect aggregate call returned wrong result count")
	}

	result := results[0]
	if !result.IsValid() {
		failReflectAggregateProbe("reflect aggregate call returned invalid result")
	}
	kind := result.Kind()
	if kind != reflect.Int {
		println("result kind", int(kind))
		failReflectAggregateProbe("reflect aggregate call returned non-int result")
	}
	value := result.Int()
	if value != 42 {
		println("result value", value)
		failReflectAggregateProbe("reflect aggregate call returned wrong value")
	}
}
