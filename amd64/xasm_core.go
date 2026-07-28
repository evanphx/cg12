package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmCore is the part of the builder every other family is written in terms of:
// the location model that says where a value currently lives, the moves between
// locations, the spill stores that commit a scratch register to a result's slot,
// and the failure path for an operation the backend cannot express.
type xasmCore interface {
	// refLoc maps an operand to its location (register, spill slot, immediate, or
	// symbol); move transfers between two locations.
	refLoc(ref ir.Ref) loc
	move(dst, src loc)
	movReg(w bool, dst, src Reg)

	// spillStore/spillStoreFP write a scratch register back to a result's slot.
	spillStore(r Reg, slot, size int)
	spillStoreFP(r Reg, slot, size int)

	fail(format string, a ...any)
}

func (b *mcXasm) refLoc(ref ir.Ref) loc       { return b.m.refLoc(ref) }
func (b *mcXasm) move(dst, src loc)           { b.m.move(dst, src) }
func (b *mcXasm) movReg(w bool, dst, src Reg) { b.m.emit(x64.MovReg(w, dst.mreg(), src.mreg())) }

func (b *mcXasm) spillStore(r Reg, slot, size int) {
	b.m.emit(x64.Store(size*8, r.mreg(), x64.At(RBP.mreg(), b.m.slotAddr(slot))))
}
func (b *mcXasm) spillStoreFP(r Reg, slot, size int) {
	mem := x64.At(RBP.mreg(), b.m.slotAddr(slot))
	if size == 8 {
		b.m.emit(x64.MovsdStore(r.mreg(), mem))
	} else {
		b.m.emit(x64.MovssStore(r.mreg(), mem))
	}
}
func (b *mcXasm) fail(format string, a ...any) { b.m.fail(fmt.Errorf(format, a...)) }
