package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// This file holds the System V AMD64 calling convention itself: which register or
// stack slot each argument lands in, which register a result comes back in, and
// the pinned temporaries that make those choices explicit in the IR. It is
// deliberately separate from lower.go, which decides *what* to move across a call
// boundary; the rules here decide *where* those values live. Keeping the two
// apart means a second convention (Go's ABIInternal, with its own register
// sequences and stack packing) is a new table here rather than another branch
// threaded through the lowering.

// roundUp rounds n up to the next multiple of a.
func roundUp(n, a int) int {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}

// argLoc is where one argument is passed: a physical register or a stack slot at
// the given byte offset in the outgoing/incoming argument area.
type argLoc struct {
	reg     Reg
	onStack bool
	stacky  int
}

// argAssigner walks a sequence of argument classes and assigns each to a register
// or, once a bank is exhausted, to the stack. Integer and floating-point
// arguments consume independent register banks (rdi.. and xmm0..).
type argAssigner struct {
	ngrn, nsrn int
	nsaa       int // next stacked-argument byte offset
}

func (a *argAssigner) assign(cls ir.Cls) argLoc {
	if cls.IsFloat() {
		if a.nsrn < len(argFP) {
			r := argFP[a.nsrn]
			a.nsrn++
			return argLoc{reg: r}
		}
	} else if a.ngrn < len(argGP) {
		r := argGP[a.ngrn]
		a.ngrn++
		return argLoc{reg: r}
	}
	off := a.nsaa
	a.nsaa += 8 // scalars occupy one 8-byte stack slot
	return argLoc{onStack: true, stacky: off}
}

// stackBytes returns the 16-aligned size of the stacked-argument area.
func (a *argAssigner) stackBytes() int { return roundUp(a.nsaa, 16) }

// retReg returns the register a value of the given class is returned in.
func retReg(cls ir.Cls) Reg {
	if cls.IsFloat() {
		return XMM0
	}
	return RAX
}

// retIntRegs / retSSERegs are the aggregate-return register sequences.
var retIntRegs = []Reg{RAX, RDX}
var retSSERegs = []Reg{XMM0, XMM(1)}

// newPinned creates a fresh temporary hard-bound to physical register r.
func newPinned(f *ir.Func, r Reg, cls ir.Cls) ir.Ref {
	ref := f.NewTemp(fmt.Sprintf("R%d", int(r)), cls)
	t := f.Temp(ref)
	t.Fixed = true
	t.Reg = int(r)
	return ref
}
