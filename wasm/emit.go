package wasm

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// emitter accumulates WAT text for one function with simple indentation.
type emitter struct {
	f     *ir.Func
	sb    *strings.Builder
	ctx   *modCtx
	depth int
	err   error

	// Shadow-stack frame for `alloc` and aggregate-result buffers: the total
	// frame size and each allocation's / result buffer's byte offset within it.
	// frame == 0 when the function needs no frame.
	frame     int
	allocOff  map[*ir.Instr]int
	retBufOff map[*ir.Instr]int // OCall with RetAgg -> its result buffer offset
	vaBufOff  map[*ir.Instr]int // variadic OCall -> its argument buffer offset
}

// emitFunc writes the (func ...) form for f into sb.
func emitFunc(f *ir.Func, sb *strings.Builder, ctx *modCtx) error {
	e := &emitter{f: f, sb: sb, ctx: ctx, depth: 1}
	e.planFrame()
	e.signature()
	e.depth = 2
	e.declareLocals()
	e.prologue()
	if err := e.body(); err != nil {
		return err
	}
	// Every IR block ends in an explicit terminator, so control never reaches
	// the end of the structured body. A trailing `unreachable` states that to
	// the validator, which otherwise demands the declared result on the stack
	// at the implicit function end (an `if` whose arms both diverge does not
	// propagate its unreachability outward).
	if e.f.Start != nil {
		e.line("unreachable")
	}
	e.depth = 1
	e.line(")")
	return e.err
}

// line writes one indented line of WAT.
func (e *emitter) line(format string, args ...any) {
	e.sb.WriteString(strings.Repeat("\t", e.depth))
	fmt.Fprintf(e.sb, format, args...)
	e.sb.WriteByte('\n')
}

// fail records the first error encountered while emitting.
func (e *emitter) fail(format string, args ...any) {
	if e.err == nil {
		e.err = fmt.Errorf(format, args...)
	}
}

// tempName is the WAT identifier for a temporary.
func tempName(t *ir.Temp) string { return "$t" + strconv.Itoa(t.ID) }

// signature emits the opening (func ...) line with params and result. A
// by-value aggregate is passed and returned by pointer (i32): an aggregate
// parameter is a pointer, and an aggregate-returning function takes a hidden
// leading result pointer ($__ret) and yields nothing (sret).
func (e *emitter) signature() {
	var b strings.Builder
	fmt.Fprintf(&b, "(func $%s", sanitize(e.f.Name))
	if e.f.Linkage.Export {
		fmt.Fprintf(&b, " (export \"%s\")", e.f.Name)
	}
	if e.f.RetAgg != nil {
		b.WriteString(" (param $__ret i32)")
	}
	for _, p := range e.f.Params {
		fmt.Fprintf(&b, " (param %s %s)", tempName(p), paramType(p))
	}
	if e.f.Variadic {
		b.WriteString(" (param $__va i32)") // hidden pointer to the variadic buffer
	}
	if e.f.HasRet && e.f.RetAgg == nil {
		fmt.Fprintf(&b, " (result %s)", wasmType(e.f.Retty))
	}
	e.line("%s", b.String())
}

// paramType is the Wasm type of a parameter: a by-value aggregate is a pointer.
func paramType(p *ir.Temp) string {
	if p.Agg != nil {
		return "i32"
	}
	return wasmType(p.Cls)
}

// declareLocals emits a (local ...) declaration for every temporary that is not
// a parameter. Parameters already occupy the leading local slots.
func (e *emitter) declareLocals() {
	isParam := make([]bool, len(e.f.Temps))
	for _, p := range e.f.Params {
		isParam[p.ID] = true
	}
	for _, t := range e.f.Temps {
		if t == nil || isParam[t.ID] {
			continue
		}
		e.line("(local %s %s)", tempName(t), wasmType(t.Cls))
	}
	if e.frame > 0 {
		e.line("(local $__base i32)") // base of this frame in the shadow stack
	}
	if funcHasVaArg(e.f) {
		e.line("(local $__vap i32)") // va_arg cursor scratch
	}
}

// planFrame assigns each stack allocation — and each aggregate-returning call's
// result buffer — an offset within the function's frame, recording the total
// frame size (rounded to 16 bytes).
func (e *emitter) planFrame() {
	e.allocOff = map[*ir.Instr]int{}
	e.retBufOff = map[*ir.Instr]int{}
	e.vaBufOff = map[*ir.Instr]int{}
	off := 0
	for _, b := range e.f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			switch {
			case in.Op.IsAlloc():
				off = alignUp(off, allocAlign(in.Op))
				e.allocOff[in] = off
				off += e.allocBytes(in)
			case in.Op == ir.OCall && in.RetAgg != nil:
				sz, _ := in.RetAgg.Layout()
				off = alignUp(off, 16)
				e.retBufOff[in] = off
				off += sz
			case in.Op == ir.OCall:
				if _, varargs, ok := e.variadicCall(in); ok {
					_, sz := e.vaLayout(varargs)
					off = alignUp(off, 8)
					e.vaBufOff[in] = off
					off += sz
				}
			}
		}
	}
	e.frame = alignUp(off, 16)
}

// prologue reserves this function's frame on the shadow stack: base = sp - frame,
// then sp = base. The frame is released before every return (see emitReturn).
func (e *emitter) prologue() {
	if e.frame == 0 {
		return
	}
	e.line("global.get $__sp")
	e.line("i32.const %d", e.frame)
	e.line("i32.sub")
	e.line("local.tee $__base")
	e.line("global.set $__sp")
}

// emitReturn returns from the function. For an aggregate return it first copies
// the aggregate into the caller's buffer ($__ret) — while the source frame is
// still live — then releases the frame and yields nothing. Otherwise it releases
// the frame and pushes the scalar result when present.
func (e *emitter) emitReturn(v ir.Ref) {
	if e.f.RetAgg != nil {
		size, _ := e.f.RetAgg.Layout()
		e.line("local.get $__ret") // dest
		e.pushAddr(v)              // src (pointer to the aggregate, may be in-frame)
		e.line("i32.const %d", size)
		e.line("memory.copy")
		e.releaseFrame()
		e.line("return")
		return
	}
	e.releaseFrame()
	if !v.IsNone() {
		e.push(v)
	}
	e.line("return")
}

// releaseFrame restores the shadow-stack pointer to the caller's value.
func (e *emitter) releaseFrame() {
	if e.frame == 0 {
		return
	}
	e.line("local.get $__base")
	e.line("i32.const %d", e.frame)
	e.line("i32.add")
	e.line("global.set $__sp")
}

// allocBytes returns the byte size requested by an allocation (its size operand,
// a constant in every case the front ends produce).
func (e *emitter) allocBytes(in *ir.Instr) int {
	if a := in.Arg(0); a.Kind == ir.RefConst {
		return int(e.f.Consts[a.ID].Int)
	}
	return 0
}

// allocAlign returns the alignment an alloc opcode guarantees.
func allocAlign(op ir.Op) int {
	switch op {
	case ir.OAlloc16:
		return 16
	case ir.OAlloc8:
		return 8
	default:
		return 4
	}
}

// alignUp rounds n up to the next multiple of a.
func alignUp(n, a int) int {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}

// body emits the function's executable code by restructuring its CFG into
// WebAssembly's block/loop/if control constructs.
func (e *emitter) body() error {
	if e.f.Start == nil {
		return nil
	}
	return e.structure()
}

// pushAddr leaves r on the stack as an i32 linear-memory address. A pointer kept
// at register width (ClsL, i64 — as parsed QBE IL types every pointer) is
// narrowed to a 32-bit address here, at the access boundary; the truncation is
// lossless because wasm32 addresses fit in 32 bits, while pointer *values* and
// arithmetic stay full-width and correct. A ClsP/ClsW pointer is already i32.
func (e *emitter) pushAddr(r ir.Ref) {
	e.push(r)
	if e.f.ClassOf(r) == ir.ClsL {
		e.line("i32.wrap_i64")
	}
}

// push emits code leaving the value of r on the operand stack.
func (e *emitter) push(r ir.Ref) {
	switch r.Kind {
	case ir.RefTemp:
		e.line("local.get %s", tempName(e.f.Temps[r.ID]))
	case ir.RefConst:
		e.pushConst(e.f.Consts[r.ID])
	default:
		e.fail("wasm: cannot push ref kind %d", r.Kind)
	}
}

// pushConst emits an immediate constant.
func (e *emitter) pushConst(c ir.Const) {
	switch c.Kind {
	case ir.ConstInt:
		if c.Cls.IsFloat() {
			// A floating value written as its raw bit pattern: reconstruct it
			// exactly by reinterpreting the integer bits.
			if c.Cls == ir.ClsS {
				e.line("i32.const %d", int32(c.Int))
				e.line("f32.reinterpret_i32")
			} else {
				e.line("i64.const %d", c.Int)
				e.line("f64.reinterpret_i64")
			}
			return
		}
		e.line("%s.const %d", wasmType(c.Cls), c.Int)
	case ir.ConstFloat:
		e.line("%s.const %s", wasmType(c.Cls), formatFloat(c))
	case ir.ConstSym:
		e.pushSym(c)
	default:
		e.fail("wasm: constant kind %d not yet supported", c.Kind)
	}
}

// pushSym pushes a symbol's value as an i32: a function's table index (a
// function pointer) or a data symbol's linear address.
func (e *emitter) pushSym(c ir.Const) {
	if e.ctx == nil {
		e.fail("wasm: symbol %q needs module context", c.Sym)
		return
	}
	if idx, ok := e.ctx.funcIdx[c.Sym]; ok {
		e.line("i32.const %d", idx) // function pointer = table index (always i32)
		if c.Cls == ir.ClsL {
			e.line("i64.extend_i32_u")
		}
		return
	}
	addr, ok := e.ctx.syms[c.Sym]
	if !ok {
		e.fail("wasm: unknown symbol %q", c.Sym)
		return
	}
	// A register-width symbol address is an i64 constant; a pointer-class one i32.
	if c.Cls == ir.ClsL {
		e.line("i64.const %d", addr+c.Int)
	} else {
		e.line("i32.const %d", addr+c.Int)
	}
}

// formatFloat renders a floating constant as a round-trip-exact WAT literal.
func formatFloat(c ir.Const) string {
	bits := 64
	if c.Cls == ir.ClsS {
		bits = 32
	}
	return strconv.FormatFloat(c.Flt, 'g', -1, bits)
}

// store emits a local.set for an instruction's result, if it has one.
func (e *emitter) store(in *ir.Instr) {
	if in.To.IsNone() {
		return
	}
	e.line("local.set %s", tempName(e.f.Temps[in.To.ID]))
}

// sanitize maps a symbol name to a WAT-legal identifier tail.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_' || r == '.' || r == '$':
			return r
		}
		return '_'
	}, name)
}
