package goc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeAllocationEscapeLowering(t *testing.T) {
	module, err := goc.Compile("escape.go", []byte(`
package main

import "runtime"

type pair struct {
	left  int
	right int
}

func localPair() int {
	value := new(pair)
	value.left = 7
	value.right = 11
	return value.left * value.right
}

func escapingPair() *pair {
	return new(pair)
}

func Test() int {
	runtime.GC()
	return localPair() + escapingPair().left
}
`))
	require.NoError(t, err)

	local := functionWithSuffix(t, module, "localPair")
	escaping := functionWithSuffix(t, module, "escapingPair")
	assert.True(t, containsInstruction(local, func(instruction ir.Instr) bool {
		return instruction.Op.IsAlloc()
	}), "local allocation was not promoted to the stack")
	assert.False(t, callsSymbol(local, "runtime.newobject"))
	assert.True(t, callsSymbol(escaping, "runtime.newobject"), "escaping allocation did not remain on the heap")
}

func functionWithSuffix(t *testing.T, module *ir.Module, suffix string) *ir.Func {
	t.Helper()
	for _, function := range module.Funcs {
		if strings.HasSuffix(function.Name, suffix) {
			return function
		}
	}
	t.Fatalf("function with suffix %q not found", suffix)
	return nil
}

func containsInstruction(function *ir.Func, match func(ir.Instr) bool) bool {
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if match(instruction) {
				return true
			}
		}
	}
	return false
}

func callsSymbol(function *ir.Func, symbol string) bool {
	return containsInstruction(function, func(instruction ir.Instr) bool {
		if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
			return false
		}
		callee := instruction.Args[0]
		if callee.Kind != ir.RefConst {
			return false
		}
		constant := function.Consts[callee.ID]
		return constant.Kind == ir.ConstSym && constant.Sym == symbol
	})
}
