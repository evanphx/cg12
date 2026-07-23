package main

import (
	"os"
	"reflect"
)

type reflectInstructionOp func(instruction *reflectInstruction, state *reflectInstructionState, value reflect.Value)

type reflectInstruction struct {
	op    reflectInstructionOp
	field int
	index []int
	indir int
}

type reflectInstructionState struct {
	sendZero bool
	fieldnum int
	total    int64
}

type reflectInstructionEngine struct {
	instr []reflectInstruction
}

func reflectInstructionAdd(instruction *reflectInstruction, state *reflectInstructionState, value reflect.Value) {
	if value.Kind() != reflect.Int {
		println("instruction reflect value kind mismatch:", int(value.Kind()))
		os.Exit(1)
	}
	if value.Int() != 42 {
		println("instruction reflect value mismatch:", value.Int())
		os.Exit(1)
	}
	state.total += int64(instruction.field - state.fieldnum)
	state.total += value.Int()
	state.fieldnum = instruction.field
}

func reflectInstructionValid(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Invalid:
		return false
	case reflect.Pointer:
		return !value.IsNil()
	}
	return true
}

func main() {
	engine := &reflectInstructionEngine{
		instr: []reflectInstruction{
			{
				op:    reflectInstructionAdd,
				field: 5,
			},
		},
	}
	state := &reflectInstructionState{
		sendZero: true,
	}
	value := reflect.ValueOf(42)
	instruction := &engine.instr[0]
	if reflectInstructionValid(value) {
		instruction.op(instruction, state, value)
	}
	if state.total != 47 {
		println("instruction reflect total mismatch:", state.total)
		os.Exit(1)
	}
}
