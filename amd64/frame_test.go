package amd64

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin frame.go's three contracts: the outgoing/frameTop geometry the
// plan names as B4 -> B1/B2, the save order of the callee-saved list, and the
// frame-size effect of alloca colouring. All three are things whose failure mode
// is silent -- a save list that reorders corrupts the stack rather than failing a
// test, and a frame-top offset that is off by eight reads the wrong argument --
// so they are asserted directly rather than inferred from emitted bytes.

// frameFunc returns a bare platform-ABI function with an entry block.
func frameFunc(name string) *ir.Func {
	f := ir.NewModule().NewFuncVoid(name)
	f.Entry()
	return f
}

// useReg pins a fresh temporary to a physical register, which is how
// computeFrame learns the allocator used it.
func useReg(f *ir.Func, r Reg) {
	ref := f.NewTemp("", ir.ClsL)
	t := f.Temp(ref)
	t.Reg = int(r)
}

// ---------------------------------------------------------------------------
// The outgoing / frameTop contract
// ---------------------------------------------------------------------------

// TestFrameTopIsTheIncomingArgumentBase pins frameTop() to the one number the
// geometry allows: RBP+16, past the saved RBP and the return address the CALL
// pushed. The emitter spells that literal at three sites today (the incoming
// stack-parameter address, callerSP, and va_start's overflow_arg_area), and
// goabi.go names the same offset from the argument frame's side, so all three
// spellings are asserted equal here rather than left to agree by coincidence.
func TestFrameTopIsTheIncomingArgumentBase(t *testing.T) {
	lay := computeFrame(frameFunc("f"), &allocation{})
	assert.Equal(t, int32(16), lay.frameTop())
	assert.Equal(t, int32(goArgumentFrameFromFramePointer), lay.frameTop())

	// The entry-SP origin is one push lower. A stack-growth prologue runs before
	// `push %rbp` and must use that one instead.
	assert.Equal(t, int(lay.frameTop()), goArgumentFrameFromEntrySP+8)
}

// TestFrameTopDoesNotVaryWithTheFrame is the negative half: on amd64 frameTop()
// is a constant *because* the frame hangs below its anchor, which is what makes
// arm64's `frame - outgoing` arithmetic wrong to port. If someone ever makes it
// depend on the layout, this fails.
func TestFrameTopDoesNotVaryWithTheFrame(t *testing.T) {
	plain := computeFrame(frameFunc("plain"), &allocation{})

	big := frameFunc("big")
	useReg(big, RBX)
	big.Entry().Instrs = append(big.Entry().Instrs, ir.Instr{Op: ir.OCall, Aux: 64})
	heavy := computeFrame(big, &allocation{spillBytes: 128})

	require.NotEqual(t, plain.frame, heavy.frame)
	assert.Equal(t, plain.frameTop(), heavy.frameTop())
}

// TestOutgoingIsTheLargestCallArea pins the outgoing field: it is the maximum
// stacked-argument area over the function's calls (OCall.Aux, set by lowerCalls
// from argAssigner.stackBytes), it lives at the bottom of the frame, and the
// frame is the 16-aligned sum of everything else and it.
func TestOutgoingIsTheLargestCallArea(t *testing.T) {
	f := frameFunc("caller")
	e := f.Entry()
	e.Instrs = append(e.Instrs,
		ir.Instr{Op: ir.OCall, Aux: 16},
		ir.Instr{Op: ir.OCall, Aux: 48},
		ir.Instr{Op: ir.OCall, Aux: 32},
	)
	useReg(f, RBX) // one callee save, so the local area is not empty

	lay := computeFrame(f, &allocation{spillBytes: 24})
	assert.Equal(t, 48, lay.outgoing)

	// local = 8 (one callee save) + 24 (spills); frame = roundUp(local+outgoing, 16).
	assert.Equal(t, roundUp(8+24+48, 16), lay.frame)

	// The area is at the bottom of the frame: RSP+off and RBP+outgoingAddr(off)
	// are the same byte, since RSP == RBP-frame.
	assert.Equal(t, int32(-lay.frame), lay.outgoingAddr(0))
	assert.Equal(t, int32(48-lay.frame), lay.outgoingAddr(48))

	// The outgoing area must not collide with the local area, which grows down
	// from RBP: the deepest local byte is at -(8+24) and the top of the outgoing
	// area is at outgoingAddr(outgoing).
	assert.LessOrEqual(t, int(lay.outgoingAddr(lay.outgoing)), -(8 + 24))
}

// TestOutgoingIsZeroWithoutCalls keeps the leaf case honest: a function that
// makes no call reserves nothing, and the frame is just its locals.
func TestOutgoingIsZeroWithoutCalls(t *testing.T) {
	lay := computeFrame(frameFunc("leaf"), &allocation{spillBytes: 8})
	assert.Equal(t, 0, lay.outgoing)
	assert.Equal(t, 16, lay.frame)
}

// ---------------------------------------------------------------------------
// Callee-save ordering
// ---------------------------------------------------------------------------

// TestCalleeSaveOrderIsTheAllocationOrder pins the order itself. It is the
// System V allocation order filtered to the callee-saved registers, which is
// also ascending register number today -- the two coincide, and asserting the
// literal list is what would catch a change to intAllocOrder silently reordering
// every prologue.
func TestCalleeSaveOrderIsTheAllocationOrder(t *testing.T) {
	assert.Equal(t, []Reg{RBX, R12, R13, R14, R15}, calleeSaveOrder(ir.CallConvPlatform))

	// Go ABIInternal preserves nothing, so there is no save list at all.
	assert.Empty(t, calleeSaveOrder(ir.CallConvGoInternal))
}

// TestCalleeSaveOrderCoversEveryCalleeSavedRegister is the assertion the
// trailing sweep in calleeSaveOrder exists for: every register calleeSavedFor
// reports must appear, so no used register can be dropped from the save list.
// arm64 drops silently here; the sweep makes that impossible, and this test
// records that today the allocation order already covers everything, so the
// sweep contributes nothing and the two backends agree.
func TestCalleeSaveOrderCoversEveryCalleeSavedRegister(t *testing.T) {
	order := calleeSaveOrder(ir.CallConvPlatform)
	inOrder := map[Reg]bool{}
	for _, r := range order {
		require.False(t, inOrder[r], "register %d appears twice in the save order", int(r))
		inOrder[r] = true
	}
	for r := Reg(0); r < XMM0+16; r++ {
		if calleeSavedFor(ir.CallConvPlatform, r) {
			assert.True(t, inOrder[r], "callee-saved register %d is missing from the save order", int(r))
		}
	}

	// And the allocation order alone already covers them, so the sweep is a
	// backstop rather than load-bearing. If this ever fails, the sweep started
	// mattering and the prologue changed shape.
	fromAlloc := map[Reg]bool{}
	for _, r := range intAllocOrderFor(ir.CallConvPlatform) {
		fromAlloc[r] = true
	}
	for _, r := range floatAllocOrderFor(ir.CallConvPlatform) {
		fromAlloc[r] = true
	}
	for _, r := range order {
		assert.True(t, fromAlloc[r], "register %d is saved but is outside the allocation order", int(r))
	}
}

// TestCalleeSaveListIsIndependentOfDiscoveryOrder is the real hazard: the save
// list indexes the save slots (savedAddr(k)), so if it depended on which
// register the allocator reached first -- or on Go's randomized map iteration --
// two computations of the same frame could disagree and the epilogue would
// restore registers from each other's slots. Declaring the temps in every
// rotation must produce the identical list.
func TestCalleeSaveListIsIndependentOfDiscoveryOrder(t *testing.T) {
	regs := []Reg{RBX, R12, R13, R14, R15}
	want := []Reg{RBX, R12, R13, R14, R15}
	for rot := range regs {
		f := frameFunc("rot")
		for i := range regs {
			useReg(f, regs[(i+rot)%len(regs)])
		}
		// Repeat within a rotation too: the old implementation ranged a map, and a
		// single sample can pass by luck.
		for attempt := 0; attempt < 8; attempt++ {
			assert.Equal(t, want, computeFrame(f, &allocation{}).calleeSaved)
		}
	}
}

// TestInlineAsmClobberIsSavedInOrder checks the other way a register enters the
// save set. An asm clobber is not an allocation, so it is exactly the case a
// list built by walking the allocation order could drop.
func TestInlineAsmClobberIsSavedInOrder(t *testing.T) {
	f := frameFunc("cpuid")
	useReg(f, R14)
	f.Entry().Instrs = append(f.Entry().Instrs, ir.Instr{
		Op:  ir.OAsm,
		Asm: &ir.AsmOp{Clobbers: []string{"rbx"}},
	})
	lay := computeFrame(f, &allocation{})
	assert.Equal(t, []Reg{RBX, R14}, lay.calleeSaved)
	assert.Equal(t, int32(-8), lay.savedAddr(0))
	assert.Equal(t, int32(-16), lay.savedAddr(1))
	assert.Equal(t, 16, lay.spillBase)
}

// ---------------------------------------------------------------------------
// Alloca colouring
// ---------------------------------------------------------------------------

// Interference is block-granular, inherited deliberately from arm64: two
// allocas that are both live *anywhere* in the same block are treated as
// overlapping, even if one's lifetime.end precedes the other's
// lifetime.start. Sharing therefore happens across control flow -- the arms of
// an if, the bodies of successive loops -- and never between two scopes in one
// straight-line block. Every test below is written in blocks for that reason,
// and a test that puts two disjoint scopes in one block is asserting the
// conservative answer, not a bug.

// allocaWithLifetime appends an alloca bracketed by lifetime markers, in its own
// block so its live region is a whole block.
func allocaWithLifetime(f *ir.Func, pred *ir.Block, align, size int) *ir.Block {
	b := f.NewBlock("")
	pred.Goto(b)
	addr := b.Alloc(align, size)
	b.LifeStart(addr)
	b.LifeEnd(addr)
	return b
}

// stripLifetimes removes the lifetime markers from a function. computeFrame is
// the only thing downstream of register allocation that reads them (the emitter
// treats both as no-ops), so a layout computed after stripping is exactly the
// layout this backend produced before colouring existed -- which makes it a
// sound "before" for a before/after frame-size comparison.
func stripLifetimes(f *ir.Func) {
	for _, b := range f.Blocks {
		kept := make([]ir.Instr, 0, len(b.Instrs))
		for _, in := range b.Instrs {
			if in.Op.IsLifetime() {
				continue
			}
			kept = append(kept, in)
		}
		b.Instrs = kept
	}
}

// TestAllocaColouringSharesDisjointSlots is the direct statement: four
// same-shaped buffers whose lifetimes never overlap occupy one slot, not four,
// and the frame shrinks accordingly.
func TestAllocaColouringSharesDisjointSlots(t *testing.T) {
	const n, size = 4, 64
	f := frameFunc("disjoint")
	b := f.Entry()
	for i := 0; i < n; i++ {
		b = allocaWithLifetime(f, b, 16, size)
	}
	b.RetVoid()

	coloured := computeFrame(f, &allocation{})
	offsets := map[int]bool{}
	for _, off := range coloured.allocOff {
		offsets[off] = true
	}
	require.Len(t, coloured.allocOff, n)
	assert.Len(t, offsets, 1, "disjoint allocas should share a single slot")

	stripLifetimes(f)
	plain := computeFrame(f, &allocation{})
	require.Len(t, plain.allocOff, n)

	assert.Equal(t, roundUp(n*size, 16), plain.frame)
	assert.Equal(t, roundUp(size, 16), coloured.frame)
	assert.Less(t, coloured.frame, plain.frame,
		"colouring must shrink the frame: %d bytes coloured vs %d uncoloured", coloured.frame, plain.frame)
}

// TestAllocaColouringKeepsOverlappingSlotsApart is the safety half. Two buffers
// whose lifetimes nest must not share, or the inner one would scribble on the
// outer one's live storage.
func TestAllocaColouringKeepsOverlappingSlotsApart(t *testing.T) {
	f := frameFunc("nested")
	e := f.Entry()
	outer := e.Alloc(16, 64)
	e.LifeStart(outer)
	inner := e.Alloc(16, 64)
	e.LifeStart(inner)
	e.LifeEnd(inner)
	e.LifeEnd(outer)
	e.RetVoid()

	lay := computeFrame(f, &allocation{})
	offsets := map[int]bool{}
	for _, off := range lay.allocOff {
		offsets[off] = true
	}
	assert.Len(t, offsets, 2, "overlapping lifetimes must not share a slot")
	assert.Equal(t, roundUp(2*64, 16), lay.frame)
}

// TestAllocaColouringNeedsBothMarkers pins the conservative direction: an alloca
// with only a start has an unbounded live region and keeps a private slot, so a
// frontend that emits half a pair cannot lose storage. Two fully-marked buffers
// beside it still share, which is what makes the assertion about the half-marked
// one rather than about colouring being off.
func TestAllocaColouringNeedsBothMarkers(t *testing.T) {
	f := frameFunc("halfmarked")
	b := allocaWithLifetime(f, f.Entry(), 16, 32)
	b = allocaWithLifetime(f, b, 16, 32)
	last := f.NewBlock("")
	b.Goto(last)
	open := last.Alloc(16, 32)
	last.LifeStart(open)
	last.RetVoid()

	lay := computeFrame(f, &allocation{})
	require.Len(t, lay.allocOff, 3)
	offsets := map[int]bool{}
	for _, off := range lay.allocOff {
		offsets[off] = true
	}
	assert.Len(t, offsets, 2, "the half-marked alloca must keep a private slot")
}

// TestAllocaColouringRespectsShape checks that a group never mixes sizes, which
// is what lets computeFrame place a group once and never widen it. Two 16-byte
// and two 64-byte buffers, all disjoint, must collapse to one slot each and not
// to one slot overall.
func TestAllocaColouringRespectsShape(t *testing.T) {
	f := frameFunc("shapes")
	b := f.Entry()
	for _, size := range []int{16, 64, 16, 64} {
		b = allocaWithLifetime(f, b, 16, size)
	}
	b.RetVoid()

	lay := computeFrame(f, &allocation{})
	sizes := map[int]int{}
	for in, off := range lay.allocOff {
		_, size := allocShape(f, in)
		if prev, seen := sizes[off]; seen {
			assert.Equal(t, prev, size, "a colour group mixed sizes")
		}
		sizes[off] = size
	}
	assert.Len(t, sizes, 2)
	assert.Equal(t, roundUp(16+64, 16), lay.frame)
}

// TestAllocaColouringIsDeterministic runs the same function through the layout
// repeatedly. Frame offsets are observable in the emitted bytes, so a colouring
// that depended on map iteration would make compilation non-reproducible; the
// group index assignment is program order for exactly this reason.
func TestAllocaColouringIsDeterministic(t *testing.T) {
	build := func() *ir.Func {
		f := frameFunc("determinism")
		b := f.Entry()
		for i := 0; i < 6; i++ {
			b = allocaWithLifetime(f, b, 16, 32)
		}
		live := b.Alloc(16, 32)
		b.LifeStart(live)
		b = allocaWithLifetime(f, b, 16, 32)
		b.LifeEnd(live)
		b.RetVoid()
		return f
	}

	want := computeFrame(build(), &allocation{})
	for attempt := 0; attempt < 32; attempt++ {
		got := computeFrame(build(), &allocation{})
		assert.Equal(t, want.frame, got.frame)
	}
}
