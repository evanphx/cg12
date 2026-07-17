package arm64

import "github.com/evanphx/cg12/ir"

// frameLayout is the stack-frame plan for one function: where everything the
// function needs room for ends up, relative to the frame pointer. It is pure
// computation over the register allocation: the prologue, the epilogue, and
// every frame-relative access all read their offsets from here, so they cannot
// disagree about where a spill slot went.
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

// allocShape returns the alignment and byte size of a stack allocation.
func allocShape(f *ir.Func, in *ir.Instr) (align, size int) {
	switch in.Op {
	case ir.OAlloc4:
		align = 4
	case ir.OAlloc8:
		align = 8
	default:
		align = 16
	}
	size = align
	if a := in.Arg(0); a.Kind == ir.RefConst {
		if c := f.Consts[a.ID]; c.Kind == ir.ConstInt && c.Int > 0 {
			size = int(c.Int)
		}
	}
	return align, roundUp(size, align)
}

func roundUp(n, a int) int {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}

// computeNamedCounts replays argument assignment over a variadic function's named
// parameters, returning how many GP/SIMD registers and stack bytes they used.
func computeNamedCounts(f *ir.Func) (ngrn, nsrn, stack int) {
	var a argAssigner
	for _, p := range f.Params {
		if p.Agg != nil {
			cls := classifyAgg(p.Agg)
			switch cls.kind {
			case aggGP:
				a.assignGP(cls.nregs, cls.size)
			case aggHFA:
				a.assignHFA(cls.nregs, cls.size)
			default:
				a.assign(ir.ClsL)
			}
			continue
		}
		a.assign(p.Cls)
	}
	return a.ngrn, a.nsrn, a.nsaa
}
