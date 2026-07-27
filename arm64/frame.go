package arm64

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// frameLayout is the stack-frame plan for one function: where everything the
// function needs room for ends up, relative to the frame pointer. It is pure
// computation over the register allocation: the prologue, the epilogue, and
// every frame-relative access all read their offsets from here, so they cannot
// disagree about where a spill slot went.
type frameLayout struct {
	frame       int               // total frame size in bytes (16-aligned)
	outgoing    int               // fixed call area below x29 for managed AAPCS64 calls
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

// computeFrame lays out a function's stack frame from its allocation. Managed
// AAPCS64 functions reserve a fixed outgoing-call area below x29. The frame
// record begins at x29, followed by callee saves, spills, allocas, and the
// variadic save area. Keeping outgoing arguments below the frame record lets SP
// remain stable across calls without allowing a stack argument to overwrite LR.
func computeFrame(f *ir.Func, alloc *allocation) frameLayout {
	lay := frameLayout{allocOff: map[*ir.Instr]int{}}

	used := map[Reg]bool{}
	for _, t := range f.Temps {
		if t.Reg != ir.NoReg {
			if r := Reg(t.Reg); calleeSavedFor(f.UsesGoInternalCallConvention(), r) {
				used[r] = true
			}
		}
	}
	// An inline asm that declares it writes a callee-saved register makes this
	// function responsible for it, exactly as using it would: the ABI promise is
	// to the caller, and it does not care whether the write came from the
	// allocator or from a template. Keeping the allocator out of those registers
	// is a different job (regalloc's avoid set) and does not discharge this one.
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			for _, r := range asmClobberRegs(&b.Instrs[i]) {
				if calleeSavedFor(f.UsesGoInternalCallConvention(), r) {
					used[r] = true
				}
			}
		}
	}
	// Walk the allocation orders rather than the map, so the save order is stable
	// and every register the allocator could hand out (including a platform-ABI
	// function's X26/X28) is a candidate for saving.
	for _, r := range intAllocOrderFor(f) {
		if used[r] {
			lay.calleeSaved = append(lay.calleeSaved, r)
		}
	}
	for _, r := range floatAllocOrder {
		if used[r] {
			lay.calleeSaved = append(lay.calleeSaved, r)
		}
	}

	if f.UsesManagedFrame() {
		lay.outgoing = maxOutgoingCallSize(f)
	}
	off := 16 + 8*len(lay.calleeSaved) // x29/x30 occupy [0,16)
	lay.spillBase = off
	off += alloc.spillBytes

	// Reserve stack for each OAlloc, honouring its alignment. Allocations whose
	// lifetimes are disjoint (per their lifetime.start/end markers) share one slot;
	// allocaGroups maps each such alloc to its group's representative, and the group
	// is laid out once, when its first member is reached.
	groups := allocaGroups(f, analysis.BuildCFG(f))
	groupOff := map[*ir.Instr]int{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(f, in)
				if rep, shared := groups[in]; shared {
					if o, done := groupOff[rep]; done {
						lay.allocOff[in] = o // reuse the group's slot
						continue
					}
					off = roundUp(off, align)
					groupOff[rep] = off
					lay.allocOff[in] = off
					off += size
					continue
				}
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

	lay.frame = roundUp(lay.outgoing+off, 16)
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
	a := newArgAssigner(f.UsesGoInternalCallConvention())
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
