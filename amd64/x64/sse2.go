package x64

// SSE2 whole-register 128-bit moves. These are the only x86-64 instructions that
// move sixteen bytes in one go, so they are what a 128-bit IR load or store
// (ir.OLoadq / ir.OStoreq) becomes; there is no 128-bit form of MOV.
//
// Aligned versus unaligned is a correctness decision here, not a tuning one.
// MOVDQA raises #GP(0) when its memory operand is not 16-byte aligned, and the
// address of a 128-bit IR access is an arbitrary pointer: a folded
// base+index*scale+disp form, a field inside an aggregate, a byte-offset cast.
// Nothing in the IR promises alignment, and a fault is not a performance
// regression. So the two memory forms below are MOVDQU only, and MOVDQA appears
// solely in its register-to-register form -- which has no memory operand and
// therefore no alignment requirement at all. On every microarchitecture this
// backend targets MOVDQU on an address that happens to be aligned costs exactly
// what MOVDQA would, so nothing is given up by never proving alignment.

// movdquPfx is MOVDQU's mandatory F3 prefix. It is the same byte as fp.go's
// scalar-single prefix but does a different job -- F3 selects the instruction
// here rather than an operand size -- so it is named for this use.
var movdquPfx = []byte{0xf3}

// MovdquLoad loads 16 bytes from memory into an XMM register (F3 0F 6F /r),
// without requiring the address to be 16-byte aligned.
func MovdquLoad(dst Reg, m Mem) []byte {
	return op_rm(nil, movdquPfx, false, []byte{0x0f, 0x6f}, dst, m, false)
}

// MovdquStore stores an XMM register's 16 bytes to memory (F3 0F 7F /r), without
// requiring the address to be 16-byte aligned. MOVDQU has a separate opcode per
// direction rather than the usual load/store pair off one opcode, so the store is
// 7F where the load is 6F.
func MovdquStore(src Reg, m Mem) []byte {
	return op_rm(nil, movdquPfx, false, []byte{0x0f, 0x7f}, src, m, false)
}

// MovdqaReg copies all 128 bits of one XMM register to another (66 0F 6F /r).
// Both operands are registers, so MOVDQA's alignment requirement -- which applies
// only to a memory operand -- cannot be violated; this is the whole-vector
// register move, the analogue of arm64's `mov vd.16b, vn.16b`.
func MovdqaReg(dst, src Reg) []byte {
	return op_rr(nil, pfx66, false, []byte{0x0f, 0x6f}, dst, src, false)
}
