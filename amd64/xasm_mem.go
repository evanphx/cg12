package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmMem is the scalar load/store surface.
//
// Note what these methods take: the GP forms are handed the whole instruction and
// the FP forms an address operand, rather than a resolved address. Each method
// resolves the address (base+disp, RIP-relative symbol, or a computed pointer)
// internally, so the addressing model stays per-backend -- x86's memory operands
// are richer than one signature could usefully describe.
type xasmMem interface {
	loadGP(in *ir.Instr, w bool, dst Reg)
	loadFP(op ir.Op, dst Reg, addr ir.Ref)
	storeGP(in *ir.Instr, val Reg)
	storeFP(op ir.Op, val Reg, addr ir.Ref)
}

func (b *mcXasm) loadGP(in *ir.Instr, w bool, dst Reg) {
	op := in.Op
	mem, fixup := b.m.memFor(in, 0)
	dm := dst.mreg()
	switch op {
	case ir.OLoadub:
		b.m.emit(x64.MovzxLoadByte(w, dm, mem))
	case ir.OLoadsb:
		b.m.emit(x64.MovsxLoadByte(w, dm, mem))
	case ir.OLoaduh:
		b.m.emit(x64.MovzxLoadWord(w, dm, mem))
	case ir.OLoadsh:
		b.m.emit(x64.MovsxLoadWord(w, dm, mem))
	case ir.OLoaduw:
		b.m.emit(x64.Load(false, dm, mem)) // a 32-bit load zero-extends
	case ir.OLoadsw:
		b.m.emit(x64.MovsxdLoad(dm, mem))
	case ir.OLoadl:
		b.m.emit(x64.Load(true, dm, mem))
	}
	fixup()
}
func (b *mcXasm) loadFP(op ir.Op, dst Reg, addr ir.Ref) {
	mem, fixup := b.m.memAddr(addr, gpScratch1)
	if op == ir.OLoadd {
		b.m.emit(x64.MovsdLoad(dst.mreg(), mem))
	} else {
		b.m.emit(x64.MovssLoad(dst.mreg(), mem))
	}
	fixup()
}
func (b *mcXasm) storeGP(in *ir.Instr, val Reg) {
	mem, fixup := b.m.memFor(in, 1)
	sz := map[ir.Op]int{ir.OStoreb: 8, ir.OStoreh: 16, ir.OStorew: 32, ir.OStorel: 64}[in.Op]
	b.m.emit(x64.Store(sz, val.mreg(), mem))
	fixup()
}
func (b *mcXasm) storeFP(op ir.Op, val Reg, addr ir.Ref) {
	mem, fixup := b.m.memAddr(addr, gpScratch1)
	if op == ir.OStored {
		b.m.emit(x64.MovsdStore(val.mreg(), mem))
	} else {
		b.m.emit(x64.MovssStore(val.mreg(), mem))
	}
	fixup()
}
