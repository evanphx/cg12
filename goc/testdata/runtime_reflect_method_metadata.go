package main

import "reflect"

type reflectMethodWorker struct {
	base int
}

func (worker reflectMethodWorker) Add(value int) int {
	return worker.base + value
}

func main() {
	typ := reflect.TypeOf(reflectMethodWorker{})
	if typ.NumMethod() != 1 {
		panic("wrong method count")
	}

	method, ok := typ.MethodByName("Add")
	if !ok {
		panic("method not found")
	}
	if method.Type.NumIn() != 2 || method.Type.In(1).Kind() != reflect.Int {
		panic("wrong method type")
	}

	results := method.Func.Call([]reflect.Value{
		reflect.ValueOf(reflectMethodWorker{base: 35}),
		reflect.ValueOf(7),
	})
	if len(results) != 1 || int(results[0].Int()) != 42 {
		panic("reflect method call returned wrong result")
	}
}
