package main

import (
	"os"
	"reflect"
)

type reflectValueInstruction struct {
	field int
}

type reflectValueState struct {
	total int64
}

type reflectValueOp func(instruction *reflectValueInstruction, state *reflectValueState, value reflect.Value)

func addReflectValue(instruction *reflectValueInstruction, state *reflectValueState, value reflect.Value) {
	if value.Kind() != reflect.Int {
		println("indirect reflect value kind mismatch:", int(value.Kind()))
		os.Exit(1)
	}
	state.total += int64(instruction.field)
	state.total += value.Int()
}

func main() {
	instruction := &reflectValueInstruction{field: 5}
	state := &reflectValueState{}
	value := reflect.ValueOf(42)
	var operation reflectValueOp = addReflectValue
	operation(instruction, state, value)
	if state.total != 47 {
		println("indirect reflect value total mismatch:", state.total)
		os.Exit(1)
	}
}
