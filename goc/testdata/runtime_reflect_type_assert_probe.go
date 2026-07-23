package main

import (
	"reflect"
	"unsafe"
)

type reflectTypeAssertStringer interface {
	String() string
}

type reflectTypeAssertNumber int

func (value reflectTypeAssertNumber) String() string {
	if value == 42 {
		return "forty-two"
	}
	return "other"
}

func Test() int {
	reflected := reflect.ValueOf(reflectTypeAssertNumber(42))
	empty := reflected.Interface()
	emptyWords := (*[2]unsafe.Pointer)(unsafe.Pointer(&empty))
	if emptyWords[0] == nil || emptyWords[1] == nil {
		return 1
	}

	value, ok := reflect.TypeAssert[reflectTypeAssertStringer](reflected)
	if !ok {
		return 2
	}
	interfaceWords := (*[2]unsafe.Pointer)(unsafe.Pointer(&value))
	if interfaceWords[0] == nil || interfaceWords[1] == nil {
		return 3
	}
	if value.String() != "forty-two" {
		return 4
	}
	return 42
}

func main() {
	got := Test()
	if got != 42 {
		println("got", got)
	}
}
