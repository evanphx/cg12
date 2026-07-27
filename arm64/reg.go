// Package arm64 is cg12's AArch64 (AAPCS64) machine-code backend. It lowers the
// SSA IR to assembly through SSA destruction + ABI lowering, linear-scan
// register allocation, and GNU-assembler text emission.
package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// Reg is a physical AArch64 register identifier. Integer registers X0..X30 use
// ids 0..30; SP and the zero register follow; the SIMD/FP registers V0..V31
// occupy a separate id range so a register's class is recoverable from its id.
type Reg int

const (
	X0 Reg = iota
	X1
	X2
	X3
	X4
	X5
	X6
	X7
	X8
	X9
	X10
	X11
	X12
	X13
	X14
	X15
	X16
	X17
	X18
	X19
	X20
	X21
	X22
	X23
	X24
	X25
	X26
	X27
	X28
	X29 // frame pointer
	X30 // link register
	SP
	ZR // zero register (XZR/WZR)

	V0
	// V1..V31 follow contiguously.
	vLast = V0 + 31

	FP = X29
	LR = X30

	// Five fixed scratch registers, never handed out by the allocator. Ordinary
	// instructions use the first two for spilled/immediate operands and the third
	// for remainder and parallel-move cycles. Atomic retry loops may also use one
	// caller-saved register and the architecture's dedicated temporary register.
	scratch0 = X16
	scratch1 = X17
	scratch2 = X15
	scratch3 = X27
	scratch4 = X14
)

// IsFloat reports whether r is a SIMD/FP register.
func (r Reg) IsFloat() bool { return r >= V0 && r <= vLast }

// xName returns the 64-bit register name.
func (r Reg) xName() string {
	switch {
	case r >= X0 && r <= X30:
		return fmt.Sprintf("x%d", int(r-X0))
	case r == SP:
		return "sp"
	case r == ZR:
		return "xzr"
	case r >= V0 && r <= vLast:
		return fmt.Sprintf("d%d", int(r-V0))
	}
	return "<badreg>"
}

// wName returns the 32-bit register name.
func (r Reg) wName() string {
	switch {
	case r >= X0 && r <= X30:
		return fmt.Sprintf("w%d", int(r-X0))
	case r == SP:
		return "wsp"
	case r == ZR:
		return "wzr"
	case r >= V0 && r <= vLast:
		return fmt.Sprintf("s%d", int(r-V0))
	}
	return "<badreg>"
}

// qName returns the 128-bit SIMD register name.
func (r Reg) qName() string {
	if r >= V0 && r <= vLast {
		return fmt.Sprintf("q%d", int(r-V0))
	}
	return "<badreg>"
}

// vec16Name returns the register's .16B vector-arrangement name, used by the
// full-width SIMD register move (mov Vd.16B, Vn.16B).
func (r Reg) vec16Name() string {
	if r >= V0 && r <= vLast {
		return fmt.Sprintf("v%d.16b", int(r-V0))
	}
	return "<badreg>"
}

// Name returns the assembler name of r viewed at the given width (4, 8, or 16
// bytes; 16 names a 128-bit Q register).
func (r Reg) Name(size int) string {
	switch size {
	case 4:
		return r.wName()
	case 16:
		return r.qName()
	default:
		return r.xName()
	}
}

// intAllocOrder is the base integer allocation order: caller-saved temporaries
// first (cheaper, no save/restore), then callee-saved. X14-X17 and X27 (scratch),
// X18 (platform), X26 (Go closure context), X28 (the Go g register), and
// X29/X30/SP/ZR are excluded. A platform-C-ABI function reserves neither X26 nor
// X28 and gets them back via [intAllocOrderFor].
var intAllocOrder = []Reg{
	// caller-saved (clobbered by calls); X14-X17 and X27 are reserved as scratch
	X9, X10, X11, X12, X13,
	X0, X1, X2, X3, X4, X5, X6, X7, X8,
	// callee-saved (preserved across calls)
	X19, X20, X21, X22, X23, X24, X25,
}

// callerSaved marks the integer registers a call clobbers.
var callerSaved = func() map[Reg]bool {
	m := map[Reg]bool{}
	for r := X0; r <= X18; r++ {
		m[r] = true
	}
	return m
}()

// calleeSaved marks the integer registers a callee must preserve.
var calleeSaved = func() map[Reg]bool {
	m := map[Reg]bool{}
	for r := X19; r <= X28; r++ {
		m[r] = true
	}
	return m
}()

// isCallerSaved reports whether a call clobbers r.
func isCallerSaved(r Reg) bool { return callerSaved[r] }

// vReg returns SIMD/FP register Vn.
func vReg(n int) Reg { return V0 + Reg(n) }

// Float scratch registers, reserved for spilled/immediate float operands.
var (
	fscratch0 = vReg(30)
	fscratch1 = vReg(31)
)

// floatAllocOrder is the SIMD/FP allocation order: caller-saved temporaries
// first, then argument registers, then callee-saved. V30/V31 are reserved as
// scratch.
var floatAllocOrder = func() []Reg {
	var order []Reg
	for n := 16; n <= 29; n++ { // caller-saved temporaries (V30/V31 are scratch)
		order = append(order, vReg(n))
	}
	for n := 0; n <= 7; n++ { // argument/return registers (caller-saved)
		order = append(order, vReg(n))
	}
	for n := 8; n <= 15; n++ { // callee-saved
		order = append(order, vReg(n))
	}
	return order
}()

// calleeSavedFloat marks the SIMD/FP registers a callee must preserve (the low
// 64 bits of V8..V15 under AAPCS64).
var calleeSavedFloat = func() map[Reg]bool {
	m := map[Reg]bool{}
	for n := 8; n <= 15; n++ {
		m[vReg(n)] = true
	}
	return m
}()

// calleeSavedReg reports whether r (integer or float) must be preserved across a
// call.
func calleeSavedReg(r Reg) bool { return calleeSaved[r] || calleeSavedFloat[r] }

// intAllocOrderCalleeFirst / floatAllocOrderCalleeFirst try callee-saved registers
// before caller-saved. A value live across a call whose crossings are hot prefers a
// callee-saved register (saved once in the prologue); only when those run out does it
// fall back to a caller-saved one (which the caller-save pass then saves and restores
// around each crossed call). Non-crossing values keep the caller-first orders above.
var (
	intAllocOrderCalleeFirst   = calleeFirstOrder(intAllocOrder)
	floatAllocOrderCalleeFirst = calleeFirstOrder(floatAllocOrder)
)

// intAllocOrder excludes X26 and X28 because the Go runtime reserves them (the
// closure context and the g register). A function using the platform C ABI --
// neither Go's internal convention nor a managed frame -- reserves neither, so it
// may allocate both as ordinary callee-saved registers; they sit last so caller-
// saved registers are still tried first. The frame builder walks the same
// per-function order, so a used one is saved and restored in the prologue.
var (
	intAllocOrderPlatform            = append(append([]Reg(nil), intAllocOrder...), X26, X28)
	intAllocOrderPlatformCalleeFirst = calleeFirstOrder(intAllocOrderPlatform)
)

// reservesRuntimeRegs reports whether f's convention reserves X26/X28 for the Go
// runtime; only such functions use the narrower base allocation order.
func reservesRuntimeRegs(f *ir.Func) bool {
	return f.UsesGoInternalCallConvention() || f.UsesManagedFrame()
}

// intAllocOrderFor / intAllocOrderCalleeFirstFor return f's integer allocation
// order, widened with X26/X28 for platform-ABI functions.
func intAllocOrderFor(f *ir.Func) []Reg {
	if reservesRuntimeRegs(f) {
		return intAllocOrder
	}
	return intAllocOrderPlatform
}

func intAllocOrderCalleeFirstFor(f *ir.Func) []Reg {
	if reservesRuntimeRegs(f) {
		return intAllocOrderCalleeFirst
	}
	return intAllocOrderPlatformCalleeFirst
}

func calleeFirstOrder(order []Reg) []Reg {
	var callee, caller []Reg
	for _, r := range order {
		if calleeSavedReg(r) {
			callee = append(callee, r)
		} else {
			caller = append(caller, r)
		}
	}
	return append(callee, caller...)
}

// calleeSavedFor reports whether a register survives calls made by f. Go's
// ABIInternal has no callee-saved general-purpose or floating-point registers;
// the platform ABI retains the AAPCS64 preservation rules.
func calleeSavedFor(goABI bool, r Reg) bool {
	if !conventionABI(goInternalConvention(goABI)).savesCalleeRegs {
		return false
	}
	return calleeSavedReg(r)
}

// registerSurvivesManagedCalls reports whether a value may remain in register
// across an arbitrary call. Managed functions can enter the raw stack-growth
// path, which follows Go's volatile-register contract even when the function's
// ordinary calls use AAPCS64.
func registerSurvivesManagedCalls(function *ir.Func, register Reg) bool {
	if function.UsesManagedFrame() {
		return false
	}
	return calleeSavedFor(function.UsesGoInternalCallConvention(), register)
}

// intScratch and fscratch map a slot to a reserved scratch register. General
// operand materialization uses slots 0-2; atomic retry loops may borrow all five.
var intScratchRegs = [5]Reg{scratch0, scratch1, scratch2, scratch3, scratch4}

var floatScratchRegs = [2]Reg{fscratch0, fscratch1}

// srcReg returns the assembler name of a register holding ref's value at the
// given width, loading from a spill slot or materialising a constant into a
// class-appropriate scratch register when necessary. slot (0 or 1) selects which
// scratch register to borrow.
