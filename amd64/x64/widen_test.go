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
