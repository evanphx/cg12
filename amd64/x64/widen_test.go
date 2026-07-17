package x64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These encoders were already here and already checked against llvm-mc; the
// parser could not name any of them. syscall is the one that matters most --
// it is why most inline asm on this target exists at all, and cg12 could not
// assemble the word.
//
// Each line is assembled by us and disassembled by llvm-mc, and must come back
// as what we asked for. That is the same question the encoders answer, asked of
// the parser: reaching the right encoder with the wrong operands is no better.
func TestWidenedParsingMatchesReference(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"syscall", "syscall"},
		{"cdq", "cltd"}, // llvm prints the AT&T names for the sign-extends
		{"cqo", "cqto"},
		{"testq %rax, %rbx", "testq %rax, %rbx"},
		{"testl %eax, %ebx", "testl %eax, %ebx"},
		{"pushq %rbx", "pushq %rbx"},
		{"popq %rbx", "popq %rbx"},
		{"pushq %r12", "pushq %r12"},
		{"idivq %rcx", "idivq %rcx"},
		{"idivl %ecx", "idivl %ecx"},
		{"divq %rcx", "divq %rcx"},
		{"divl %ecx", "divl %ecx"},
	} {
		got, err := Assemble(c.src)
		require.NoErrorf(t, err, "we cannot assemble %q", c.src)
		check(t, c.want, got)
	}
}

// The operands still have to mean what they say.
func TestWidenedFormsRejectBadOperands(t *testing.T) {
	for _, c := range []struct{ src, why string }{
		{"pushq %ebx", "a 32-bit name in a 64-bit push"},
		{"pushl %ebx", "there is no 32-bit push on this target"},
		{"testq %eax, %rbx", "mismatched widths"},
		{"testb %al, %bl", "no byte encoders"},
		{"idivq $5", "an immediate divisor"},
		{"syscall %rax", "syscall takes no operands"},
	} {
		_, err := Assemble(c.src)
		require.Errorf(t, err, "%q (%s) must not assemble", c.src, c.why)
	}
}

// The SSE set, which the parser could not name either. Same gap, same
// consequence, one target over: floating-point inline asm did not compile on
// amd64 at all, because the assembler this backend hands templates to had no
// word for a single one of these instructions -- while arm64's had learned its
// whole FP set.
//
// llvm-mc is the judge, as above: reaching the right encoder with the operands
// the wrong way round assembles just as cleanly and computes something else.
func TestSSEParsingMatchesReference(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		// Arithmetic. AT&T names the source first, so these must come back with
		// xmm1 as the source and xmm0 as the destination, not reversed.
		{"addsd %xmm1, %xmm0", "addsd %xmm1, %xmm0"},
		{"subsd %xmm1, %xmm0", "subsd %xmm1, %xmm0"},
		{"mulsd %xmm1, %xmm0", "mulsd %xmm1, %xmm0"},
		{"divsd %xmm1, %xmm0", "divsd %xmm1, %xmm0"},
		{"addss %xmm3, %xmm2", "addss %xmm3, %xmm2"},
		{"subss %xmm3, %xmm2", "subss %xmm3, %xmm2"},
		{"mulss %xmm3, %xmm2", "mulss %xmm3, %xmm2"},
		{"divss %xmm3, %xmm2", "divss %xmm3, %xmm2"},
		// A high register, so the REX bit is exercised rather than assumed.
		{"addsd %xmm14, %xmm15", "addsd %xmm14, %xmm15"},
		// Moves, register and memory in both directions.
		{"movsd %xmm1, %xmm0", "movsd %xmm1, %xmm0"},
		{"movss %xmm1, %xmm0", "movss %xmm1, %xmm0"},
		{"movsd (%rax), %xmm0", "movsd (%rax), %xmm0"},
		{"movsd %xmm0, (%rax)", "movsd %xmm0, (%rax)"},
		{"movss (%rbx), %xmm2", "movss (%rbx), %xmm2"},
		{"movss %xmm2, (%rbx)", "movss %xmm2, (%rbx)"},
		// Compares and the sign-bit tricks.
		{"ucomisd %xmm1, %xmm0", "ucomisd %xmm1, %xmm0"},
		{"ucomiss %xmm1, %xmm0", "ucomiss %xmm1, %xmm0"},
		{"xorps %xmm1, %xmm0", "xorps %xmm1, %xmm0"},
		{"xorpd %xmm1, %xmm0", "xorpd %xmm1, %xmm0"},
		// Precision conversion between the two float widths.
		{"cvtsd2ss %xmm1, %xmm0", "cvtsd2ss %xmm1, %xmm0"},
		{"cvtss2sd %xmm1, %xmm0", "cvtss2sd %xmm1, %xmm0"},
	} {
		got, err := Assemble(c.src)
		require.NoErrorf(t, err, "we cannot assemble %q", c.src)
		check(t, c.want, got)
	}
}

// An SSE mnemonic with a general register, or an integer one with an SSE
// register, is a mistake rather than something to encode by guessing which the
// author meant. #105 contained the silent mis-encoding on both sides; this keeps
// the new mnemonics inside that.
func TestSSEWrongRegisterFileIsRejected(t *testing.T) {
	for _, src := range []string{
		"addsd %rax, %xmm0",   // a general register where an SSE one belongs
		"addsd %xmm0, %rax",   // and the other way round
		"movsd %eax, %xmm0",   // narrow general register
		"ucomisd %xmm0, %rbx", // compare into a general register
		"addq %xmm0, %rax",    // an integer mnemonic on an SSE register
	} {
		_, err := Assemble(src)
		require.Errorf(t, err, "%q must not assemble to something", src)
	}
}
