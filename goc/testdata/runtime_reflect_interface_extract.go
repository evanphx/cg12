package main

import "reflect"

type reflectInterfaceExtract interface {
	Value() int
}

type reflectInterfacePayload struct {
	value int
}

func (payload *reflectInterfacePayload) Value() int {
	return payload.value
}

func main() {
	var original reflectInterfaceExtract = &reflectInterfacePayload{value: 42}
	value := reflect.ValueOf(original)
	extracted, ok := value.Interface().(reflectInterfaceExtract)
	if !ok {
		panic("reflect interface extraction lost interface type")
	}
	if extracted.Value() != 42 {
		panic("reflect interface extraction returned wrong value")
	}
}
