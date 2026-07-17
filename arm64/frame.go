package arm64

import "github.com/evanphx/cg12/ir"

// frameLayout is the stack-frame plan for one function: where everything the
// function needs room for ends up, relative to the frame pointer. It is pure
// computation over the register allocation, so both emitters share it rather
// than deciding separately and risking two different answers.
type frameLayout struct {
	frame       int               // total frame size in bytes (16-aligned)
	calleeSaved []Reg             // callee-saved registers to preserve, in save order
	spillBase   int               // byte offset of the spill area within the frame
	allocOff    map[*ir.Instr]int // OAlloc instruction -> frame offset
	hasDynAlloc bool              // the function has a VLA (dynamic) alloca

	// Variadic support: the register save area, and how much of the argument
	// registers the named parameters already consumed.
	variadic   bool
	gpSaveOff  int // frame offset of the x0..x7 save area
	fpSaveOff  int // frame offset of the v0..v7 save area
	namedGr    int // GP registers used by named parameters
	namedSr    int // SIMD registers used by named parameters
	namedStack int // bytes of stack used by named parameters
}

// computeFrame lays out a function's stack frame from its allocation. The frame
// grows upward from x29: the saved x29/x30 pair, the callee-saved registers,
// the spill area, the allocas, and finally the variadic save area.
func computeFrame(f *ir.Func, alloc *allocation) frameLayout {
	lay := frameLayout{allocOff: map[*ir.Instr]int{}}

	used := map[Reg]bool{}
	for _, t := range f.Temps {
		if t.Reg != ir.NoReg {
			if r := Reg(t.Reg); calleeSavedReg(r) {
				used[r] = true
			}
		}
	}
	// Walk the allocation orders rather than the map, so the save order is stable.
	for _, r := range intAllocOrder {
		if used[r] {
			lay.calleeSaved = append(lay.calleeSaved, r)
		}
	}
	for _, r := range floatAllocOrder {
		if used[r] {
			lay.calleeSaved = append(lay.calleeSaved, r)
		}
	}

	off := 16 + 8*len(lay.calleeSaved) // x29/x30 occupy [0,16)
	lay.spillBase = off
	off += alloc.spillBytes

	// Reserve stack for each OAlloc, honouring its alignment.
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(f, in)
				off = roundUp(off, align)
				lay.allocOff[in] = off
				off += size
			}
			if in.Op == ir.OAllocN {
				lay.hasDynAlloc = true
			}
		}
	}

	if f.Variadic {
		lay.variadic = true
		lay.namedGr, lay.namedSr, lay.namedStack = computeNamedCounts(f)
		off = roundUp(off, 8)
		lay.gpSaveOff = off
		off += 8 * 8 // x0..x7
		off = roundUp(off, 16)
		lay.fpSaveOff = off
		off += 8 * 16 // v0..v7 (16-byte stride)
	}

	lay.frame = roundUp(off, 16)
	return lay
}

// locOf is where a value lives: a register, a spill slot, or an immediate. It
// reports false for an operand that is not a movable value -- a symbol's address
// has to be materialized before it can be moved, so it has no location of its
// own, and the caller decides how to complain.
func locOf(f *ir.Func, ref ir.Ref) (loc, bool) {
	switch ref.Kind {
	case ir.RefTemp:
		t := f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return loc{reg: Reg(t.Reg), size: t.Cls.Size()}, true
		}
		return loc{mem: true, slot: t.Slot, size: t.Cls.Size()}, true
	case ir.RefConst:
		switch c := f.Consts[ref.ID]; c.Kind {
		case ir.ConstFloat:
			// Carry the bit pattern; the move goes via a GPR into the SIMD register.
			return loc{imm: true, val: floatBits(c), size: c.Cls.Size()}, true
		case ir.ConstInt:
			return loc{imm: true, val: c.Int, size: c.Cls.Size()}, true
		}
	}
	return loc{}, false
}

// isConstSym reports whether ref is a symbol's address, which is materialized
// rather than moved.
func isConstSym(f *ir.Func, ref ir.Ref) bool {
	return ref.Kind == ir.RefConst && f.Consts[ref.ID].Kind == ir.ConstSym
}
