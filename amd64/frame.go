package amd64

import (
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// frameLayout is the stack-frame plan for one function: the prologue, the
// epilogue, and every frame access read their offsets from here, so they cannot
// disagree about where a spill slot went.
//
// # Geometry
//
// The frame is anchored on RBP, which the prologue sets with the standard
// `push %rbp; mov %rsp,%rbp` pair, so RBP points at the saved RBP and RSP stays
// at RBP-frame for the whole body. Growing downward from RBP:
//
//	RBP+16 .. (caller's frame)  incoming stack arguments      <- frameTop()
//	RBP+8                       return address (pushed by CALL)
//	RBP+0                       saved RBP
//	RBP-8 ..                    callee-saved registers        <- savedAddr(k)
//	  ..                        spill slots                   <- slotAddr(s)
//	  ..                        allocas                       <- allocOff[in]
//	  ..                        variadic register save area   <- regSaveDist
//	  ..                        (alignment padding)
//	RBP-frame+outgoing          top of the outgoing area
//	  ..                        stacked outgoing arguments    <- [RSP, RSP+outgoing)
//	RBP-frame == RSP            base of the outgoing area     <- outgoingAddr(0)
//
// Everything the function owns is at a *negative* displacement from RBP; only
// the outgoing argument area is naturally addressed off RSP, at non-negative
// offsets, because that is the base a callee measures its argument frame from
// (goabi.go's base and sign convention).
//
// Two consequences worth stating, because arm64's frameLayout has the opposite
// shape and a port that keeps arm64's arithmetic would be silently wrong:
//
//   - arm64 anchors on x29 at the *bottom* of its local area and counts upward,
//     so its callee saves, spills and allocas are at positive offsets and its
//     outgoing area sits below x29. amd64's anchor is at the *top*, so every
//     local offset here is negated on use.
//   - There is no hand-reserved frame-chain link. The CALL pushed the return
//     address itself, which is why conventionABI(...).stackLinkBytes is zero
//     under both conventions and why nothing here adds 8 for it.
type frameLayout struct {
	calleeSaved []Reg             // callee-saved registers to preserve, in save order
	spillBase   int               // bytes below RBP where spill slots begin
	allocOff    map[*ir.Instr]int // each stack allocation's distance below RBP
	frame       int               // bytes subtracted from RSP (16-aligned)
	outgoing    int               // stacked-argument area at [RSP, RSP+outgoing)
	regSaveDist int               // variadic register save area, at [rbp - regSaveDist]
}

// frameTop is the RBP-relative byte offset of the top of the frame: the first
// byte this function's frame does not own, which is also the base of its
// incoming argument area and the value of RSP in the caller immediately before
// the CALL.
//
// This is the B4 -> B1/B2 contract named in AMD64_PARITY_PLAN.md, and on amd64
// it is a constant. That is a property of the geometry above, not an oversight:
// the frame is anchored at its top, so the only things between RBP and the
// caller's stack are the saved RBP and the return address the CALL pushed --
// eight bytes each, under both calling conventions. arm64's frameTop() varies
// (frame - outgoing) purely because x29 is anchored at the bottom instead.
//
// It is exposed as a layout accessor anyway, for two reasons. It gives the
// concept one name, so `16` stops being spelled as a literal at the three sites
// that mean it (the incoming stack-parameter address, the callerSP intrinsic,
// and va_start's overflow_arg_area). And it makes the amd64 and arm64 emitters
// read the same at those sites, which is what stops a later port from
// reintroducing arm64's arithmetic.
//
// Related origins, so a consumer picks deliberately rather than by accident:
//
//   - frameTop()                          from RBP, after the prologue's push
//   - goArgumentFrameFromFramePointer     the same number, named from the
//     argument frame's side; use that spelling for a goRegisterSpill.offset
//   - goArgumentFrameFromEntrySP          from RSP at entry, *before* the push
//     -- this is the one a stack-growth prologue wants, since it runs first
//   - frame + frameTop()                  from RSP inside the body
//
// A managed-frame prologue (B2) must use goArgumentFrameFromEntrySP for the
// argument home slots it writes before `push %rbp`, and frameTop() only after.
func (l *frameLayout) frameTop() int32 { return goArgumentFrameFromFramePointer }

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
	// Emit the save list in a fixed order that is a property of the convention
	// alone -- never of map iteration, and never of which register the allocator
	// happened to reach for first. calleeSaved indexes the save slots
	// (savedAddr(k)), so an order that could vary between two computations of the
	// same frame would be a silent stack corruption rather than a test failure.
	for _, r := range calleeSaveOrder(cc) {
		if used[r] {
			lay.calleeSaved = append(lay.calleeSaved, r)
		}
	}

	calleeArea := 8 * len(lay.calleeSaved)
	lay.spillBase = calleeArea
	acc := calleeArea + alloc.spillBytes

	// Place each stack allocation below the spills. Allocations whose lifetimes
	// are disjoint (per their lifetime.start/end markers) share one slot:
	// allocaGroups maps each such alloca to its group's representative, and the
	// group's slot is laid out once, when its first member is reached in program
	// order. Members of a group have identical size and alignment, so reuse is
	// exact and the slot never needs widening.
	//
	// acc is a distance *below* RBP, so a slot is claimed by growing acc past the
	// object and then rounding: the object occupies [RBP-acc, RBP-acc+size), and
	// rounding acc up to the alignment aligns its base, because RBP is itself
	// 16-aligned (entry RSP is 8 mod 16 after the CALL, and the prologue's push
	// makes it 0).
	groups := allocaGroups(f, analysis.BuildCFG(f))
	groupOff := map[*ir.Instr]int{}
	maxCall := 0
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(f, in)
				if rep, shared := groups[in]; shared {
					off, placed := groupOff[rep]
					if !placed {
						acc = roundUp(acc+size, align)
						off = acc
						groupOff[rep] = off
					}
					lay.allocOff[in] = off
				} else {
					acc = roundUp(acc+size, align)
					lay.allocOff[in] = acc
				}
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
	// The outgoing area is reserved unconditionally, unlike arm64, which reserves
	// it only for managed frames and otherwise moves SP around each call
	// (dynamicAAPCSFrame). amd64 has no such path: emitArgs always writes stacked
	// arguments at [RSP + OCall.Aux], so the room has to be in the frame. A tail
	// call contributes nothing, because lowerCalls rejects a tail call that has
	// stack arguments, leaving its Aux at zero.
	lay.outgoing = maxCall
	lay.frame = roundUp(acc+lay.outgoing, 16)
	return lay
}

// calleeSaveOrder returns the order the prologue saves callee-saved registers
// in, under one calling convention. It is a total order over every register that
// could ever need saving, so filtering it by the used set (computeFrame) yields
// a save list that depends only on *which* registers are live, never on how they
// were discovered.
//
// The order is the allocation order, integer bank then float bank, which is the
// same rule arm64 uses and keeps the save list in the order a reader of
// intAllocOrderFor expects. Under Go ABIInternal calleeSavedFor is false for
// every register, so the result is empty.
//
// The trailing sweep is the part arm64 does not have, and it is deliberate.
// arm64 builds its save list by walking the allocation orders and dropping
// anything not found there; a callee-saved register that is outside the
// allocation order but reachable another way -- an inline-asm clobber is the
// live example, since asmClobberRegs answers for a hand-written template and not
// for the allocator -- would then be silently not saved, which is an ABI
// violation with no diagnostic. Appending the leftovers in register order
// instead makes the list total by construction, and
// TestCalleeSaveOrderCoversEveryCalleeSavedRegister pins that today there are no
// leftovers, so the sweep costs nothing and exists to stay correct if the
// allocation order is ever narrowed.
func calleeSaveOrder(cc ir.CallConvention) []Reg {
	var order []Reg
	seen := map[Reg]bool{}
	add := func(r Reg) {
		if !calleeSavedFor(cc, r) || seen[r] {
			return
		}
		seen[r] = true
		order = append(order, r)
	}
	for _, r := range intAllocOrderFor(cc) {
		add(r)
	}
	for _, r := range floatAllocOrderFor(cc) {
		add(r)
	}
	var rest []Reg
	for r := range calleeSaved {
		if calleeSavedFor(cc, r) && !seen[r] {
			rest = append(rest, r)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(order, rest...)
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

// outgoingAddr returns the RBP-relative address of byte off within the outgoing
// argument area, for a consumer that has RBP but not RSP to hand. The area is at
// the bottom of the frame, so RSP+off and RBP+outgoingAddr(off) name the same
// byte; the emitter uses the RSP form (emitArgs), and this exists so the
// equivalence is written down once rather than re-derived.
func (l *frameLayout) outgoingAddr(off int) int32 { return int32(off - l.frame) }
