package x64

import "testing"

// The 128-bit moves are checked the same way as every other encoder in this
// package: our bytes are handed to llvm-mc and the instruction it reads back must
// be the one we meant. That matters more than usual here, because MOVDQU's two
// directions are different opcodes (6F loads, 7F stores) rather than the
// load/store pair of one opcode used elsewhere, and swapping them would still
// produce a legal instruction -- one that moves the data the wrong way.

func TestMovdquLoadStore(t *testing.T) {
	check(t, "movdqu (%rbx), %xmm0", MovdquLoad(XMM0, At(RBX, 0)))
	check(t, "movdqu -32(%rbp), %xmm1", MovdquLoad(XMM1, At(RBP, -32)))
	check(t, "movdqu 4096(%r12), %xmm7", MovdquLoad(XMM7, At(R12, 4096)))
	// XMM8..15 and R8..15 both need their REX extension bits, in the reg and the
	// rm field respectively, and the mandatory F3 prefix must precede the REX byte.
	check(t, "movdqu 16(%r13), %xmm14", MovdquLoad(Reg(14), At(R13, 16)))

	check(t, "movdqu %xmm0, (%rbx)", MovdquStore(XMM0, At(RBX, 0)))
	check(t, "movdqu %xmm1, -32(%rbp)", MovdquStore(XMM1, At(RBP, -32)))
	check(t, "movdqu %xmm15, 8(%rsp)", MovdquStore(Reg(15), At(RSP, 8)))
	check(t, "movdqu %xmm3, 4096(%r12)", MovdquStore(XMM3, At(R12, 4096)))
}

// A 128-bit access reaches the SIB forms through the address fold, so the indexed
// and RIP-relative operands are checked too: the fold hands memory operands to
// these encoders that the scalar ones never see.
func TestMovdquAddressingForms(t *testing.T) {
	check(t, "movdqu (%rbp,%rax,8), %xmm2",
		MovdquLoad(XMM2, Mem{Base: RBP, Index: RAX, Scale: 8, HasIndex: true}))
	check(t, "movdqu -48(%rbp,%rcx,4), %xmm5",
		MovdquLoad(XMM5, Mem{Base: RBP, Index: RCX, Scale: 4, HasIndex: true, Disp: -48}))
	check(t, "movdqu %xmm6, -48(%rbp,%r11,2)",
		MovdquStore(XMM6, Mem{Base: RBP, Index: R11, Scale: 2, HasIndex: true, Disp: -48}))
	// llvm elides a scale of 1 when it prints the operand.
	check(t, "movdqu %xmm4, (%rbp,%rdx)",
		MovdquStore(XMM4, Mem{Base: RBP, Index: RDX, Scale: 1, HasIndex: true}))
	// A symbol reference is [rip + disp32]; the backend patches the disp32 with a
	// PC32 relocation, which needs the displacement to be the instruction's last
	// four bytes -- true only because MOVDQU carries no immediate.
	check(t, "movdqu (%rip), %xmm0", MovdquLoad(XMM0, RIPRel(0)))
	check(t, "movdqu %xmm0, (%rip)", MovdquStore(XMM0, RIPRel(0)))
}

func TestMovdqaReg(t *testing.T) {
	check(t, "movdqa %xmm1, %xmm0", MovdqaReg(XMM0, XMM1))
	check(t, "movdqa %xmm0, %xmm1", MovdqaReg(XMM1, XMM0))
	check(t, "movdqa %xmm15, %xmm14", MovdqaReg(Reg(14), Reg(15)))
	check(t, "movdqa %xmm2, %xmm9", MovdqaReg(Reg(9), XMM2))
}
