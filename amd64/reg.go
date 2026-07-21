// Package amd64 is the x86-64 (System V AMD64) backend: it lowers cg12 SSA IR to
// x86-64 machine code and writes a relocatable ELF object, using the amd64/x64
// instruction encoder. It mirrors the arm64 backend's pipeline — pointer
// lowering, SSA destruction plus ABI lowering, linear-scan register allocation,
// then a direct machine-code emitter — adapted to x86-64's registers, variable-
// length encoding, and calling convention.
package amd64

import "github.com/evanphx/cg12/amd64/x64"

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
const (
	gpScratch0 = R10
	gpScratch1 = R11
	fpScratch0 = XMM0 + 14
	fpScratch1 = XMM0 + 15
)

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
