package x64

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
	"github.com/stretchr/testify/require"
)

// The atomic encoders are checked the way the rest of the package is: our bytes
// are handed to llvm-mc and the instruction it reports back is compared against
// the one we meant to write. For an atomic that check earns its keep twice over,
// because llvm reports the LOCK prefix explicitly -- so "lock xaddl" versus
// "xaddl" is the difference between an atomic read-modify-write and a non-atomic
// one, and it is visible right here in the expectation string.
//
// check() cannot be reused directly for the locked forms: llvm-mc disassembles a
// prefixed instruction onto two lines ("lock", then the instruction) and check
// returns only the first, so a locked ADD and a bare LOCK prefix would compare
// equal. checkPrefixed keeps every line and joins them, which is what makes the
// prefix part of the assertion rather than something the comparison discards.
func checkPrefixed(t *testing.T, want string, code []byte) {
	t.Helper()
	var hexb []string
	for _, b := range code {
		hexb = append(hexb, fmt.Sprintf("0x%02x", b))
	}
	cmd := exec.Command(testenv.Tool(t, "llvm-mc"), "--triple=x86_64", "--disassemble")
	cmd.Stdin = strings.NewReader(strings.Join(hexb, " ") + "\n")
	out, err := cmd.Output()
	require.NoErrorf(t, err, "llvm-mc could not disassemble % x", code)

	var parts []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ".") {
			continue
		}
		parts = append(parts, normalize(line))
	}
	require.NotEmptyf(t, parts, "no instruction in llvm-mc disassembly:\n%s", out)
	require.Equalf(t, normalize(want), strings.Join(parts, " "), "for bytes %x", code)
}

func TestAtomicCmpxchg(t *testing.T) {
	checkPrefixed(t, "lock cmpxchgb %cl, (%rdi)", LockCmpxchg(8, At(RDI, 0), RCX))
	checkPrefixed(t, "lock cmpxchgw %cx, (%rdi)", LockCmpxchg(16, At(RDI, 0), RCX))
	checkPrefixed(t, "lock cmpxchgl %ecx, (%rdi)", LockCmpxchg(32, At(RDI, 0), RCX))
	checkPrefixed(t, "lock cmpxchgq %rcx, (%rdi)", LockCmpxchg(64, At(RDI, 0), RCX))
	checkPrefixed(t, "lock cmpxchgq %r10, 16(%rbp)", LockCmpxchg(64, At(RBP, 16), R10))
	checkPrefixed(t, "lock cmpxchgl %r11d, -8(%rbp)", LockCmpxchg(32, At(RBP, -8), R11))
	checkPrefixed(t, "lock cmpxchgq %rbx, 4096(%r12)", LockCmpxchg(64, At(R12, 4096), RBX))
	// A byte source in RSP..RDI is only addressable through an otherwise-empty REX.
	checkPrefixed(t, "lock cmpxchgb %sil, (%rdi)", LockCmpxchg(8, At(RDI, 0), RSI))
	// A RIP-relative destination: the disp32 is the last field, so a relocation
	// against a symbol patches the tail of the instruction.
	checkPrefixed(t, "lock cmpxchgl %ecx, (%rip)", LockCmpxchg(32, RIPRel(0), RCX))
}

func TestAtomicXadd(t *testing.T) {
	checkPrefixed(t, "lock xaddb %al, (%rdi)", LockXadd(8, At(RDI, 0), RAX))
	checkPrefixed(t, "lock xaddw %ax, (%rdi)", LockXadd(16, At(RDI, 0), RAX))
	checkPrefixed(t, "lock xaddl %ecx, 8(%rbx)", LockXadd(32, At(RBX, 8), RCX))
	checkPrefixed(t, "lock xaddq %r10, (%r11)", LockXadd(64, At(R11, 0), R10))
	checkPrefixed(t, "lock xaddq %rax, (%rsp)", LockXadd(64, At(RSP, 0), RAX))
	checkPrefixed(t, "lock xaddb %dil, -1(%rbp)", LockXadd(8, At(RBP, -1), RDI))
	checkPrefixed(t, "lock xaddl %r10d, (%rip)", LockXadd(32, RIPRel(0), R10))
}

func TestAtomicXchg(t *testing.T) {
	// No "lock" in any of these: a memory-operand XCHG is atomic without it.
	check(t, "xchgb %al, (%rdi)", Xchg(8, At(RDI, 0), RAX))
	check(t, "xchgw %ax, (%rdi)", Xchg(16, At(RDI, 0), RAX))
	check(t, "xchgl %ecx, (%rdi)", Xchg(32, At(RDI, 0), RCX))
	check(t, "xchgq %r10, (%r11)", Xchg(64, At(R11, 0), R10))
	check(t, "xchgq %rax, (%rdi)", Xchg(64, At(RDI, 0), RAX))
	check(t, "xchgb %bpl, 7(%rbx)", Xchg(8, At(RBX, 7), RBP))
	check(t, "xchgq %rbx, 4096(%r12)", Xchg(64, At(R12, 4096), RBX))
	check(t, "xchgl %eax, (%rip)", Xchg(32, RIPRel(0), RAX))
}

func TestAtomicLockedALU(t *testing.T) {
	// The memory-destination direction, which the package had no encoder for: these
	// write memory, where AddMem/AndMem/... write a register.
	checkPrefixed(t, "lock addl %ecx, (%rdi)", LockAdd(32, At(RDI, 0), RCX))
	checkPrefixed(t, "lock addb %cl, (%rdi)", LockAdd(8, At(RDI, 0), RCX))
	checkPrefixed(t, "lock addw %cx, (%rdi)", LockAdd(16, At(RDI, 0), RCX))
	checkPrefixed(t, "lock addq %r10, (%rbx)", LockAdd(64, At(RBX, 0), R10))
	checkPrefixed(t, "lock subl %ecx, (%rdi)", LockSub(32, At(RDI, 0), RCX))
	checkPrefixed(t, "lock subq %rax, -16(%rbp)", LockSub(64, At(RBP, -16), RAX))
	checkPrefixed(t, "lock andl %ecx, (%rdi)", LockAnd(32, At(RDI, 0), RCX))
	checkPrefixed(t, "lock andb %sil, (%rdi)", LockAnd(8, At(RDI, 0), RSI))
	checkPrefixed(t, "lock orl %ecx, (%rdi)", LockOr(32, At(RDI, 0), RCX))
	checkPrefixed(t, "lock orq %r11, 4096(%r12)", LockOr(64, At(R12, 4096), R11))
	checkPrefixed(t, "lock xorl %ecx, (%rdi)", LockXor(32, At(RDI, 0), RCX))
	checkPrefixed(t, "lock xorw %ax, 2(%rbx)", LockXor(16, At(RBX, 2), RAX))
	checkPrefixed(t, "lock xorq %rdx, (%rsp)", LockXor(64, At(RSP, 0), RDX))
}

func TestAtomicFence(t *testing.T) {
	check(t, "mfence", Mfence())
}
