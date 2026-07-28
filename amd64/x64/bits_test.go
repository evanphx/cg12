package x64

import "testing"

// The bit-manipulation encoders, checked the same way as every other encoder in
// this package: our bytes are handed to llvm-mc's disassembler and the
// instruction it reports is compared against the one we meant. That direction
// matters for BSR in particular -- it differs from LZCNT only by an F3 prefix, so
// a stray prefix byte would still disassemble to *something*, just not to this.
func TestBitScan(t *testing.T) {
	check(t, "bsrq %rcx, %rax", Bsr(true, RAX, RCX))
	check(t, "bsrl %esi, %edi", Bsr(false, RDI, RSI))
	check(t, "bsrq %r11, %rcx", Bsr(true, RCX, R11))
	check(t, "bsrl %r11d, %r10d", Bsr(false, R10, R11))
	check(t, "bsrq %rax, %r15", Bsr(true, R15, RAX))
}

func TestWideMultiply(t *testing.T) {
	check(t, "mulq %rcx", MulWide(true, RCX))
	check(t, "mulq %r11", MulWide(true, R11))
	check(t, "mull %esi", MulWide(false, RSI))
	check(t, "imulq %rcx", ImulWide(true, RCX))
	check(t, "imulq %r11", ImulWide(true, R11))
	check(t, "imull %r11d", ImulWide(false, R11))
	// The one-operand IMUL must not be confused with the two-operand form, which
	// keeps only the low half of the product; llvm spells them apart by arity.
	check(t, "imulq %rcx, %rax", Imul(true, RAX, RCX))
}
