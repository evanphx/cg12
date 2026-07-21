package x64

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
	"github.com/stretchr/testify/require"
)

// disasm decodes machine-code bytes with llvm-mc and returns the single
// disassembled instruction, normalized. Validating by disassembly (rather than
// re-assembling a mnemonic and comparing bytes) checks that our bytes mean the
// intended instruction regardless of which legal encoding we picked — llvm often
// prefers a different-but-equivalent form (accumulator short forms, shift-by-1,
// imm8 imul). llvm-mc is only ever a check, never used at runtime.
func disasm(t *testing.T, code []byte) string {
	t.Helper()
	var hexb []string
	for _, b := range code {
		hexb = append(hexb, fmt.Sprintf("0x%02x", b))
	}
	cmd := exec.Command(testenv.Tool(t, "llvm-mc"), "--triple=x86_64", "--disassemble")
	cmd.Stdin = strings.NewReader(strings.Join(hexb, " ") + "\n")
	out, err := cmd.Output()
	require.NoErrorf(t, err, "llvm-mc could not disassemble % x", code)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ".") {
			continue
		}
		return normalize(line)
	}
	t.Fatalf("no instruction in llvm-mc disassembly:\n%s", out)
	return ""
}

// normalize collapses whitespace and strips llvm's trailing "# ..." comments so
// disassembled instructions compare as canonical single-spaced strings.
func normalize(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	return strings.Join(strings.Fields(s), " ")
}

// check asserts that our encoded bytes disassemble to the expected instruction.
func check(t *testing.T, want string, got []byte) {
	t.Helper()
	require.Equalf(t, normalize(want), disasm(t, got), "for bytes %x", got)
}

func TestMovReg(t *testing.T) {
	check(t, "movq %rcx, %rax", MovReg(true, RAX, RCX))
	check(t, "movq %rax, %rcx", MovReg(true, RCX, RAX))
	check(t, "movl %esi, %edi", MovReg(false, RDI, RSI))
	check(t, "movq %r8, %r15", MovReg(true, R15, R8))
	check(t, "movq %rsp, %rbp", MovReg(true, RBP, RSP))
	check(t, "movl %r10d, %eax", MovReg(false, RAX, R10))
}

func TestMovImm(t *testing.T) {
	check(t, "movq $1, %rax", MovImm32(true, RAX, 1))
	check(t, "movq $-5, %rdx", MovImm32(true, RDX, -5))
	check(t, "movl $305419896, %ebx", MovImm32(false, RBX, 0x12345678))
	check(t, "movq $305419896, %r9", MovImm32(true, R9, 0x12345678))
	check(t, "movabsq $1234605616436508552, %rax", MovImm64(RAX, 0x1122334455667788))
	check(t, "movabsq $-1, %r12", MovImm64(R12, -1))
}

func TestLoadStore(t *testing.T) {
	check(t, "movq (%rbx), %rax", Load(true, RAX, At(RBX, 0)))
	check(t, "movq 8(%rbx), %rax", Load(true, RAX, At(RBX, 8)))
	check(t, "movq -16(%rbp), %rcx", Load(true, RCX, At(RBP, -16)))
	check(t, "movl 4(%rsp), %edx", Load(false, RDX, At(RSP, 4)))
	check(t, "movq 4096(%r12), %r13", Load(true, R13, At(R12, 4096)))
	check(t, "movq %rax, (%rbx)", Store(64, RAX, At(RBX, 0)))
	check(t, "movq %rax, 8(%rbx)", Store(64, RAX, At(RBX, 8)))
	check(t, "movl %edx, 4(%rsp)", Store(32, RDX, At(RSP, 4)))
	check(t, "movb %al, (%rdi)", Store(8, RAX, At(RDI, 0)))
	check(t, "movb %sil, (%rdi)", Store(8, RSI, At(RDI, 0)))
	check(t, "movw %ax, 2(%rbx)", Store(16, RAX, At(RBX, 2)))
	check(t, "movq %rbp, (%rbp)", Store(64, RBP, At(RBP, 0)))
}

func TestAluMem(t *testing.T) {
	check(t, "addq -16(%rbp), %rax", AddMem(true, RAX, At(RBP, -16)))
	check(t, "addl 8(%rbx), %ecx", AddMem(false, RCX, At(RBX, 8)))
	check(t, "subq 8(%rbx), %rcx", SubMem(true, RCX, At(RBX, 8)))
	check(t, "andq (%rsp), %rdx", AndMem(true, RDX, At(RSP, 0)))
	check(t, "orq -8(%rbp), %rsi", OrMem(true, RSI, At(RBP, -8)))
	check(t, "xorl 4(%r12), %r13d", XorMem(false, R13, At(R12, 4)))
	check(t, "cmpq -8(%rbp), %r12", CmpMem(true, R12, At(RBP, -8)))
	check(t, "imulq 16(%r13), %rax", ImulMem(true, RAX, At(R13, 16)))
}

func TestLoadStoreRIP(t *testing.T) {
	// RIP-relative with disp 0: the disp32 is where a PC-relative relocation
	// against a symbol will be patched; llvm disassembles it as (%rip).
	check(t, "movq (%rip), %rax", Load(true, RAX, RIPRel(0)))
	check(t, "leaq (%rip), %rdi", Lea(true, RDI, RIPRel(0)))
}

func TestAlu(t *testing.T) {
	check(t, "addq %rcx, %rax", AddReg(true, RAX, RCX))
	check(t, "subq %rdx, %rbx", SubReg(true, RBX, RDX))
	check(t, "andl %esi, %edi", AndReg(false, RDI, RSI))
	check(t, "orq %r8, %r9", OrReg(true, R9, R8))
	check(t, "xorl %eax, %eax", XorReg(false, RAX, RAX))
	check(t, "cmpq %rsi, %rdi", CmpReg(true, RDI, RSI))
	check(t, "testq %rbx, %rax", TestReg(true, RAX, RBX)) // test is symmetric; reg/rm swap in display
}

func TestAluImm(t *testing.T) {
	check(t, "addq $5, %rax", AddImm(true, RAX, 5))
	check(t, "addq $-1, %rax", AddImm(true, RAX, -1))
	check(t, "addq $1000, %rax", AddImm(true, RAX, 1000))
	check(t, "subq $16, %rsp", SubImm(true, RSP, 16))
	check(t, "andl $255, %eax", AndImm(false, RAX, 255))
	check(t, "orq $4096, %rbx", OrImm(true, RBX, 4096))
	check(t, "xorq $7, %r10", XorImm(true, R10, 7))
	check(t, "cmpq $0, %rdi", CmpImm(true, RDI, 0))
	check(t, "cmpl $100, %esi", CmpImm(false, RSI, 100))
}

func TestMulDiv(t *testing.T) {
	check(t, "imulq %rcx, %rax", Imul(true, RAX, RCX))
	check(t, "imull %esi, %edi", Imul(false, RDI, RSI))
	check(t, "imulq $10, %rax, %rbx", ImulImm(true, RBX, RAX, 10))
	check(t, "idivq %rcx", Idiv(true, RCX))
	check(t, "divq %r8", Div(true, R8))
	check(t, "cqto", Cqo())
	check(t, "cltd", Cdq())
	check(t, "negq %rax", Neg(true, RAX))
	check(t, "notq %rbx", Not(true, RBX))
}

func TestShift(t *testing.T) {
	check(t, "shlq %cl, %rax", ShlCL(true, RAX))
	check(t, "shrq %cl, %rbx", ShrCL(true, RBX))
	check(t, "sarq %cl, %rcx", SarCL(true, RCX))
	check(t, "shlq $3, %rax", ShlImm(true, RAX, 3))
	check(t, "shrl $1, %edx", ShrImm(false, RDX, 1))
	check(t, "sarq $63, %r9", SarImm(true, R9, 63))
}

func TestExtend(t *testing.T) {
	check(t, "movzbl %al, %eax", MovzxByte(false, RAX, RAX))
	check(t, "movzbq %sil, %rdi", MovzxByte(true, RDI, RSI))
	check(t, "movzwl %ax, %eax", MovzxWord(false, RAX, RAX))
	check(t, "movsbl %al, %eax", MovsxByte(false, RAX, RAX))
	check(t, "movsbq %dil, %rax", MovsxByte(true, RAX, RDI))
	check(t, "movswq %ax, %rax", MovsxWord(true, RAX, RAX))
	check(t, "movslq %eax, %rax", Movsxd(RAX, RAX))
	check(t, "movslq %r10d, %rbx", Movsxd(RBX, R10))
}

func TestExtendLoad(t *testing.T) {
	check(t, "movzbl (%rbx), %eax", MovzxLoadByte(false, RAX, At(RBX, 0)))
	check(t, "movzbq 8(%rsp), %rax", MovzxLoadByte(true, RAX, At(RSP, 8)))
	check(t, "movzwl (%rdi), %ecx", MovzxLoadWord(false, RCX, At(RDI, 0)))
	check(t, "movsbl 1(%rbx), %eax", MovsxLoadByte(false, RAX, At(RBX, 1)))
	check(t, "movswq 2(%rbx), %rax", MovsxLoadWord(true, RAX, At(RBX, 2)))
}

func TestLea(t *testing.T) {
	check(t, "leaq 16(%rsp), %rax", Lea(true, RAX, At(RSP, 16)))
	check(t, "leaq -8(%rbp), %rdi", Lea(true, RDI, At(RBP, -8)))
	check(t, "leaq (%rbx,%rcx,4), %rax", Lea(true, RAX, Mem{Base: RBX, Index: RCX, Scale: 4, HasIndex: true}))
}

func TestStoreImm(t *testing.T) {
	check(t, "movq $0, (%rbx)", StoreImm32(64, At(RBX, 0), 0))
	check(t, "movq $-1, 8(%rsp)", StoreImm32(64, At(RSP, 8), -1))
	check(t, "movl $42, (%rdi)", StoreImm32(32, At(RDI, 0), 42))
}

func TestBranch(t *testing.T) {
	// llvm disassembles a PC-relative branch as its raw displacement operand.
	check(t, "jmp 0", JmpRel(0))
	check(t, "jmp 16", JmpRel(16))
	check(t, "callq 0", CallRel(0))
	check(t, "jne 32", Jcc(NE, 32))
	check(t, "je 5", Jcc(E, 5))
	check(t, "jmpq *%rax", JmpReg(RAX))
	check(t, "callq *%rax", CallReg(RAX))
	check(t, "callq *%r11", CallReg(R11))
	check(t, "retq", Ret())
	check(t, "pushq %rax", Push(RAX))
	check(t, "pushq %r12", Push(R12))
	check(t, "popq %rbp", Pop(RBP))
	check(t, "popq %r15", Pop(R15))
	check(t, "syscall", Syscall())
}

func TestSetccCmov(t *testing.T) {
	check(t, "sete %al", Setcc(E, RAX))
	check(t, "setne %sil", Setcc(NE, RSI))
	check(t, "setl %r10b", Setcc(L, R10))
	check(t, "cmovlq %rcx, %rax", Cmovcc(true, L, RAX, RCX))
	check(t, "cmovaeq %rbx, %rdx", Cmovcc(true, AE, RDX, RBX))
}

func TestFPArith(t *testing.T) {
	check(t, "addsd %xmm1, %xmm0", Addsd(XMM0, XMM1))
	check(t, "subsd %xmm2, %xmm3", Subsd(XMM3, XMM2))
	check(t, "mulsd %xmm4, %xmm5", Mulsd(XMM5, XMM4))
	check(t, "divsd %xmm7, %xmm6", Divsd(XMM6, XMM7))
	check(t, "addss %xmm1, %xmm0", Addss(XMM0, XMM1))
	check(t, "mulss %xmm2, %xmm0", Mulss(XMM0, XMM2))
}

func TestFPMove(t *testing.T) {
	check(t, "movsd %xmm1, %xmm0", MovsdReg(XMM0, XMM1))
	check(t, "movss %xmm3, %xmm2", MovssReg(XMM2, XMM3))
	check(t, "movsd 8(%rsp), %xmm0", MovsdLoad(XMM0, At(RSP, 8)))
	check(t, "movsd %xmm0, 8(%rsp)", MovsdStore(XMM0, At(RSP, 8)))
	check(t, "movss (%rbx), %xmm1", MovssLoad(XMM1, At(RBX, 0)))
}

func TestFPCompareBitwise(t *testing.T) {
	check(t, "ucomisd %xmm1, %xmm0", Ucomisd(XMM0, XMM1))
	check(t, "ucomiss %xmm1, %xmm0", Ucomiss(XMM0, XMM1))
	check(t, "xorps %xmm0, %xmm0", Xorps(XMM0, XMM0))
	check(t, "xorpd %xmm1, %xmm2", Xorpd(XMM2, XMM1))
}

func TestFPConvert(t *testing.T) {
	check(t, "cvtsi2sd %rax, %xmm0", Cvtsi2sd(true, XMM0, RAX))
	check(t, "cvtsi2sd %ecx, %xmm1", Cvtsi2sd(false, XMM1, RCX))
	check(t, "cvtsi2ss %rax, %xmm0", Cvtsi2ss(true, XMM0, RAX))
	check(t, "cvttsd2si %xmm0, %rax", Cvttsd2si(true, RAX, XMM0))
	check(t, "cvttsd2si %xmm1, %ecx", Cvttsd2si(false, RCX, XMM1))
	check(t, "cvttss2si %xmm0, %rdx", Cvttss2si(true, RDX, XMM0))
	check(t, "cvtsd2ss %xmm1, %xmm0", Cvtsd2ss(XMM0, XMM1))
	check(t, "cvtss2sd %xmm2, %xmm3", Cvtss2sd(XMM3, XMM2))
}

func TestTLSEncodings(t *testing.T) {
	check(t, "movq %fs:0, %rax", MovFSZero(RAX))
	check(t, "movq %fs:0, %r10", MovFSZero(R10))
	check(t, "addq $0, %rax", AddImm32(true, RAX, 0))
	check(t, "addq $305419896, %rbx", AddImm32(true, RBX, 0x12345678))
}

func TestGPRXMM(t *testing.T) {
	check(t, "movq %rax, %xmm0", MovqToXmm(true, XMM0, RAX))
	check(t, "movd %ecx, %xmm1", MovqToXmm(false, XMM1, RCX))
	check(t, "movq %xmm0, %rax", MovqFromXmm(true, RAX, XMM0))
	check(t, "movd %xmm1, %ecx", MovqFromXmm(false, RCX, XMM1))
}
