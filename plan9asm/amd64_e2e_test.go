package plan9asm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// amd64EndToEndSource exercises the frame and ABI machinery: a frame with a
// saved BP (the function calls), FP arguments of mixed widths including a
// float, named pseudo-SP locals next to hardware SP slots, an FP access after
// PUSHQ (obj6 tracks the SP delta), an ABI0 call to a symbol the backend
// provides, a PC-relative branch, a floating-point immediate, and two results
// of different classes. loadg reads g through TLS with the go_tls.h macros;
// bump has no arguments, so its plain name is the body itself; twice is
// declared <ABIInternal>.
const amd64EndToEndSource = `#include "textflag.h"
#include "go_tls.h"

// func compute(a, b int64, c int32, f float64) (sum int64, prod float64)
TEXT ·compute(SB), NOSPLIT, $40-48
	MOVQ	a+0(FP), AX
	MOVQ	b+8(FP), BX
	MOVQ	AX, x-8(SP)
	MOVQ	BX, y-16(SP)
	MOVLQSX	c+16(FP), CX
	PUSHQ	CX
	MOVQ	a+0(FP), DX
	POPQ	CX
	ADDQ	x-8(SP), CX
	ADDQ	y-16(SP), CX
	MOVQ	DX, 0(SP)
	MOVQ	CX, 8(SP)
	CALL	·scale(SB)
	MOVQ	16(SP), CX
	CMPQ	CX, $0
	JGE	2(PC)
	NEGQ	CX
	MOVQ	CX, sum+32(FP)
	MOVSD	f+24(FP), X0
	MULSD	$2.5, X0
	MOVSD	X0, prod+40(FP)
	RET

// func loadg() uintptr
TEXT ·loadg(SB), NOSPLIT, $0-8
	get_tls(CX)
	MOVQ	g(CX), AX
	MOVQ	AX, ret+0(FP)
	RET

// func many(a1, a2, a3, a4, a5, a6, a7, a8, a9 int64) (sum, spread int64)
TEXT ·many(SB), NOSPLIT, $0-88
	MOVQ	a1+0(FP), AX
	ADDQ	a2+8(FP), AX
	ADDQ	a3+16(FP), AX
	ADDQ	a4+24(FP), AX
	ADDQ	a5+32(FP), AX
	ADDQ	a6+40(FP), AX
	ADDQ	a7+48(FP), AX
	ADDQ	a8+56(FP), AX
	ADDQ	a9+64(FP), AX
	MOVQ	AX, sum+72(FP)
	MOVQ	a9+64(FP), AX
	SUBQ	a1+0(FP), AX
	MOVQ	AX, spread+80(FP)
	RET

TEXT ·bump(SB), NOSPLIT, $0
	INCQ	·counter(SB)
	RET

TEXT ·twice<ABIInternal>(SB), NOSPLIT, $0
	ADDQ	AX, AX
	RET

// func triple(a, b int64, f float64) (sum, diff int64, square float64)
// returns all three results in registers (AX, BX, X0), as ABIInternal does;
// its plain name is the adapter that stores diff and square through the
// pointers cg12 passes in CX and DI.
TEXT ·triple<ABIInternal>(SB), NOSPLIT, $0
	MOVQ	AX, CX
	ADDQ	BX, AX
	SUBQ	BX, CX
	MOVQ	CX, BX
	MULSD	X0, X0
	RET

GLOBL ·counter(SB), NOPTR, $8
`

// amd64EndToEndStub plays the backend's part: SysV-callable entry points that
// load the ABIInternal argument registers and call the translated entries,
// and the ABI0 shim for scale (a Go function the assembly calls), which here
// computes a*s from its ABI0 stack arguments.
const amd64EndToEndStub = `
	.text
	.globl call_compute
call_compute:
	pushq %rbx
	pushq %rbp
	pushq %r12
	pushq %r13
	pushq %r14
	pushq %r15
	# ABIInternal preserves only RSP, RBP, R14, and X15 across a call, so
	# the out-pointers wait on the stack.
	pushq %r8
	pushq %r9
	movq %rcx, %r12
	movq %rdi, %rax
	movq %rsi, %rbx
	movl %edx, %ecx
	movq %r12, %rdi
	movq $0xdead, %r14
	pcmpeqd %xmm15, %xmm15
	call e2e_compute
	popq %r9
	popq %r8
	movq %r14, (%r8)
	movq %xmm15, (%r9)
	popq %r15
	popq %r14
	popq %r13
	popq %r12
	popq %rbp
	popq %rbx
	ret

	# int64 call_many(int64 *second): many(1, 2, ..., 9); the pointer to the
	# second result no longer fits in a register and goes on the stack.
	.globl call_many
call_many:
	pushq %rbx
	pushq %r14
	pushq %rdi
	movq $1, %rax
	movq $2, %rbx
	movq $3, %rcx
	movq $4, %rdi
	movq $5, %rsi
	movq $6, %r8
	movq $7, %r9
	movq $8, %r10
	movq $9, %r11
	call e2e_many
	popq %rdi
	popq %r14
	popq %rbx
	ret

	.globl call_loadg
call_loadg:
	pushq %rbx
	pushq %r14
	call e2e_loadg
	popq %r14
	popq %rbx
	ret

	# int64 call_triple(int64 a, int64 b, double f, int64 *diff, double *square)
	.globl call_triple
call_triple:
	pushq %rbx
	pushq %r14
	movq %rdi, %rax
	movq %rsi, %rbx
	movq %rcx, %rdi
	movq %rdx, %rcx
	call e2e_triple
	popq %r14
	popq %rbx
	ret

	.globl call_twice
call_twice:
	movq %rdi, %rax
	call e2e_twice
	ret

	.globl set_tls_g
set_tls_g:
	movq %rdi, %fs:runtime_tlsg@tpoff
	ret

	.globl tlsg_tpoff
tlsg_tpoff:
	movq $runtime_tlsg@tpoff, %rax
	ret

	.globl e2e_scale_abi0
e2e_scale_abi0:
	movq 8(%rsp), %rax
	imulq 16(%rsp), %rax
	movq %rax, 24(%rsp)
	ret
	.section .note.GNU-stack,"",@progbits
`

const amd64EndToEndDriver = `#include <stdint.h>
#include <stdio.h>

extern int64_t call_compute(int64_t, int64_t, int32_t, double, double *, uint64_t *, uint64_t *);
extern int64_t call_many(int64_t *);
extern uint64_t call_loadg(void);
extern int64_t call_twice(int64_t);
extern int64_t call_triple(int64_t, int64_t, double, int64_t *, double *);
extern void set_tls_g(uint64_t);
extern int64_t tlsg_tpoff(void);
extern void e2e_bump(void);
extern int64_t e2e_counter;

int main(void) {
	double prod = 0;
	uint64_t r14 = 0, x15 = 1;
	set_tls_g(0x1234abcd);
	int64_t sum = call_compute(3, -10, 1, 1.5, &prod, &r14, &x15);
	printf("sum=%lld prod=%g r14=%llx x15=%llx\n", (long long)sum, prod, (unsigned long long)r14, (unsigned long long)x15);
	int64_t spread = 0;
	int64_t total = call_many(&spread);
	printf("many=%lld,%lld\n", (long long)total, (long long)spread);
	printf("g=%llx\n", (unsigned long long)call_loadg());
	printf("twice=%lld\n", (long long)call_twice(21));
	int64_t diff = 0;
	double square = 0;
	int64_t sum3 = call_triple(10, 3, 1.5, &diff, &square);
	printf("triple=%lld,%lld,%g\n", (long long)sum3, (long long)diff, square);
	e2e_bump();
	e2e_bump();
	printf("counter=%lld\n", (long long)e2e_counter);
	printf("tpoff=%lld\n", (long long)tlsg_tpoff());
	return 0;
}
`

const amd64SyscallStub = `
	.text
	# int64 call_vfork_entry(int64 trap, int64 a1, uint64 *err)
	.globl call_vfork_entry
call_vfork_entry:
	pushq %rbx
	pushq %r14
	movq %rsi, %rbx
	movq %rdx, %rsi
	movq %rdi, %rax
	xorl %ecx, %ecx
	xorl %edi, %edi
	call syscall_rawVforkSyscall
	popq %r14
	popq %rbx
	ret

	# int64 call_no_error(int64 trap, uint64 *r2): the pointer for the second
	# result is already in RSI, the fifth ABIInternal integer register.
	.globl call_no_error
call_no_error:
	pushq %rbx
	pushq %r14
	movq %rdi, %rax
	xorl %ebx, %ebx
	xorl %ecx, %ecx
	xorl %edi, %edi
	call syscall_rawSyscallNoError
	popq %r14
	popq %rbx
	ret

	.data
	.globl runtime_vdsoGettimeofdaySym
runtime_vdsoGettimeofdaySym:
	.quad 0
	.section .note.GNU-stack,"",@progbits
`

const amd64SyscallDriver = `#include <stdint.h>
#include <stdio.h>
#include <unistd.h>

extern int64_t call_vfork_entry(int64_t, int64_t, uint64_t *);
extern int64_t call_no_error(int64_t, uint64_t *);

int main(void) {
	uint64_t err = 77;
	int64_t pid = call_vfork_entry(39, 0, &err);
	printf("getpid=%d err=%llu\n", pid == (int64_t)getpid(), (unsigned long long)err);
	int64_t closed = call_vfork_entry(3, -1, &err);
	printf("close=%lld err=%llu\n", (long long)closed, (unsigned long long)err);
	uint64_t r2 = 77;
	pid = call_no_error(39, &r2);
	printf("noerror=%d\n", pid == (int64_t)getpid());
	return 0;
}
`

// TestExecuteAMD64SyscallEntries runs the real syscall/asm_linux_amd64.s:
// the hand-built ABIInternal entry of rawVforkSyscall (exercised with getpid
// and a failing close rather than vfork) and the generated two-result entry
// of rawSyscallNoError, whose second result arrives through a pointer.
func TestExecuteAMD64SyscallEntries(t *testing.T) {
	cc := requireAMD64Assembler(t)
	translation := translateAMD64SourceFile(t, amd64SourceFile{packagePath: "syscall", name: "asm_linux_amd64.s"})
	directory := t.TempDir()
	translated := filepath.Join(directory, "translated.S")
	stub := filepath.Join(directory, "stub.S")
	driver := filepath.Join(directory, "driver.c")
	binary := filepath.Join(directory, "syscalls")
	require.NoError(t, os.WriteFile(translated, []byte(translation.Assembly+AMD64TLSDefinition()), 0o644))
	require.NoError(t, os.WriteFile(stub, []byte(amd64SyscallStub), 0o644))
	require.NoError(t, os.WriteFile(driver, []byte(amd64SyscallDriver), 0o644))
	output, err := exec.Command(cc, "-no-pie", "-o", binary, driver, stub, translated).CombinedOutput()
	require.NoError(t, err, "%s", output)
	output, err = exec.Command(binary).CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Equal(t, "getpid=1 err=0\nclose=-1 err=9\nnoerror=1\n", string(output))
}

const amd64Syscall6Stub = `
	.text
	# int64 call_syscall6(int64 num, int64 a1, uint64 *r2, uint64 *errno)
	.globl call_syscall6
call_syscall6:
	pushq %rbx
	pushq %r14
	movq %rdx, %r10
	movq %rcx, %r11
	movq %rdi, %rax
	movq %rsi, %rbx
	xorl %ecx, %ecx
	xorl %edi, %edi
	xorl %esi, %esi
	xorl %r8d, %r8d
	xorl %r9d, %r9d
	call internal_runtime_syscall_linux_Syscall6
	popq %r14
	popq %rbx
	ret
	.section .note.GNU-stack,"",@progbits
`

const amd64Syscall6Driver = `#include <stdint.h>
#include <stdio.h>
#include <unistd.h>

extern int64_t call_syscall6(int64_t, int64_t, uint64_t *, uint64_t *);

int main(void) {
	uint64_t r2 = 77, errno_value = 77;
	int64_t pid = call_syscall6(39, 0, &r2, &errno_value);
	printf("getpid=%d errno=%llu\n", pid == (int64_t)getpid(), (unsigned long long)errno_value);
	int64_t closed = call_syscall6(3, -1, &r2, &errno_value);
	printf("close=%lld r2=%llu errno=%llu\n", (long long)closed, (unsigned long long)r2, (unsigned long long)errno_value);
	return 0;
}
`

// TestExecuteAMD64ABIInternalMultiResultAdapter runs the real
// internal/runtime/syscall/linux.Syscall6, an <ABIInternal> function that
// returns r1, r2, errno in AX, BX, CX, through the adapter that gives it
// cg12's contract: &r2 in R10 and &errno in R11, r1 in RAX.
func TestExecuteAMD64ABIInternalMultiResultAdapter(t *testing.T) {
	cc := requireAMD64Assembler(t)
	file := amd64SourceFile{packagePath: "internal/runtime/syscall/linux", name: "asm_linux_amd64.s"}
	parsed := parseAMD64SourceFile(t, file)
	var params []AsmSlot
	for index, name := range []string{"num", "a1", "a2", "a3", "a4", "a5", "a6"} {
		params = append(params, AsmSlot{Name: name, Offset: 8 * index, Width: 8})
	}
	translation, err := CompileAMD64(parsed, AMD64Options{
		PackagePath: file.packagePath,
		Filename:    file.name,
		Signatures: map[string]AsmSignature{
			"internal_runtime_syscall_linux_Syscall6": {
				Params: params,
				Results: []AsmSlot{
					{Name: "r1", Offset: 56, Width: 8},
					{Name: "r2", Offset: 64, Width: 8},
					{Name: "errno", Offset: 72, Width: 8},
				},
			},
		},
	})
	require.NoError(t, err)
	names := make([]string, 0, len(translation.Functions))
	for _, function := range translation.Functions {
		names = append(names, function.Name)
	}
	require.Equal(t, []string{"internal_runtime_syscall_linux_Syscall6", "internal_runtime_syscall_linux_Syscall6_abiinternal"}, names)
	require.Equal(t, []string{"internal_runtime_syscall_linux_Syscall6_abiinternal"}, translation.Functions[0].Calls)

	directory := t.TempDir()
	translated := filepath.Join(directory, "translated.S")
	stub := filepath.Join(directory, "stub.S")
	driver := filepath.Join(directory, "driver.c")
	binary := filepath.Join(directory, "syscall6")
	require.NoError(t, os.WriteFile(translated, []byte(translation.Assembly), 0o644))
	require.NoError(t, os.WriteFile(stub, []byte(amd64Syscall6Stub), 0o644))
	require.NoError(t, os.WriteFile(driver, []byte(amd64Syscall6Driver), 0o644))
	output, err := exec.Command(cc, "-no-pie", "-o", binary, driver, stub, translated).CombinedOutput()
	require.NoError(t, err, "%s", output)
	output, err = exec.Command(binary).CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, numberLines(translation.Assembly))
	// Syscall6 returns r2 = 0 on failure; errno is EBADF.
	require.Equal(t, "getpid=1 errno=0\nclose=-1 r2=0 errno=9\n", string(output))
}

// TestExecuteAMD64TranslationThroughABIInternalEntries translates a small
// Plan 9 file, links it with glibc via cc -no-pie together with the TLS
// definition and a stand-in for the backend, and runs it natively.
func TestExecuteAMD64TranslationThroughABIInternalEntries(t *testing.T) {
	cc := requireAMD64Assembler(t)
	options := amd64ParseOptions(t, "runtime")
	file, err := ParseWithOptions(strings.NewReader(amd64EndToEndSource), options)
	require.NoError(t, err)
	translation, err := CompileAMD64(file, AMD64Options{
		PackagePath: "e2e",
		Filename:    "e2e_amd64.s",
		Signatures: map[string]AsmSignature{
			"e2e_triple": {
				Params: []AsmSlot{
					{Name: "a", Offset: 0, Width: 8},
					{Name: "b", Offset: 8, Width: 8},
					{Name: "f", Offset: 16, Width: 8, Float: true},
				},
				Results: []AsmSlot{
					{Name: "sum", Offset: 24, Width: 8},
					{Name: "diff", Offset: 32, Width: 8},
					{Name: "square", Offset: 40, Width: 8, Float: true},
				},
			},
			"e2e_compute": {
				Params: []AsmSlot{
					{Name: "a", Offset: 0, Width: 8},
					{Name: "b", Offset: 8, Width: 8},
					{Name: "c", Offset: 16, Width: 4},
					{Name: "f", Offset: 24, Width: 8, Float: true},
				},
				Results: []AsmSlot{
					{Name: "sum", Offset: 32, Width: 8},
					{Name: "prod", Offset: 40, Width: 8, Float: true},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Contains(t, translation.ABI0References, "e2e_scale")

	directory := t.TempDir()
	translated := filepath.Join(directory, "translated.S")
	stub := filepath.Join(directory, "stub.S")
	driver := filepath.Join(directory, "driver.c")
	binary := filepath.Join(directory, "e2e")
	require.NoError(t, os.WriteFile(translated, []byte(translation.Assembly+AMD64TLSDefinition()), 0o644))
	require.NoError(t, os.WriteFile(stub, []byte(amd64EndToEndStub), 0o644))
	require.NoError(t, os.WriteFile(driver, []byte(amd64EndToEndDriver), 0o644))
	output, err := exec.Command(cc, "-no-pie", "-o", binary, driver, stub, translated).CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, numberLines(translation.Assembly))
	output, err = exec.Command(binary).CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, numberLines(translation.Assembly))
	require.Equal(t, strings.Join([]string{
		// c+a+b = -6; scale(3, -6) = -18; negated by the JGE 2(PC) skip.
		"sum=18 prod=3.75 r14=1234abcd x15=0",
		// The second result travels through the pointer on the stack.
		"many=45,8",
		"g=1234abcd",
		"twice=42",
		// Three register results become one register result and two stores.
		"triple=13,7,2.25",
		"counter=2",
		// runtime_tlsg is the executable's only TLS variable, so it sits
		// directly below the thread pointer, where Go expects g: -8(FS).
		"tpoff=-8",
	}, "\n")+"\n", string(output))
}
