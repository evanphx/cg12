package amd64

import "github.com/evanphx/cg12/amd64/x64"

// xasmStack is everything that reads or moves the stack pointer: the runtime-sized
// allocation behind a VLA, the save/restore pair around it, the two callers'-frame
// queries, and the teardown half of the frame. Teardown sits here rather than with
// the ret it precedes because it is stack manipulation -- and because it is shared
// by two callers, the return epilogue and the tail-call branch.
type xasmStack interface {
	// Stack pointer: allocNSP grows the stack by a runtime size (VLA) and yields
	// the new top; movFromSP/movToSP snapshot and restore rsp.
	allocNSP(dst, size Reg)
	movFromSP(dst Reg)
	movToSP(src Reg)
	callerPC(dst Reg)
	callerSP(dst Reg)

	// Frame teardown, shared by the return epilogue and the tail-call branch:
	// restoreGP reloads a callee-saved register from its RBP-relative slot and
	// framePop unwinds the frame (mov rsp,rbp; pop rbp).
	restoreGP(r Reg, off int32)
	framePop()
}

func (b *mcXasm) allocNSP(dst, size Reg) {
	b.m.emit(x64.SubReg(true, RSP.mreg(), size.mreg()))
	b.m.emit(x64.MovReg(true, dst.mreg(), RSP.mreg()))
}
func (b *mcXasm) movFromSP(dst Reg) { b.m.emit(x64.MovReg(true, dst.mreg(), RSP.mreg())) }
func (b *mcXasm) movToSP(src Reg)   { b.m.emit(x64.MovReg(true, RSP.mreg(), src.mreg())) }
func (b *mcXasm) callerPC(dst Reg)  { b.m.emit(x64.Load(true, dst.mreg(), x64.At(RBP.mreg(), 8))) }
func (b *mcXasm) callerSP(dst Reg)  { b.m.emit(x64.Lea(true, dst.mreg(), x64.At(RBP.mreg(), 16))) }
func (b *mcXasm) restoreGP(r Reg, off int32) {
	b.m.emit(x64.Load(true, r.mreg(), x64.At(RBP.mreg(), off)))
}
func (b *mcXasm) framePop() {
	b.m.emit(x64.MovReg(true, RSP.mreg(), RBP.mreg()))
	b.m.emit(x64.Pop(RBP.mreg()))
}
