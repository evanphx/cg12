package amd64

import (
	"testing"

	"github.com/evanphx/cg12/ir"
)

// The tests in this file pin the amd64 register decision (AMD64_PARITY_PLAN
// B0). They are invariant checks rather than codegen assertions: the tables
// they guard are consumed by lowering that does not exist yet, so the value
// here is that a later change which quietly breaks one of the constraints the
// decision rests on fails immediately rather than at the far end of B1.

func regsOf(t *testing.T, name string, regs []Reg) map[Reg]bool {
	t.Helper()
	set := make(map[Reg]bool, len(regs))
	for _, r := range regs {
		if set[r] {
			t.Fatalf("%s lists %v twice", name, r)
		}
		set[r] = true
	}
	return set
}

// TestGoABIArgumentRegistersMatchRuntime pins goArgGP to the sequence
// internal/abi documents for amd64. The runtime's abi.RegArgs is laid out in
// this order, so a divergence here is not a code-quality issue but a
// wrong-register read on any reflect-mediated call.
func TestGoABIArgumentRegistersMatchRuntime(t *testing.T) {
	// stdlib/src/internal/abi/abi_amd64.go: "RAX, RBX, RCX, RDI, RSI, R8, R9,
	// R10, R11", IntArgRegs = 9.
	want := []Reg{RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11}
	if len(goArgGP) != len(want) {
		t.Fatalf("goArgGP has %d registers, want %d", len(goArgGP), len(want))
	}
	for i := range want {
		if goArgGP[i] != want[i] {
			t.Errorf("goArgGP[%d] = %v, want %v", i, goArgGP[i], want[i])
		}
	}
}

// TestGoABIScratchDoesNotAliasArgumentRegisters is the load-bearing one. amd64
// has no scratch pair that is free under both conventions, which is why
// scratchGPFor exists at all; this asserts both halves of that claim, so
// collapsing it back to a single constant pair cannot pass silently.
func TestGoABIScratchDoesNotAliasArgumentRegisters(t *testing.T) {
	goArgs := regsOf(t, "goArgGP", goArgGP)

	s0, s1 := scratchGPFor(ir.CallConvGoInternal)
	if goArgs[s0] || goArgs[s1] {
		t.Errorf("ABIInternal scratch pair (%v, %v) aliases an argument register", s0, s1)
	}

	// The System V pair must alias, or the indirection would be pointless and a
	// future reader would be right to remove it.
	p0, p1 := scratchGPFor(ir.CallConvPlatform)
	if !goArgs[p0] && !goArgs[p1] {
		t.Errorf("System V scratch pair (%v, %v) no longer aliases an ABIInternal "+
			"argument register; scratchGPFor's per-convention split may now be unnecessary", p0, p1)
	}

	sysvArgs := regsOf(t, "argGP", argGP)
	if sysvArgs[p0] || sysvArgs[p1] {
		t.Errorf("System V scratch pair (%v, %v) aliases a System V argument register", p0, p1)
	}
}

// TestGoABIFloatScratchAvoidsZeroRegister covers the float side of the same
// constraint. X15 must stay zero in Go code and X0..X14 are all argument
// registers under a faithful table, so the two float scratch registers are
// carved out of the argument range -- goArgFP stops where scratch begins.
func TestGoABIFloatScratchAvoidsZeroRegister(t *testing.T) {
	f0, f1 := scratchFPFor(ir.CallConvGoInternal)
	if f0 == regFloatZero || f1 == regFloatZero {
		t.Errorf("ABIInternal float scratch (%v, %v) uses the zero register %v", f0, f1, regFloatZero)
	}
	for i, r := range goArgFP {
		if r == f0 || r == f1 || r == regFloatZero {
			t.Errorf("goArgFP[%d] = %v overlaps scratch or the zero register", i, r)
		}
	}
	// The cap is a deliberate deviation from Go's FloatArgRegs = 15, so state
	// the number here: a change to it is a change to the ABI contract.
	if len(goArgFP) != 13 {
		t.Errorf("goArgFP carries %d registers, want 13 (X0..X12); "+
			"changing this changes where cg12 and Go's own compiler disagree", len(goArgFP))
	}
	if !goFloatArgRegSpill(len(goArgFP) + 1) {
		t.Error("goFloatArgRegSpill does not report a signature that exceeds the capped table")
	}
	if goFloatArgRegSpill(len(goArgFP)) {
		t.Error("goFloatArgRegSpill reports a signature that fits the capped table")
	}
}

// TestRuntimeReservedRegistersLeaveAllocation checks that the registers the Go
// runtime owns are not handed out under ABIInternal. g in particular is read by
// every managed-frame prologue; allocating it would corrupt the goroutine
// pointer in a way that surfaces far from the cause.
func TestRuntimeReservedRegistersLeaveAllocation(t *testing.T) {
	order := regsOf(t, "goIntAllocOrder", intAllocOrderFor(ir.CallConvGoInternal))
	if order[regG] {
		t.Errorf("ABIInternal allocation order includes %v, reserved for g", regG)
	}
	s0, s1 := scratchGPFor(ir.CallConvGoInternal)
	if order[s0] || order[s1] {
		t.Errorf("ABIInternal allocation order includes a scratch register (%v, %v)", s0, s1)
	}
	for _, r := range reservedForFixedOps {
		if order[r] {
			t.Errorf("ABIInternal allocation order includes %v, reserved for fixed-register ops", r)
		}
	}
	if order[RSP] || order[RBP] {
		t.Error("ABIInternal allocation order includes a frame register")
	}

	// The System V order must keep its own invariants: it may use R14, since g
	// only exists under a Go frame, but never the platform scratch pair.
	sysv := regsOf(t, "intAllocOrder", intAllocOrderFor(ir.CallConvPlatform))
	p0, p1 := scratchGPFor(ir.CallConvPlatform)
	if sysv[p0] || sysv[p1] {
		t.Errorf("System V allocation order includes a scratch register (%v, %v)", p0, p1)
	}
}

// TestCalleeSavedByConvention records that ABIInternal preserves nothing, so a
// value live across an ABIInternal call is always spilled.
func TestCalleeSavedByConvention(t *testing.T) {
	for _, r := range []Reg{RBX, R12, R13, R14, R15} {
		if !calleeSavedFor(ir.CallConvPlatform, r) {
			t.Errorf("%v is not callee-saved under System V", r)
		}
		if calleeSavedFor(ir.CallConvGoInternal, r) {
			t.Errorf("%v is callee-saved under ABIInternal, which preserves nothing", r)
		}
	}
}

// TestReservesRuntimeRegistersKeysOffFrameAndConvention pins the either/or in
// reservesRuntimeRegs. Keying it off the convention alone would miss the
// runtime's own managed-frame platform-ABI helpers, whose prologues still read
// g out of R14.
func TestReservesRuntimeRegistersKeysOffFrameAndConvention(t *testing.T) {
	plain := &ir.Func{}
	if reservesRuntimeRegs(plain) {
		t.Error("a platform-ABI, unmanaged function reserves the runtime registers")
	}

	managed := &ir.Func{ManagedFrame: true}
	if !reservesRuntimeRegs(managed) {
		t.Error("a managed-frame function does not reserve the runtime registers")
	}

	goABI := &ir.Func{CallConv: ir.CallConvGoInternal}
	if !reservesRuntimeRegs(goABI) {
		t.Error("an ABIInternal function does not reserve the runtime registers")
	}
}

// TestPlatformConventionUnchanged guards the whole decision against regressing
// the working C path: System V's tables are what they were before B0, so
// nothing that compiles today changes.
func TestPlatformConventionUnchanged(t *testing.T) {
	c := conventionABI(ir.CallConvPlatform)
	wantInt := []Reg{RDI, RSI, RDX, RCX, R8, R9}
	if len(c.intArgRegs) != len(wantInt) {
		t.Fatalf("System V integer argument registers = %d, want %d", len(c.intArgRegs), len(wantInt))
	}
	for i := range wantInt {
		if c.intArgRegs[i] != wantInt[i] {
			t.Errorf("System V intArgRegs[%d] = %v, want %v", i, c.intArgRegs[i], wantInt[i])
		}
	}
	if len(c.floatArgRegs) != 8 {
		t.Errorf("System V float argument registers = %d, want 8", len(c.floatArgRegs))
	}
	if c.packsStackArgs {
		t.Error("System V packs stacked arguments; it must give each scalar an 8-byte slot")
	}
	if !c.savesCalleeRegs {
		t.Error("System V does not save callee registers")
	}
}

// TestStackLinkBytesZeroOnAMD64 records why arm64's goStackLinkSize has no
// amd64 counterpart: the call instruction pushes the return address, so the
// frame-chain link ABIInternal reserves by hand on arm64 already exists here.
// B1 and B4 both consume this, and a nonzero value would double-count it.
func TestStackLinkBytesZeroOnAMD64(t *testing.T) {
	for _, cc := range []ir.CallConvention{ir.CallConvPlatform, ir.CallConvGoInternal} {
		if got := conventionABI(cc).stackLinkBytes; got != 0 {
			t.Errorf("stackLinkBytes for convention %d = %d, want 0", cc, got)
		}
	}
}

// TestCalleeConventionsResolvesFromTheCallee covers the rule B1 consumes to
// decide how each call is lowered, and in particular the case arm64's
// enclosing-function fallback gets wrong: an unmarked direct call to a
// platform-ABI function from inside an ABIInternal function.
func TestCalleeConventionsResolvesFromTheCallee(t *testing.T) {
	mod := &ir.Module{}

	plain := mod.NewFuncVoid("main.counter.add")

	wrapper := mod.NewFuncVoid("main.methodvalue.counter.add")
	wrapper.CallConv = ir.CallConvGoInternal

	closure := mod.NewFuncVoid("main.func.1")
	closure.CallConv = ir.CallConvGoInternal

	conv := newCalleeConventions(mod)

	t.Run("unmarked call to a platform callee from an ABIInternal function", func(t *testing.T) {
		entry := wrapper.Entry()
		entry.CallVoid(wrapper.Sym(plain.Name, 0))
		call := &entry.Instrs[len(entry.Instrs)-1]

		if got := conv.forCall(wrapper, call); got != ir.CallConvPlatform {
			t.Errorf("convention = %d, want %d (the callee's); inheriting the enclosing "+
				"function's convention here is the arm64 rule that miscompiles on amd64",
				got, ir.CallConvPlatform)
		}
	})

	t.Run("explicit convention on the call wins", func(t *testing.T) {
		entry := plain.Entry()
		entry.CallVoidWithConvention(ir.CallConvGoInternal, plain.Sym("indirect", 0))
		call := &entry.Instrs[len(entry.Instrs)-1]

		if got := conv.forCall(plain, call); got != ir.CallConvGoInternal {
			t.Errorf("convention = %d, want %d (the call's own)", got, ir.CallConvGoInternal)
		}
	})

	t.Run("unmarked call to an ABIInternal callee", func(t *testing.T) {
		entry := plain.Entry()
		entry.CallVoid(plain.Sym(closure.Name, 0))
		call := &entry.Instrs[len(entry.Instrs)-1]

		if got := conv.forCall(plain, call); got != ir.CallConvGoInternal {
			t.Errorf("convention = %d, want %d (the callee's)", got, ir.CallConvGoInternal)
		}
	})

	t.Run("call to a symbol this module does not define", func(t *testing.T) {
		entry := wrapper.Entry()
		entry.CallVoid(wrapper.Sym("memcpy", 0))
		call := &entry.Instrs[len(entry.Instrs)-1]

		if got := conv.forCall(wrapper, call); got != ir.CallConvPlatform {
			t.Errorf("convention = %d, want %d; an external symbol is platform ABI by "+
				"definition, since ABIInternal only exists inside a module cg12 compiled",
				got, ir.CallConvPlatform)
		}
	})
}

// TestArgAssignerPacksOnlyUnderGoABI checks the stack-packing axis of the
// descriptor, which is the part of the convention that shows up in stack
// offsets rather than in register numbers.
func TestArgAssignerPacksOnlyUnderGoABI(t *testing.T) {
	// Exhaust the integer registers, then place two 4-byte values.
	spill := func(goABI bool) []int {
		a := newArgAssigner(goABI)
		n := len(conventionABI(goInternalConvention(goABI)).intArgRegs)
		for i := 0; i < n; i++ {
			a.assign(ir.ClsL)
		}
		return []int{a.assign(ir.ClsW).stacky, a.assign(ir.ClsW).stacky}
	}

	if got := spill(false); got[0] != 0 || got[1] != 8 {
		t.Errorf("System V stacked 4-byte arguments at %v, want [0 8] (one 8-byte slot each)", got)
	}
	if got := spill(true); got[0] != 0 || got[1] != 4 {
		t.Errorf("ABIInternal stacked 4-byte arguments at %v, want [0 4] (packed)", got)
	}
}
