package wasm

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// instr lowers one non-phi instruction to stack code followed by a local.set of
// its result (if any).
func (e *emitter) instr(in *ir.Instr) {
	switch in.Op {
	case ir.ONop:
		return

	case ir.OAdd, ir.OSub, ir.OMul, ir.ODiv, ir.OUDiv, ir.ORem, ir.OURem,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		e.push(in.Args[0])
		e.push(in.Args[1])
		e.line("%s", binOp(in.Op, in.Cls))

	case ir.ONeg:
		e.neg(in)

	case ir.OCmp:
		e.compare(in)

	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw,
		ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof,
		ir.OCast:
		e.convert(in)

	case ir.OCopy:
		e.copy(in)

	case ir.OLoadsb, ir.OLoadub, ir.OLoadsh, ir.OLoaduh,
		ir.OLoadsw, ir.OLoaduw, ir.OLoadl, ir.OLoads, ir.OLoadd:
		e.pushAddr(in.Args[0])
		e.line("%s", loadOp(in.Op, in.Cls))

	case ir.OStoreb, ir.OStoreh, ir.OStorew, ir.OStorel, ir.OStores, ir.OStored:
		e.pushAddr(in.Args[1]) // address
		e.push(in.Args[0])     // value
		e.line("%s", storeOp(in.Op, e.f.ClassOf(in.Args[0])))
		return // stores define no result

	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		e.alloc(in)

	case ir.OCall:
		e.call(in)
		return // call() handles its own result

	case ir.OVaStart:
		e.emitVaStart(in)
		return

	case ir.OVaArg:
		e.emitVaArg(in)
		return

	default:
		e.fail("wasm: op %s not yet supported", in.Op)
		return
	}
	e.store(in)
}

// alloc materialises a stack allocation's address: base + its frame offset. The
// base is an i32 address; a register-width (ClsL) pointer is widened to i64.
func (e *emitter) alloc(in *ir.Instr) {
	e.frameAddr(e.allocOff[in])
	if in.Cls == ir.ClsL {
		e.line("i64.extend_i32_u")
	}
}

// call lowers a call. A symbol callee is a direct `call $name`; a computed
// callee is an indirect `call_indirect` through the function table, its
// signature spelled inline from the call's argument and result classes. A call
// returning a by-value aggregate passes a caller-owned result buffer as a hidden
// leading argument (sret) and its result temporary becomes that buffer.
func (e *emitter) call(in *ir.Instr) {
	if in.Tail {
		// A well-placed tail call is emitted by emitTailCall as the block's
		// terminator; reaching call() means it is not in tail position.
		e.fail("wasm: tail call must be the last instruction and have its result returned")
		return
	}
	if cf, varargs, ok := e.variadicCall(in); ok {
		e.emitVariadicCall(in, cf, varargs)
		return
	}

	callee := in.Args[0]
	direct := callee.Kind == ir.RefConst && e.f.Consts[callee.ID].Kind == ir.ConstSym

	if in.RetAgg != nil {
		if !direct {
			e.fail("wasm: indirect aggregate-returning calls are not supported")
			return
		}
		// The result buffer lives in this frame; its address is the result.
		e.frameAddr(e.retBufOff[in])
		e.line("local.set %s", tempName(e.f.Temps[in.To.ID]))
		e.push(in.To) // hidden sret pointer, first argument
	}
	for _, a := range in.Args[1:] {
		e.push(a)
	}
	if direct {
		e.line("call $%s", sanitize(e.f.Consts[callee.ID].Sym))
	} else {
		e.pushAddr(callee) // the table index (i32) sits above the arguments
		e.line("call_indirect %s", e.callSig(in))
	}
	if in.RetAgg == nil {
		e.store(in) // a scalar result; the aggregate result was set above
	}
}

// frameAddr pushes the address of a frame slot at the given byte offset.
func (e *emitter) frameAddr(off int) {
	e.line("local.get $__base")
	if off != 0 {
		e.line("i32.const %d", off)
		e.line("i32.add")
	}
}

// emitTailCall emits a mandatory tail call as WebAssembly's return_call, which
// reuses the current frame and returns the callee's result. The shadow-stack
// frame is released first, exactly as an ordinary return would.
func (e *emitter) emitTailCall(in *ir.Instr) {
	if in.RetAgg != nil {
		e.fail("wasm: aggregate-returning tail call is not supported")
		return
	}
	callee := in.Args[0]
	direct := callee.Kind == ir.RefConst && e.f.Consts[callee.ID].Kind == ir.ConstSym
	e.releaseFrame()
	for _, a := range in.Args[1:] {
		e.push(a)
	}
	if direct {
		e.line("return_call $%s", sanitize(e.f.Consts[callee.ID].Sym))
	} else {
		e.pushAddr(callee) // table index on top, above the arguments
		e.line("return_call_indirect %s", e.callSig(in))
	}
}

// callSig spells an indirect call's type inline: one (param T) per argument and
// a (result T) when the call yields a value.
func (e *emitter) callSig(in *ir.Instr) string {
	var b strings.Builder
	for _, a := range in.Args[1:] {
		fmt.Fprintf(&b, "(param %s) ", wasmType(e.f.ClassOf(a)))
	}
	if !in.To.IsNone() {
		fmt.Fprintf(&b, "(result %s)", wasmType(in.Cls))
	}
	return strings.TrimSpace(b.String())
}

// loadOp returns the Wasm load mnemonic for a load opcode whose result has class
// res. The opcode fixes the memory width and signedness; res picks i32 vs i64.
func loadOp(op ir.Op, res ir.Cls) string {
	t := wasmType(res)
	switch op {
	case ir.OLoadsb:
		return t + ".load8_s"
	case ir.OLoadub:
		return t + ".load8_u"
	case ir.OLoadsh:
		return t + ".load16_s"
	case ir.OLoaduh:
		return t + ".load16_u"
	case ir.OLoadsw:
		if res == ir.ClsL {
			return "i64.load32_s"
		}
		return "i32.load"
	case ir.OLoaduw:
		if res == ir.ClsL {
			return "i64.load32_u"
		}
		return "i32.load"
	case ir.OLoadl:
		return "i64.load"
	case ir.OLoads:
		return "f32.load"
	case ir.OLoadd:
		return "f64.load"
	}
	return "unreachable"
}

// storeOp returns the Wasm store mnemonic for a store opcode whose value has
// class val. The opcode fixes the memory width; val picks i32 vs i64.
func storeOp(op ir.Op, val ir.Cls) string {
	t := wasmType(val)
	switch op {
	case ir.OStoreb:
		return t + ".store8"
	case ir.OStoreh:
		return t + ".store16"
	case ir.OStorew:
		if val == ir.ClsL {
			return "i64.store32"
		}
		return "i32.store"
	case ir.OStorel:
		return "i64.store"
	case ir.OStores:
		return "f32.store"
	case ir.OStored:
		return "f64.store"
	}
	return "unreachable"
}

// neg emits negation: floats have a native op, integers subtract from zero.
func (e *emitter) neg(in *ir.Instr) {
	if in.Cls.IsFloat() {
		e.push(in.Args[0])
		e.line("%s.neg", wasmType(in.Cls))
		return
	}
	e.line("%s.const 0", wasmType(in.Cls))
	e.push(in.Args[0])
	e.line("%s.sub", wasmType(in.Cls))
}

// copy moves a value, reinterpreting the bits when the source and destination
// classes differ in kind but share a width.
func (e *emitter) copy(in *ir.Instr) {
	src := e.f.ClassOf(in.Args[0])
	e.push(in.Args[0])
	if src != in.Cls && src.IsFloat() != in.Cls.IsFloat() {
		e.line("%s.reinterpret_%s", wasmType(in.Cls), wasmType(src))
	}
}

// binOp returns the Wasm mnemonic for a binary arithmetic/bitwise op at cls.
func binOp(op ir.Op, cls ir.Cls) string {
	t := wasmType(cls)
	switch op {
	case ir.OAdd:
		return t + ".add"
	case ir.OSub:
		return t + ".sub"
	case ir.OMul:
		return t + ".mul"
	case ir.ODiv:
		if cls.IsFloat() {
			return t + ".div"
		}
		return t + ".div_s"
	case ir.OUDiv:
		return t + ".div_u"
	case ir.ORem:
		return t + ".rem_s"
	case ir.OURem:
		return t + ".rem_u"
	case ir.OAnd:
		return t + ".and"
	case ir.OOr:
		return t + ".or"
	case ir.OXor:
		return t + ".xor"
	case ir.OShl:
		return t + ".shl"
	case ir.OShr:
		return t + ".shr_u"
	case ir.OSar:
		return t + ".shr_s"
	}
	return "unreachable"
}
