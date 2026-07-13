package wasm

import "github.com/evanphx/cg12/ir"

// WebAssembly has no variadic calling convention, so cg12 uses the common one:
// a variadic callee receives a single hidden pointer ($__va) to a buffer holding
// the variadic arguments, packed at their natural alignment. va_list is just
// that pointer; va_start seeds it and va_arg reads and advances it. The caller
// writes the buffer into its own frame and passes its address.

// variadicCall returns, for a direct call to a module variadic function, the
// callee and the variadic argument refs (those past the fixed parameters).
func (e *emitter) variadicCall(in *ir.Instr) (cf *ir.Func, varargs []ir.Ref, ok bool) {
	if e.ctx == nil {
		return nil, nil, false
	}
	callee := in.Arg(0)
	if callee.Kind != ir.RefConst || e.f.Consts[callee.ID].Kind != ir.ConstSym {
		return nil, nil, false
	}
	cf = e.ctx.funcs[e.f.Consts[callee.ID].Sym]
	if cf == nil || !cf.Variadic {
		return nil, nil, false
	}
	fixed := len(cf.Params)
	return cf, in.Args[1+fixed:], true
}

// vaLayout packs variadic arguments at their natural alignment, returning each
// argument's byte offset and the total (8-byte-rounded) buffer size.
func (e *emitter) vaLayout(varargs []ir.Ref) (offs []int, size int) {
	off := 0
	for _, a := range varargs {
		s := e.f.ClassOf(a).Size()
		off = alignUp(off, s)
		offs = append(offs, off)
		off += s
	}
	return offs, alignUp(off, 8)
}

// emitVariadicCall writes the variadic arguments into this frame's va-buffer,
// then calls the function with the fixed arguments followed by the buffer
// pointer as the hidden trailing argument.
func (e *emitter) emitVariadicCall(in *ir.Instr, cf *ir.Func, varargs []ir.Ref) {
	base := e.vaBufOff[in]
	offs, _ := e.vaLayout(varargs)
	for i, a := range varargs {
		e.frameAddr(base + offs[i])
		e.push(a)
		e.line("%s", storeFull(e.f.ClassOf(a)))
	}
	fixed := len(cf.Params)
	for _, a := range in.Args[1 : 1+fixed] {
		e.push(a)
	}
	e.frameAddr(base) // the va-buffer pointer, hidden trailing argument
	e.line("call $%s", sanitize(cf.Name))
	e.store(in)
}

// emitVaStart seeds the va_list at *ap with this frame's va-buffer pointer.
func (e *emitter) emitVaStart(in *ir.Instr) {
	e.pushAddr(in.Args[0]) // &va_list
	e.line("local.get $__va")
	e.line("i32.store")
}

// emitVaArg reads the next variadic argument: it aligns the cursor in the
// va_list up to the value's width, loads the value, and advances the cursor.
func (e *emitter) emitVaArg(in *ir.Instr) {
	size := in.Cls.Size()
	// $__vap = align(*ap, size)
	e.pushAddr(in.Args[0])
	e.line("i32.load")
	e.line("i32.const %d", size-1)
	e.line("i32.add")
	e.line("i32.const %d", -size) // mask ~(size-1) == -size for a power of two
	e.line("i32.and")
	e.line("local.set $__vap")
	// result = load[cls] $__vap
	e.line("local.get $__vap")
	e.line("%s", loadFull(in.Cls))
	e.store(in)
	// *ap = $__vap + size
	e.pushAddr(in.Args[0])
	e.line("local.get $__vap")
	e.line("i32.const %d", size)
	e.line("i32.add")
	e.line("i32.store")
}

// loadFull / storeFull give the full-width load/store mnemonic for a class.
func loadFull(c ir.Cls) string  { return wasmType(c) + ".load" }
func storeFull(c ir.Cls) string { return wasmType(c) + ".store" }

// funcHasVaArg reports whether f reads any variadic argument (so the emitter
// declares the $__vap scratch local).
func funcHasVaArg(f *ir.Func) bool {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Op == ir.OVaArg {
				return true
			}
		}
	}
	return false
}
