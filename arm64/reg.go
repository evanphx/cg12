// Package arm64 is cg12's AArch64 (AAPCS64) machine-code backend. It lowers the
// SSA IR to assembly through SSA destruction + ABI lowering, linear-scan
// register allocation, and GNU-assembler text emission.
package arm64

import "fmt"

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

	// Three fixed scratch registers, never handed out by the allocator: two for
	// spilled/immediate operands and one extra for rem's quotient and for
	// breaking parallel-move cycles.
	scratch0 = X16
	scratch1 = X17
	scratch2 = X15
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

// intAllocOrder is the integer allocation order: caller-saved temporaries first
// (cheaper, no save/restore), then callee-saved. X16/X17 (scratch), X18
// (platform), X29/X30/SP/ZR are intentionally excluded.
var intAllocOrder = []Reg{
	// caller-saved (clobbered by calls); X15/X16/X17 are reserved as scratch
	X9, X10, X11, X12, X13, X14,
	X0, X1, X2, X3, X4, X5, X6, X7, X8,
	// callee-saved (preserved across calls)
	// X26 and X27 are reserved for the Go runtime's system-stack transition.
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

// intScratch and fscratch map a slot to a reserved scratch register. Slot 2
// (x15) is used only by 3-operand indexed stores.
var intScratchRegs = [3]Reg{scratch0, scratch1, scratch2}

var floatScratchRegs = [2]Reg{fscratch0, fscratch1}

// srcReg returns the assembler name of a register holding ref's value at the
// given width, loading from a spill slot or materialising a constant into a
// class-appropriate scratch register when necessary. slot (0 or 1) selects which
// scratch register to borrow.
