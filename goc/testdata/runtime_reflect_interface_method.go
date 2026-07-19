package main

import "reflect"

type reflectedGreeter interface {
	Greet(string) string
}

type reflectedValue struct {
	prefix string
}

func (value reflectedValue) Greet(name string) string {
	return value.prefix + name
}

func main() {
	var greeter reflectedGreeter = reflectedValue{prefix: "hello "}
	method := reflect.ValueOf(greeter).MethodByName("Greet")
	results := method.Call([]reflect.Value{reflect.ValueOf("runtime")})
	if len(results) != 1 {
		panic("reflect method returned wrong count")
	}
	if results[0].String() != "hello runtime" {
		panic("reflect interface method result mismatch")
	}
}
