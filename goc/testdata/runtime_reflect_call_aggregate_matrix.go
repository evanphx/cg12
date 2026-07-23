package main

import (
	"os"
	"reflect"
)

type reflectStringAggregate struct {
	count int
	text  string
}

type reflectSliceAggregate struct {
	count int
	data  []byte
}

type reflectMixedAggregate struct {
	count int
	text  string
	data  []byte
}

func inspectReflectStringAggregate(value reflectStringAggregate) int {
	if value.count != 5 {
		return 10 + value.count
	}
	if value.text != "runtime" {
		return 20 + len(value.text)
	}
	return 42
}

func inspectReflectSliceAggregate(value reflectSliceAggregate) int {
	if value.count != 5 {
		return 10 + value.count
	}
	if len(value.data) != 3 {
		return 30 + len(value.data)
	}
	if value.data[0] != 10 {
		return 40 + int(value.data[0])
	}
	if value.data[2] != 30 {
		return 50 + int(value.data[2])
	}
	return 42
}

func inspectReflectMixedAggregate(value reflectMixedAggregate) int {
	if value.count != 5 {
		return 10 + value.count
	}
	if value.text != "runtime" {
		return 20 + len(value.text)
	}
	if len(value.data) != 3 {
		return 30 + len(value.data)
	}
	if value.data[0] != 10 {
		return 40 + int(value.data[0])
	}
	if value.data[2] != 30 {
		return 50 + int(value.data[2])
	}
	return 42
}

func callReflectAggregateMatrix(function any, argument any) int {
	results := reflect.ValueOf(function).Call([]reflect.Value{reflect.ValueOf(argument)})
	if len(results) != 1 {
		println("result count", len(results))
		os.Exit(1)
	}
	return int(results[0].Int())
}

func callReflectMixedAggregateDirect() int {
	function := reflect.ValueOf(inspectReflectMixedAggregate)
	argument := reflectMixedAggregate{
		count: 5,
		text:  "runtime",
		data:  []byte{10, 20, 30},
	}
	results := function.Call([]reflect.Value{reflect.ValueOf(argument)})
	if len(results) != 1 {
		println("direct result count", len(results))
		os.Exit(1)
	}
	return int(results[0].Int())
}

func checkReflectAggregateMatrix(name string, got int) {
	if got != 42 {
		println(name, got)
		os.Exit(1)
	}
}

func main() {
	checkReflectAggregateMatrix("string", callReflectAggregateMatrix(
		inspectReflectStringAggregate,
		reflectStringAggregate{
			count: 5,
			text:  "runtime",
		},
	))
	checkReflectAggregateMatrix("slice", callReflectAggregateMatrix(
		inspectReflectSliceAggregate,
		reflectSliceAggregate{
			count: 5,
			data:  []byte{10, 20, 30},
		},
	))
	checkReflectAggregateMatrix("mixed", callReflectAggregateMatrix(
		inspectReflectMixedAggregate,
		reflectMixedAggregate{
			count: 5,
			text:  "runtime",
			data:  []byte{10, 20, 30},
		},
	))
	checkReflectAggregateMatrix("mixed-direct", callReflectMixedAggregateDirect())
}
