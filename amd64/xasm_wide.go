package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmWide is 128-bit memory traffic: the OLoadq/OStoreq pair, plus the whole-
// vector register copy the pair's values need in order to be moved around at all.
//
// The load and store take the whole instruction rather than a resolved address,
// exactly as the scalar forms in xasm_mem.go do, and for the same reason: the
// address arrives already folded into an x86 base+index*scale+disp operand by
// foldaddr.go, and only memFor knows how to read that back off the instruction. It
// is what lets the 128-bit ops share every addressing mode the scalar ops have --
// and removing them from foldaddr.go's skip list is sound precisely because these
// two methods go through memFor, where the FP scalar path (which ignores Aux and
// Amode) is why that skip still covers OLoads/OLoadd/OStores/OStored.
type xasmWide interface {
	loadWide(in *ir.Instr, dst Reg)
	storeWide(in *ir.Instr, val Reg)
	movWide(dst, src Reg)
}

func (b *mcXasm) loadWide(in *ir.Instr, dst Reg) {
	mem, fixup := b.m.memFor(in, 0)
	b.m.emit(x64.MovdquLoad(dst.mreg(), mem))
	fixup()
}

func (b *mcXasm) storeWide(in *ir.Instr, val Reg) {
	mem, fixup := b.m.memFor(in, 1)
	b.m.emit(x64.MovdquStore(val.mreg(), mem))
	fixup()
}

// movWide copies all 128 bits between XMM registers. It is MOVDQA, not MOVDQU:
// with two register operands there is no memory operand to be misaligned, so the
// aligned form's requirement cannot be violated and its shorter prefix is free.
func (b *mcXasm) movWide(dst, src Reg) {
	if dst == src {
		return
	}
	b.m.emit(x64.MovdqaReg(dst.mreg(), src.mreg()))
}
