package amd64

import (
	"testing"

	"github.com/evanphx/cg12/ir"
)

// These tests cover AMD64_PARITY_PLAN B1's wiring: the four consumers that have
// to agree about a function's register file -- graph colouring (gcalloc.go), the
// callee-save frame (frame.go), caller-save insertion (callersave.go) and the
// emitter's scratch pair (mc.go) -- now read their tables through the single
// switch emissionConvention.
//
// The switch returns the platform ABI for every function today, so the
// ABIInternal rows cannot be reached by compiling anything. They are exercised
// here by calling the per-convention functions with an explicit convention, the
// same shape convention_test.go uses for B0: the point is that the row is right
// *before* B1 flips the switch, since the flip is a one-line change with nothing
// left to catch a wrong table.

// allocatableRegs is every register either convention could hand out, used to
// check a predicate exhaustively rather than at a hand-picked sample.
func allocatableRegs() []Reg {
	regs := make([]Reg, 0, 32)
	for r := RAX; r <= R15; r++ {
		regs = append(regs, r)
	}
	for i := 0; i < 16; i++ {
		regs = append(regs, XMM(i))
	}
	return regs
}

// ---------------------------------------------------------------------------
// The switch itself
// ---------------------------------------------------------------------------

// TestEmissionConventionIsTheFunctionsOwn is the guard on the whole change,
// rewritten by B1 when the flip landed. It used to assert the opposite -- that
// this returned the platform ABI for every function -- because before the
// matching lowering existed a per-function ABIInternal register file would have
// sat underneath System V argument assignment and miscompiled every closure.
//
// Now lowerParams, lowerReturns and lowerCalls read this same switch, so a
// function goc marked CallConvGoInternal really is emitted ABIInternal end to
// end. If this test fails because emissionConvention returns the platform ABI
// again, the lowering must have been reverted with it: the two are one change.
func TestEmissionConventionIsTheFunctionsOwn(t *testing.T) {
	cases := []struct {
		name string
		f    *ir.Func
		want ir.CallConvention
	}{
		{"unannotated", &ir.Func{}, ir.CallConvPlatform},
		{"platform", &ir.Func{CallConv: ir.CallConvPlatform}, ir.CallConvPlatform},
		{"goc-marked closure", &ir.Func{CallConv: ir.CallConvGoInternal}, ir.CallConvGoInternal},
	}
	for _, c := range cases {
		if got := emissionConvention(c.f); got != c.want {
			t.Errorf("emissionConvention(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestEmissionConventionDoesNotDecideCallConventions separates the two questions
// that used to have the same answer. A body's register file comes from
// emissionConvention; a call's argument placement comes from its callee. A
// platform-ABI function calling a closure -- the ordinary shape in goc output --
// must get System V for its own frame and ABIInternal for that call, and the two
// conventions share no integer argument register at all, so conflating them
// misplaces every argument.
func TestEmissionConventionDoesNotDecideCallConventions(t *testing.T) {
	caller := &ir.Func{Name: "caller", CallConv: ir.CallConvPlatform}
	closure := ir.Instr{Op: ir.OCall, CallConv: ir.CallConvGoInternal, CallConvSet: true}
	conventions := calleeConventions{}

	if got := emissionConvention(caller); got != ir.CallConvPlatform {
		t.Fatalf("caller body convention = %d, want platform", got)
	}
	if got := conventions.forCall(caller, &closure); got != ir.CallConvGoInternal {
		t.Errorf("closure call resolved to %d, want Go ABIInternal", got)
	}
	if conventionABI(ir.CallConvPlatform).intArgRegs[0] == conventionABI(ir.CallConvGoInternal).intArgRegs[0] {
		t.Error("the two conventions share their first integer argument register; " +
			"the distinction this test guards would be unobservable")
	}
}

// TestEveryConsumerRoutesThroughEmissionConvention checks the property the switch
// exists for: all four consumers must produce the *same* convention's answers for
// one function. It is written against the tables rather than against emitted code
// so it keeps meaning after the B1 flip, when the four would otherwise be free to
// disagree.
func TestEveryConsumerRoutesThroughEmissionConvention(t *testing.T) {
	f := &ir.Func{CallConv: ir.CallConvGoInternal}
	cc := emissionConvention(f)

	// gcalloc: the pools a colorGraph hands out.
	g := newColorGraph(f)
	if g.cc != cc {
		t.Errorf("colorGraph.cc = %d, want %d (emissionConvention's answer)", g.cc, cc)
	}

	// mc: the emitter's scratch pair, and the selector built from it.
	m := &mc{f: f, scratchRegs: scratchRegsFor(cc)}
	if m.scratchRegs != scratchRegsFor(cc) {
		t.Error("mc.scratchRegs does not match the function's convention")
	}
	if s := m.sel(); s.scratchRegs != m.scratchRegs {
		t.Errorf("xsel built by mc.sel carries %+v, want the emitter's %+v", s.scratchRegs, m.scratchRegs)
	}

	// frame and callersave read calleeSavedFor / callerClobberedForConv directly;
	// the two must be exact complements or a register would be both preserved by
	// the prologue and saved around every call.
	for _, r := range allocatableRegs() {
		if calleeSavedFor(cc, r) == callerClobberedForConv(cc, r) {
			t.Errorf("%v is both callee-saved and caller-clobbered under convention %d", r, cc)
		}
	}
}

// ---------------------------------------------------------------------------
// The ABIInternal rows, reached explicitly
// ---------------------------------------------------------------------------

// TestGoABIAllocationOrderDropsRuntimeAndScratchRegisters names the three
// registers ABIInternal removes from the System V order, rather than deriving
// them, so a table edit that drops the wrong one fails here instead of at the far
// end of B1. R14 holds g and is read by every managed-frame prologue; R12/R13 are
// the ABIInternal scratch pair.
func TestGoABIAllocationOrderDropsRuntimeAndScratchRegisters(t *testing.T) {
	order := intAllocOrderFor(ir.CallConvGoInternal)
	got := regsOf(t, "goIntAllocOrder", order)

	for _, banned := range []struct {
		r      Reg
		reason string
	}{
		{R14, "holds g under ABIInternal"},
		{R12, "is the ABIInternal scratch register 0"},
		{R13, "is the ABIInternal scratch register 1"},
		{RAX, "is reserved for fixed-register ops (div/rem, return value)"},
		{RCX, "is reserved for the variable shift count"},
		{RDX, "is reserved for the div/rem and widening-multiply high half"},
		{RSP, "anchors the stack"},
		{RBP, "anchors the frame"},
	} {
		if got[banned.r] {
			t.Errorf("ABIInternal allocation order includes %v, which %s", banned.r, banned.reason)
		}
	}

	// And it must not have quietly shrunk to nothing: the eight it does keep are
	// what makes ABIInternal codegen possible at all.
	want := []Reg{RBX, RSI, RDI, R8, R9, R10, R11, R15}
	if len(order) != len(want) {
		t.Fatalf("ABIInternal allocation order has %d registers, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("ABIInternal allocation order[%d] = %v, want %v", i, order[i], want[i])
		}
	}
}

// TestGoABIFloatAllocationOrderAvoidsZeroAndScratch is the float half of the same
// constraint, and the one with no B0 counterpart: X15 must hold zero throughout
// Go code and X13/X14 are ABIInternal's float scratch pair, so all three are
// unallocatable even though System V allocates X13.
func TestGoABIFloatAllocationOrderAvoidsZeroAndScratch(t *testing.T) {
	order := regsOf(t, "goFloatAllocOrder", floatAllocOrderFor(ir.CallConvGoInternal))

	if order[regFloatZero] {
		t.Errorf("ABIInternal float allocation order includes %v, which must stay zero in Go code", regFloatZero)
	}
	f0, f1 := scratchFPFor(ir.CallConvGoInternal)
	if order[f0] || order[f1] {
		t.Errorf("ABIInternal float allocation order includes a scratch register (%v, %v)", f0, f1)
	}
	for _, r := range order2slice(order) {
		if !r.IsFloat() {
			t.Errorf("ABIInternal float allocation order includes the GP register %v", r)
		}
	}

	// The System V order allocates X13; the difference between the two orders is
	// exactly the ABIInternal scratch pair, and nothing else.
	sysv := regsOf(t, "floatAllocOrder", floatAllocOrderFor(ir.CallConvPlatform))
	for r := range sysv {
		if !order[r] && r != f0 && r != f1 {
			t.Errorf("%v is allocatable under System V but not under ABIInternal, and is not its scratch pair", r)
		}
	}
	for r := range order {
		if !sysv[r] {
			t.Errorf("%v is allocatable under ABIInternal but not under System V; the ABIInternal set must be a subset", r)
		}
	}
}

// order2slice flattens a register set for iteration in tests.
func order2slice(set map[Reg]bool) []Reg {
	out := make([]Reg, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	return out
}

// TestGoABIPreservesNothing covers frame.go's and callersave.go's row at once:
// under ABIInternal no register is callee-saved, so the prologue saves nothing
// and every register-resident value live across a call must be wrapped by
// insertCallerSaves.
func TestGoABIPreservesNothing(t *testing.T) {
	for _, r := range allocatableRegs() {
		if calleeSavedFor(ir.CallConvGoInternal, r) {
			t.Errorf("%v is callee-saved under ABIInternal, which preserves nothing", r)
		}
		if !callerClobberedForConv(ir.CallConvGoInternal, r) {
			t.Errorf("%v survives a call under ABIInternal; every register is clobbered", r)
		}
	}

	// The colouring consequence: with no callee-saved registers there is nothing
	// for a call-crossing value to prefer, and gcalloc reads exactly this flag to
	// switch its preference off. Were it true, colouring would form a preference
	// no candidate could satisfy and spill every value live across a call.
	if conventionABI(ir.CallConvGoInternal).savesCalleeRegs {
		t.Error("ABIInternal claims to save callee registers; colouring would then prefer a set that is empty")
	}
	if intAllocOrderCalleeFirstFor(ir.CallConvGoInternal) == nil ||
		len(intAllocOrderCalleeFirstFor(ir.CallConvGoInternal)) != len(intAllocOrderFor(ir.CallConvGoInternal)) {
		t.Error("the ABIInternal callee-first order must be the plain order; there is nothing to put first")
	}
}

// TestGoABIScratchPairIsUsableByTheEmitter is the scratch-pair row for the
// emitter specifically. It restates the aliasing constraint at the level the
// emitter consumes it -- one bundle of four registers -- because that bundle, not
// the two accessor functions, is what mc and xsel carry.
func TestGoABIScratchPairIsUsableByTheEmitter(t *testing.T) {
	sc := scratchRegsFor(ir.CallConvGoInternal)

	goArgs := regsOf(t, "goArgGP", goArgGP)
	if goArgs[sc.gpScratch0] || goArgs[sc.gpScratch1] {
		t.Errorf("ABIInternal GP scratch pair (%v, %v) aliases an argument register; the emitter "+
			"would destroy a live argument while materializing a constant",
			sc.gpScratch0, sc.gpScratch1)
	}
	goFloatArgs := regsOf(t, "goArgFP", goArgFP)
	if goFloatArgs[sc.fpScratch0] || goFloatArgs[sc.fpScratch1] {
		t.Errorf("ABIInternal FP scratch pair (%v, %v) aliases a float argument register",
			sc.fpScratch0, sc.fpScratch1)
	}
	if sc.fpScratch0 == regFloatZero || sc.fpScratch1 == regFloatZero {
		t.Errorf("ABIInternal FP scratch pair (%v, %v) uses the zero register %v",
			sc.fpScratch0, sc.fpScratch1, regFloatZero)
	}
	if sc.gpScratch0 == regG || sc.gpScratch1 == regG {
		t.Errorf("ABIInternal GP scratch pair (%v, %v) uses %v, which holds g",
			sc.gpScratch0, sc.gpScratch1, regG)
	}

	// The four must be distinct: xasm_float.go's unsigned-conversion sequence holds
	// a value in each of the two float scratch registers simultaneously, and
	// memFor's fallback holds one GP value while computing an address in the other.
	if sc.gpScratch0 == sc.gpScratch1 {
		t.Errorf("the two GP scratch registers are the same register (%v)", sc.gpScratch0)
	}
	if sc.fpScratch0 == sc.fpScratch1 {
		t.Errorf("the two FP scratch registers are the same register (%v)", sc.fpScratch0)
	}

	// Scratch must be outside allocation, or the allocator would hand a value the
	// register the emitter overwrites underneath it.
	intOrder := regsOf(t, "goIntAllocOrder", intAllocOrderFor(ir.CallConvGoInternal))
	if intOrder[sc.gpScratch0] || intOrder[sc.gpScratch1] {
		t.Error("an ABIInternal GP scratch register is also allocatable")
	}
	fpOrder := regsOf(t, "goFloatAllocOrder", floatAllocOrderFor(ir.CallConvGoInternal))
	if fpOrder[sc.fpScratch0] || fpOrder[sc.fpScratch1] {
		t.Error("an ABIInternal FP scratch register is also allocatable")
	}
}

// ---------------------------------------------------------------------------
// The platform rows: nothing that compiles today may change
// ---------------------------------------------------------------------------

// TestPlatformConsumersUnchanged pins each of the four consumers' platform answer
// to the literal values they produced before the per-convention plumbing existed.
// Because emissionConvention returns the platform ABI for every function, this is
// the whole behavioural contract of this change: any difference here is a codegen
// difference.
func TestPlatformConsumersUnchanged(t *testing.T) {
	cc := ir.CallConvPlatform

	t.Run("gcalloc allocation orders", func(t *testing.T) {
		wantInt := []Reg{RSI, RDI, R8, R9, RBX, R12, R13, R14, R15}
		requireOrder(t, "intAllocOrderFor(platform)", intAllocOrderFor(cc), wantInt)

		wantFloat := []Reg{
			XMM(8), XMM(9), XMM(10), XMM(11), XMM(12), XMM(13),
			XMM(0), XMM(1), XMM(2), XMM(3), XMM(4), XMM(5), XMM(6), XMM(7),
		}
		requireOrder(t, "floatAllocOrderFor(platform)", floatAllocOrderFor(cc), wantFloat)

		wantCalleeFirst := []Reg{RBX, R12, R13, R14, R15, RSI, RDI, R8, R9}
		requireOrder(t, "intAllocOrderCalleeFirstFor(platform)", intAllocOrderCalleeFirstFor(cc), wantCalleeFirst)
		requireOrder(t, "floatAllocOrderCalleeFirstFor(platform)", floatAllocOrderCalleeFirstFor(cc), wantFloat)
	})

	t.Run("colorGraph pools", func(t *testing.T) {
		// The pools a real graph hands out, through the switch, for an integer and a
		// float temp -- the path gcalloc actually takes.
		f := &ir.Func{Temps: []*ir.Temp{{Cls: ir.ClsL}, {Cls: ir.ClsD}}}
		g := newColorGraph(f)
		requireOrder(t, "colorGraph.pool(int)", g.pool(0), intAllocOrder)
		requireOrder(t, "colorGraph.pool(float)", g.pool(1), floatAllocOrder)
		requireOrder(t, "colorGraph.poolCalleeFirst(int)", g.poolCalleeFirst(0), intAllocOrderCalleeFirst)
		requireOrder(t, "colorGraph.poolCalleeFirst(float)", g.poolCalleeFirst(1), floatAllocOrderCalleeFirst)
		if !conventionABI(g.cc).savesCalleeRegs {
			t.Error("the platform convention must save callee registers, or colouring stops preferring them")
		}
	})

	t.Run("frame callee-saved set", func(t *testing.T) {
		// System V: RBX and R12..R15 are preserved, everything else is not. RBP is
		// the frame pointer and handled by the prologue itself, so it is not in the
		// allocator-facing set.
		want := map[Reg]bool{RBX: true, R12: true, R13: true, R14: true, R15: true}
		for _, r := range allocatableRegs() {
			if got := calleeSavedFor(cc, r); got != want[r] {
				t.Errorf("calleeSavedFor(platform, %v) = %v, want %v", r, got, want[r])
			}
		}
	})

	t.Run("callersave clobber set", func(t *testing.T) {
		// The predicate insertCallerSaves reads must still be the exact complement
		// of the System V callee-saved set, including "every XMM is clobbered".
		for _, r := range allocatableRegs() {
			if got, want := callerClobberedForConv(cc, r), !calleeSavedReg(r); got != want {
				t.Errorf("callerClobberedForConv(platform, %v) = %v, want %v", r, got, want)
			}
		}
		for i := 0; i < 16; i++ {
			if !callerClobberedForConv(cc, XMM(i)) {
				t.Errorf("XMM%d survives a call under System V, which has no callee-saved XMM", i)
			}
		}
	})

	t.Run("emitter scratch pair", func(t *testing.T) {
		want := scratchRegs{
			gpScratch0: R10, gpScratch1: R11,
			fpScratch0: XMM(14), fpScratch1: XMM(15),
		}
		if got := scratchRegsFor(cc); got != want {
			t.Errorf("scratchRegsFor(platform) = %+v, want %+v", got, want)
		}
		// And the bundle the emitter really carries, for a function with no Go
		// annotation -- the whole C path, which must be byte-identical to what it
		// was before there was a choice of convention. (A goc-marked closure now
		// gets the ABIInternal pair instead; that is B1's flip, covered by
		// TestGoABIScratchPairIsUsableByTheEmitter.)
		f := &ir.Func{}
		m := &mc{f: f, scratchRegs: scratchRegsFor(emissionConvention(f))}
		if m.scratchRegs != want {
			t.Errorf("a platform-ABI function is emitted with scratch %+v, want the System V pair %+v",
				m.scratchRegs, want)
		}
	})
}

// TestPlatformScratchStaysOutOfAllocationAndArguments restates the two invariants
// the System V pair has always had, now that they are per-function values rather
// than constants: the allocator must never hand out a scratch register, and no
// System V argument may land in one.
func TestPlatformScratchStaysOutOfAllocationAndArguments(t *testing.T) {
	sc := scratchRegsFor(ir.CallConvPlatform)

	intOrder := regsOf(t, "intAllocOrder", intAllocOrderFor(ir.CallConvPlatform))
	if intOrder[sc.gpScratch0] || intOrder[sc.gpScratch1] {
		t.Errorf("System V GP scratch pair (%v, %v) is allocatable", sc.gpScratch0, sc.gpScratch1)
	}
	fpOrder := regsOf(t, "floatAllocOrder", floatAllocOrderFor(ir.CallConvPlatform))
	if fpOrder[sc.fpScratch0] || fpOrder[sc.fpScratch1] {
		t.Errorf("System V FP scratch pair (%v, %v) is allocatable", sc.fpScratch0, sc.fpScratch1)
	}

	args := regsOf(t, "argGP", argGP)
	if args[sc.gpScratch0] || args[sc.gpScratch1] {
		t.Errorf("System V GP scratch pair (%v, %v) aliases an argument register", sc.gpScratch0, sc.gpScratch1)
	}
	floatArgs := regsOf(t, "argFP", argFP)
	if floatArgs[sc.fpScratch0] || floatArgs[sc.fpScratch1] {
		t.Errorf("System V FP scratch pair (%v, %v) aliases a float argument register", sc.fpScratch0, sc.fpScratch1)
	}
}

// requireOrder compares an allocation order against an expected sequence, order
// included: the order is the allocator's preference, so a permutation is a
// codegen change even though the set is the same.
func requireOrder(t *testing.T, name string, got, want []Reg) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d registers, want %d (%v vs %v)", name, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %v, want %v", name, i, got[i], want[i])
		}
	}
}
