// Package amd64 is the x86-64 (System V AMD64) backend: it lowers cg12 SSA IR to
// x86-64 machine code and writes a relocatable ELF object, using the amd64/x64
// instruction encoder. It mirrors the arm64 backend's pipeline — pointer
// lowering, SSA destruction plus ABI lowering, linear-scan register allocation,
// then a direct machine-code emitter — adapted to x86-64's registers, variable-
// length encoding, and calling convention.
package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// Reg identifies a physical register. General-purpose registers use their native
// 0..15 encodings; XMM registers follow, numbered XMM0..XMM15 = 16..31, so a
// single int distinguishes the two banks.
type Reg int

const (
	RAX Reg = 0
	RCX Reg = 1
	RDX Reg = 2
	RBX Reg = 3
	RSP Reg = 4
	RBP Reg = 5
	RSI Reg = 6
	RDI Reg = 7
	R8  Reg = 8
	R9  Reg = 9
	R10 Reg = 10
	R11 Reg = 11
	R12 Reg = 12
	R13 Reg = 13
	R14 Reg = 14
	R15 Reg = 15

	XMM0 Reg = 16
)

// XMM returns the register for XMM index n (0..15).
func XMM(n int) Reg { return XMM0 + Reg(n) }

// IsFloat reports whether r is an XMM register.
func (r Reg) IsFloat() bool { return r >= XMM0 }

// mreg maps a backend register to the raw x64 encoder register (0..15 in either
// bank).
func (r Reg) mreg() x64.Reg {
	if r.IsFloat() {
		return x64.Reg(r - XMM0)
	}
	return x64.Reg(r)
}

// Scratch registers, reserved from allocation. The emitter uses them to
// materialize constants, stage spill reloads, and break move cycles.
//
// These are the System V pair. Go's ABIInternal cannot use them -- R10 and R11
// are its argument registers 8 and 9 -- and needs a different pair; see
// scratchGPFor and the "no convention-independent scratch pair" note below.
const (
	gpScratch0 = R10
	gpScratch1 = R11
	fpScratch0 = XMM0 + 14
	fpScratch1 = XMM0 + 15
)

// ---------------------------------------------------------------------------
// Go ABIInternal register assignment
//
// The registers below are not a design choice: Go's amd64 ABIInternal fixes
// them, and the runtime this backend has to interoperate with reads them
// directly. Each is cited to the vendored source that pins it, so a future
// reader can re-derive the constraint rather than trust the comment.
// ---------------------------------------------------------------------------

// regG holds the current goroutine under ABIInternal. The runtime installs it by
// name -- `MOVQ DX, R14 // set the g register`
// (stdlib/src/runtime/asm_amd64.s:444) -- and every managed-frame prologue reads
// g.stackguard through it, so this is not substitutable.
const regG = R14

// regClosure carries the closure environment pointer on an ABIInternal closure
// call: "set RDX to point to the closure (if a closure call)"
// (stdlib/src/runtime/asm_amd64.s:2003).
//
// RDX is triply committed on amd64 -- closure register here, the high half of
// div/rem's RDX:RAX and of the widening multiply, and mc_va.go's vararg scratch.
// The conflict is resolved by stabilization rather than by reservation: goc pins
// the incoming context to a Fixed temp (goc/compile.go closureContext) and the
// backend copies it to an allocatable register at function entry before anything
// else runs, which is the shape arm64 already uses in stabilizeClosureContext.
// After that copy RDX is free for its other two roles.
const regClosure = RDX

// regFloatZero must contain zero throughout Go code -- "Clear X15 because Go
// expects it" (stdlib/src/runtime/asm_amd64.s:1089-1095). It is therefore
// neither allocatable nor available as scratch under ABIInternal.
var regFloatZero = XMM(15)

// goArgGP / goArgFP are the ABIInternal argument registers.
//
// goArgGP is exactly internal/abi's list: "RAX, RBX, RCX, RDI, RSI, R8, R9, R10,
// R11" with IntArgRegs = 9 (stdlib/src/internal/abi/abi_amd64.go:10-11).
//
// goArgFP deliberately stops at X12 where Go's own FloatArgRegs = 15 runs
// X0..X14. cg12's emitter needs two float scratch registers that no argument can
// occupy (they are used together -- see xasm_float.go's unsigned-conversion
// bias sequence, which holds a value in each), X15 is spoken for as the zero
// register, and X0..X14 are all argument registers, so under a faithful table
// there is no float register left to improvise with. Capping the table at 13 is
// the deviation, chosen over stealing an argument register mid-assignment
// because it is visible in one place instead of being an aliasing hazard spread
// across every float sequence. A call needing a 14th float register argument
// must be refused by name rather than silently passed on the stack; see
// goFloatArgRegSpill.
var goArgGP = []Reg{RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11}
var goArgFP = []Reg{
	XMM(0), XMM(1), XMM(2), XMM(3), XMM(4), XMM(5), XMM(6),
	XMM(7), XMM(8), XMM(9), XMM(10), XMM(11), XMM(12),
}

// goFloatArgRegSpill reports whether an ABIInternal signature assigns more float
// register arguments than cg12's capped table can carry, i.e. whether cg12 and
// Go's own compiler would disagree about where argument n lives. Lowering must
// fail on this rather than pass the argument on the stack: both sides of a
// cg12-compiled call would agree with each other, but the runtime's
// abi.RegArgs -- sized by FloatArgRegs = 15 -- would not, so a reflect-mediated
// call would read the argument from a register cg12 never wrote.
func goFloatArgRegSpill(floatRegArgs int) bool { return floatRegArgs > len(goArgFP) }

// scratchGPFor returns the emitter's scratch pair for a calling convention.
//
// There is no pair that is free under both conventions, which is the one place
// amd64 has no arm64 precedent to copy. System V wants caller-saved non-argument
// registers, and its only ones are RAX, R10 and R11. ABIInternal passes
// arguments in R10 and R11 and has no caller-saved/callee-saved distinction at
// all; subtracting its nine argument registers, RDX, R14, RSP and RBP leaves
// exactly R12 and R13 -- which is why Go's own compiler keeps that pair free.
//
// Making the System V pair callee-saved instead (to share one pair across both)
// was rejected: R12/R13 are callee-saved under System V, so a cg12 function that
// touched them transiently would have to save and restore them in every
// prologue, paying two pushes and two pops on every function to avoid clobbering
// a C caller's live value.
//
// NOTE: the emitter still spells gpScratch0/gpScratch1 directly at ~150 sites
// and so implements only the System V row. That is correct for everything that
// compiles today, because nothing yet asks for ABIInternal register assignment:
// lowerParams and lowerCalls build platform assigners unconditionally, so a
// function marked CallConvGoInternal is currently emitted as self-consistent
// System V code. Threading the pair through the emitter is B2's work, and
// TestGoABIScratchDoesNotAliasArgumentRegisters pins the reason it cannot be
// skipped: the System V pair *is* ABIInternal's argument registers 8 and 9, so
// wiring goArgGP in without also moving scratch would let the emitter clobber a
// live argument while materializing a constant.
func scratchGPFor(cc ir.CallConvention) (Reg, Reg) {
	if cc == ir.CallConvGoInternal {
		return R12, R13
	}
	return gpScratch0, gpScratch1
}

// scratchFPFor returns the float scratch pair. Under System V, XMM14/XMM15 are
// free because System V passes at most eight float arguments. Under ABIInternal,
// X15 is the zero register, so the pair moves down to X13/X14 -- which is what
// caps goArgFP at X12.
func scratchFPFor(cc ir.CallConvention) (Reg, Reg) {
	if cc == ir.CallConvGoInternal {
		return XMM(13), XMM(14)
	}
	return fpScratch0, fpScratch1
}

// intAllocOrderFor returns the general-purpose allocation order for a calling
// convention.
//
// ABIInternal drops three registers from the System V order: R14 is g, and
// R12/R13 are its scratch pair. It does not gain RAX/RCX/RDX back even though
// ABIInternal passes arguments in them -- see reservedForFixedOps. R15 stays
// allocatable: Go reserves it for GOT-relative addressing only under -dynlink,
// which cg12 does not emit.
func intAllocOrderFor(cc ir.CallConvention) []Reg {
	if cc == ir.CallConvGoInternal {
		return goIntAllocOrder
	}
	return intAllocOrder
}

// goIntAllocOrder is the ABIInternal allocation order. ABIInternal has no
// callee-saved registers, so unlike the System V order there is no caller-first
// / callee-first split to make: every allocatable register is clobbered by a
// call, and a value live across one is spilled either way.
var goIntAllocOrder = []Reg{RBX, RSI, RDI, R8, R9, R10, R11, R15}

// reservedForFixedOps names the registers held out of allocation under every
// convention because instruction encodings, not the ABI, demand them: the
// division pair RDX:RAX and the widening multiply's high half, and CL as the
// variable shift count. xselect_bits.go and xselect_atomic.go both state their
// correctness in terms of this reservation, so it is load-bearing rather than
// merely conservative.
//
// Under ABIInternal these are also argument registers 1, 2 and 3. That is not a
// contradiction: an argument arrives in a Fixed temp pinned to the register,
// which is independent of whether the allocator may hand the register out for
// ordinary values. It costs a copy out of RAX/RBX/RCX at entry, the same copy
// arm64 already pays for its closure register.
var reservedForFixedOps = []Reg{RAX, RCX, RDX}

// calleeSavedFor reports whether a callee must preserve r under a convention.
// ABIInternal preserves nothing -- every register is caller-saved -- so a value
// live across an ABIInternal call is always spilled.
func calleeSavedFor(cc ir.CallConvention, r Reg) bool {
	if cc == ir.CallConvGoInternal {
		return false
	}
	return calleeSavedReg(r)
}

// reservesRuntimeRegs reports whether a function reserves the registers the Go
// runtime owns -- g in R14 and zero in X15.
//
// It keys off the frame rather than the convention. A managed frame reads
// g.stackguard in its prologue whatever its argument convention is (goc emits
// managed-frame platform-ABI functions for the runtime's own C-shaped helpers),
// and an ABIInternal function may be called from Go code that expects X15 intact
// even if it never grows its own stack. Either condition alone is enough.
func reservesRuntimeRegs(f *ir.Func) bool {
	return f.UsesManagedFrame() || f.UsesGoInternalCallConvention()
}

// intAllocOrder is the general-purpose allocation order: caller-saved first (no
// prologue save needed), then callee-saved. RAX/RCX/RDX are reserved for the
// fixed-register instructions (return value, div/rem in RDX:RAX, shift count in
// CL); RSP/RBP anchor the frame; R10/R11 are scratch.
var intAllocOrder = []Reg{
	RSI, RDI, R8, R9, // caller-saved, argument registers
	RBX, R12, R13, R14, R15, // callee-saved
}

// floatAllocOrder is the XMM allocation order. System V has no callee-saved XMM
// registers, so every float value live across a call is spilled (pickRegister
// enforces this via calleeSavedReg). XMM14/XMM15 are scratch.
var floatAllocOrder = []Reg{
	XMM(8), XMM(9), XMM(10), XMM(11), XMM(12), XMM(13),
	XMM(0), XMM(1), XMM(2), XMM(3), XMM(4), XMM(5), XMM(6), XMM(7),
}

// calleeSaved holds the registers a callee must preserve. In System V these are
// RBX, RBP, R12..R15; RBP is the frame pointer and handled separately, so it is
// not listed as allocatable-and-callee-saved here.
var calleeSaved = map[Reg]bool{
	RBX: true, R12: true, R13: true, R14: true, R15: true,
}

// calleeSavedReg reports whether r must be preserved across a call.
func calleeSavedReg(r Reg) bool { return calleeSaved[r] }

// callerClobbered reports whether a call clobbers register r: everything but the
// callee-saved GP registers, which includes every XMM (System V has no
// callee-saved XMM), so a float held across a call is always caller-clobbered.
func callerClobbered(r Reg) bool { return !calleeSavedReg(r) }

// intAllocOrderCalleeFirst / floatAllocOrderCalleeFirst try the callee-saved
// registers first. A value that lives across a call prefers these -- one prologue
// save/restore rather than a save/restore around every crossed call. (System V has
// no callee-saved XMM, so the float order is unchanged; a call-crossing float that
// takes a caller-saved XMM is wrapped by insertCallerSaves.)
var intAllocOrderCalleeFirst = []Reg{
	RBX, R12, R13, R14, R15, // callee-saved
	RSI, RDI, R8, R9, // caller-saved
}
var floatAllocOrderCalleeFirst = floatAllocOrder

// argGP / argFP are the System V argument registers in order.
var argGP = []Reg{RDI, RSI, RDX, RCX, R8, R9}
var argFP = []Reg{XMM(0), XMM(1), XMM(2), XMM(3), XMM(4), XMM(5), XMM(6), XMM(7)}
