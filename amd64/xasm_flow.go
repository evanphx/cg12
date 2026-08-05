package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// xasmFlow is every transfer of control: the branch forms, the address-of-a-label
// and jump-table primitives behind computed goto and switch, the two call forms,
// and ret. The frame teardown that precedes a ret is not here -- it is stack
// manipulation, and lives in xasmStack.
type xasmFlow interface {
	// Control flow. Branch targets are blocks so each backend formats its label;
	// jnz tests a register and branches to `to`, else falls through to `to2`.
	jmp(to *ir.Block)
	jnz(r Reg, w bool, to, to2 *ir.Block)
	jcc(cond x64.Cond, to, to2, next *ir.Block)
	jmpReg(r Reg)
	hlt()

	// blockAddrLea materializes a block's RIP-relative address into dst (&&label).
	blockAddrLea(dst Reg, blk *ir.Block)

	// jmpTable emits an indexed branch through a PC-relative offset table placed
	// just past the branch: target = table + (int32)table[idx]. idx is already
	// bounds-checked. Both GP scratch registers are free at a terminator.
	jmpTable(idx Reg, blk *ir.Block, targets []*ir.Block)

	// Calls. callSym is a direct call to a named function (recorded as a PLT32
	// relocation in object code); callReg is an indirect call through a register.
	callSym(sym string, off int64)
	callReg(r Reg)

	ret()
}

func (b *mcXasm) jmp(to *ir.Block) { b.m.prog.Jmp(to.Name) }
func (b *mcXasm) jnz(r Reg, w bool, to, to2 *ir.Block) {
	b.m.emit(x64.TestReg(w, r.mreg(), r.mreg()))
	b.m.prog.Jcc(x64.NE, to.Name)
	b.m.prog.Jmp(to2.Name)
}

// jcc branches on the flags a preceding cmp set: to `to` when cond holds, else to
// `to2` -- with the fall-through to `to2` elided when it is the next block.
func (b *mcXasm) jcc(cond x64.Cond, to, to2, next *ir.Block) {
	b.m.prog.Jcc(cond, to.Name)
	if to2 != next {
		b.m.prog.Jmp(to2.Name)
	}
}
func (b *mcXasm) jmpReg(r Reg) { b.m.emit(x64.JmpReg(r.mreg())) }
func (b *mcXasm) hlt()         { b.m.emit(x64.Ud2()) }
func (b *mcXasm) blockAddrLea(dst Reg, blk *ir.Block) {
	b.m.prog.LeaLabel(true, dst.mreg(), blk.Name)
}
func (b *mcXasm) jmpTable(idx Reg, blk *ir.Block, targets []*ir.Block) {
	tbl := blk.Name + ".tbl"
	b.m.prog.LeaLabel(true, b.m.gpScratch0.mreg(), tbl) // lea scratch0, [rip+tbl]
	b.m.emit(x64.MovsxdLoad(b.m.gpScratch1.mreg(), x64.Mem{Base: b.m.gpScratch0.mreg(), Index: idx.mreg(), Scale: 4, HasIndex: true}))
	b.m.emit(x64.AddReg(true, b.m.gpScratch0.mreg(), b.m.gpScratch1.mreg())) // add scratch0, scratch1
	b.m.emit(x64.JmpReg(b.m.gpScratch0.mreg()))                              // jmp *scratch0
	b.m.prog.Label(tbl)
	for _, t := range targets {
		b.m.prog.DataWord(t.Name, tbl) // .long t - tbl
	}
}
func (b *mcXasm) callSym(sym string, off int64) {
	b.m.emit(x64.CallRel(0))
	b.m.recordReloc(b.m.prog.Len()-4, sym, obj.R_X86_64_PLT32, off-4)
}
func (b *mcXasm) callReg(r Reg) { b.m.emit(x64.CallReg(r.mreg())) }
func (b *mcXasm) ret()          { b.m.emit(x64.Ret()) }
