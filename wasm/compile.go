// Package wasm is a WebAssembly backend for the cg12 IR. Unlike the arm64
// backend it targets a typed stack machine with structured control flow rather
// than a register machine, so it needs neither register allocation nor SSA
// destruction into physical registers: SSA temporaries map directly onto typed
// Wasm locals, and the arbitrary CFG is restructured into block/loop/if.
//
// The emitted form is the WebAssembly text format (WAT), the analogue of the
// arm64 backend's GNU-assembler text. It is consumed directly by wasmtime (and,
// after assembly, by any engine).
package wasm

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// ptrCls is wasm32's pointer class: linear-memory addresses are i32, the same as
// the word-register class, so pointers occupy no more space than a word and the
// pointer width can never diverge from the register width.
const ptrCls = ir.ClsW

// Compile emits WAT text for a single function in isolation. LowerPointers
// resolves the abstract pointer class to i32 first; after that the emitter sees
// only concrete classes. A function that needs module state (data symbols, or a
// shadow stack for `alloc`) should go through [CompileModule], which supplies
// the surrounding memory and globals.
func Compile(f *ir.Func) (string, error) {
	ir.LowerPointers(f, ptrCls)
	var sb strings.Builder
	if err := emitFunc(f, &sb, nil); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// CompileModule wraps every function in m in a single (module ...) form,
// preceded by the linear memory, shadow-stack pointer, and data segments the
// functions reference.
func CompileModule(m *ir.Module) (string, error) {
	ctx := layoutModule(m)

	var sb strings.Builder
	sb.WriteString("(module\n")
	if ctx.needMem {
		fmt.Fprintf(&sb, "\t(memory %d)\n", ctx.pages)
	}
	if ctx.hasSP {
		fmt.Fprintf(&sb, "\t(global $__sp (mut i32) (i32.const %d))\n", ctx.stackTop)
	}
	if ctx.needTab {
		emitTable(&sb, m)
	}
	for _, f := range m.Funcs {
		ir.LowerPointers(f, ptrCls)
		if err := emitFunc(f, &sb, ctx); err != nil {
			return "", fmt.Errorf("function %s: %w", f.Name, err)
		}
	}
	if err := emitData(&sb, m, ctx); err != nil {
		return "", err
	}
	sb.WriteString(")\n")
	return sb.String(), nil
}

// wasmType maps an IR class to its WebAssembly value type.
func wasmType(c ir.Cls) string {
	switch c {
	case ir.ClsW:
		return "i32"
	case ir.ClsL:
		return "i64"
	case ir.ClsS:
		return "f32"
	case ir.ClsD:
		return "f64"
	}
	return "i32"
}
