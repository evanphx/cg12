package amd64

import (
	"sort"

	"github.com/evanphx/cg12/ir"
)

// frameLayout is the stack-frame plan for one function: the prologue, the
// epilogue, and every frame access read their offsets from here, so they cannot
// disagree about where a spill slot went. All offsets are relative to RBP (which
// points at the saved RBP).
type frameLayout struct {
	calleeSaved []Reg             // callee-saved registers to preserve, in save order
	spillBase   int               // bytes below RBP where spill slots begin
	allocOff    map[*ir.Instr]int // each stack allocation's distance below RBP
	frame       int               // bytes subtracted from RSP (16-aligned)
	regSaveDist int               // variadic register save area, at [rbp - regSaveDist]
}

// computeFrame lays out a function's stack frame from its allocation.
func computeFrame(f *ir.Func, alloc *allocation) frameLayout {
	var lay frameLayout
	lay.allocOff = map[*ir.Instr]int{}

	// Which registers this function owes its caller is a property of the
	// convention its body is emitted against, and must be the same answer the
	// allocator used when it chose them (gcalloc's colorGraph.cc). Under Go
	// ABIInternal the set is empty and no register is saved at all.
	cc := emissionConvention(f)

	// Collect the callee-saved registers the allocator actually used.
	used := map[Reg]bool{}
	for _, t := range f.Temps {
		if t.Reg != ir.NoReg && calleeSavedFor(cc, Reg(t.Reg)) {
			used[Reg(t.Reg)] = true
		}
	}
	// An inline asm that declares it writes a callee-saved register makes this
	// function responsible for it, exactly as using it would: the ABI promise is
	// to the caller, and it does not care whether the write came from the
	// allocator or from a template. Keeping the allocator out of those registers
	// is a different job and does not discharge this one -- which is why the
	// cpuid idiom (: "rbx") needs both.
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			for _, r := range asmClobberRegs(&b.Instrs[i]) {
				if calleeSavedFor(cc, r) {
					used[r] = true
				}
			}
		}
	}
	for r := range used {
		lay.calleeSaved = append(lay.calleeSaved, r)
	}
	sort.Slice(lay.calleeSaved, func(i, j int) bool { return lay.calleeSaved[i] < lay.calleeSaved[j] })

	calleeArea := 8 * len(lay.calleeSaved)
	lay.spillBase = calleeArea
	acc := calleeArea + alloc.spillBytes

	// Place each stack allocation below the spills.
	maxCall := 0
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(f, in)
				acc += size
				acc = roundUp(acc, align)
				lay.allocOff[in] = acc
			}
			if in.Op == ir.OCall && int(in.Aux) > maxCall {
				maxCall = int(in.Aux)
			}
		}
	}
	if f.Variadic {
		acc += vaRegSaveSz
		lay.regSaveDist = acc
	}
	lay.frame = roundUp(acc+maxCall, 16)
	return lay
}

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

func (m *mc) planFrame() { m.frameLayout = computeFrame(m.f, m.alloc) }

// slotAddr returns the RBP-relative address of spill slot s.
func (l *frameLayout) slotAddr(s int) int32 { return int32(-(l.spillBase + 8 + s)) }

// savedAddr returns the RBP-relative address of the k-th saved callee register.
func (l *frameLayout) savedAddr(k int) int32 { return int32(-8 * (k + 1)) }
