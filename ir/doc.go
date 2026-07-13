// Package ir defines cg12's SSA-based intermediate representation and the
// library-first API for constructing it.
//
// The design follows QBE's intermediate language in spirit — the same four base
// classes (word, long, single, double), the same block/phi/jump structure, and
// a comparable textual syntax — but is re-cast in idiomatic Go. Where QBE
// defines a distinct opcode per operand class (addw, addl, …), cg12 keeps
// operations polymorphic over [Cls] and recovers operand classes from the
// operands themselves, yielding a much smaller opcode set.
//
// The central types are:
//
//   - [Module]: a translation unit of functions, aggregate types, and data.
//   - [Func]: one function body in SSA form; owns its temporaries and constants.
//   - [Block]: a basic block — phis, a straight-line body, and one terminator.
//   - [Instr] / [Phi] / [Jmp]: the instruction, phi node, and terminator.
//   - [Ref]: a compact, comparable operand reference (temp, constant, …).
//
// Programs are built through the fluent methods in build.go:
//
//	m := ir.NewModule()
//	f := m.NewFunc("add", ir.ClsW).Export()
//	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
//	e := f.Entry()
//	e.Ret(e.Add(ir.ClsW, a, b))
//	fmt.Print(m) // QBE-compatible textual IL
package ir
