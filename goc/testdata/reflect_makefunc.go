package main

import "reflect"

func add(arguments []reflect.Value) []reflect.Value {
	left := arguments[0].Int()
	right := arguments[1].Int()
	return []reflect.Value{reflect.ValueOf(left + right)}
}

func main() {
	functionType := reflect.TypeOf(func(int64, int64) int64 { return 0 })
	function := reflect.MakeFunc(functionType, add).Interface().(func(int64, int64) int64)
	if function(19, 23) != 42 {
		panic("reflect.MakeFunc returned the wrong result")
	}
}
