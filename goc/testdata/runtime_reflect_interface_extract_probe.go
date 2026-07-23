package main

import (
	"os"
	"reflect"
)

type reflectInterfaceProbeExtract interface {
	Value() int
}

type reflectInterfaceProbePayload struct {
	value int
}

func (payload *reflectInterfaceProbePayload) Value() int {
	return payload.value
}

func failReflectInterfaceProbe(message string) {
	println(message)
	os.Exit(1)
}

func main() {
	var original reflectInterfaceProbeExtract = &reflectInterfaceProbePayload{value: 42}
	value := reflect.ValueOf(original)
	if !value.IsValid() {
		failReflectInterfaceProbe("reflect ValueOf(interface) returned invalid value")
	}
	if value.Kind() != reflect.Ptr {
		println("value kind", int(value.Kind()))
		failReflectInterfaceProbe("reflect ValueOf(interface) returned wrong kind")
	}

	interfaced := value.Interface()
	extracted, ok := interfaced.(reflectInterfaceProbeExtract)
	if !ok {
		failReflectInterfaceProbe("reflect interface extraction lost interface type")
	}
	if extracted.Value() != 42 {
		println("extracted value", extracted.Value())
		failReflectInterfaceProbe("reflect interface extraction returned wrong value")
	}
}
