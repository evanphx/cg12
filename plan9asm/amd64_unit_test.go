package plan9asm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compileAMD64Source(t *testing.T, source string, options AMD64Options) AMD64Translation {
	t.Helper()
	parseOptions := amd64ParseOptions(t, "runtime")
	file, err := ParseWithOptions(strings.NewReader(source), parseOptions)
	require.NoError(t, err)
	if options.PackagePath == "" {
		options.PackagePath = "example"
	}
	if options.Filename == "" {
		options.Filename = "example_amd64.s"
	}
	translation, err := CompileAMD64(file, options)
	require.NoError(t, err)
	return translation
}

// amd64FunctionText returns the instruction lines emitted under a label, up
// to the next blank line.
func amd64FunctionText(t *testing.T, assembly, label string) []string {
	t.Helper()
	start := strings.Index(assembly, "\n"+label+":\n")
	require.GreaterOrEqual(t, start, 0, "no label %s in\n%s", label, assembly)
	var lines []string
	for _, line := range strings.Split(assembly[start+len(label)+3:], "\n") {
		if line == "" {
			break
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

func TestParseAMD64Operands(t *testing.T) {
	file, err := ParseWithOptions(strings.NewReader(`
TEXT ·f(SB),4,$0
	MOVQ (SI)(BX*8), CX
	LEAQ 16(SP)(AX*4), DX
	MOVQ (TLS), AX
	MOVQ 0(CX)(TLS*1), AX
	MOVQ $runtime·mainPC+8(SB), AX
	MOVQ x+8(FP), AX
	MOVQ y-16(SP), g
	MOVQ (g_sched+gobuf_sp)(R14), SP
	MOVL $0xf1, 0xf1
	JMP 2(PC)
	JEQ done
	VPERMB.Z Z4, Z6, K1, Z0
done:
	RET
`), ParseOptions{Arch: "amd64"})
	require.NoError(t, err)
	instructions := make([]*Instruction, 0)
	for _, statement := range file.Statements {
		if instruction, ok := statement.(*Instruction); ok {
			instructions = append(instructions, instruction)
		}
	}
	memory := instructions[0].Operands[0]
	assert.Equal(t, OperandMemory, memory.Kind)
	assert.Equal(t, "SI", memory.Base)
	assert.Equal(t, "BX", memory.Index)
	assert.Equal(t, "8", memory.Scale)
	assert.Equal(t, OperandRegister, instructions[0].Operands[1].Kind)

	indexed := instructions[1].Operands[0]
	assert.Equal(t, []string{"SP", "AX", "4", "16"}, []string{indexed.Base, indexed.Index, indexed.Scale, indexed.Offset})
	assert.Equal(t, "TLS", instructions[2].Operands[0].Base)
	assert.Equal(t, []string{"CX", "TLS", "1"}, []string{instructions[3].Operands[0].Base, instructions[3].Operands[0].Index, instructions[3].Operands[0].Scale})

	address := instructions[4].Operands[0]
	assert.Equal(t, OperandAddress, address.Kind)
	assert.Equal(t, "SB", address.Base)
	assert.Equal(t, "runtime·mainPC", address.Symbol.Name)
	assert.Equal(t, "runtime·mainPC+8", address.Offset)

	assert.Equal(t, []string{"FP", "x+8"}, []string{instructions[5].Operands[0].Base, instructions[5].Operands[0].Offset})
	assert.Equal(t, "R14", instructions[6].Operands[1].Register, "g is R14")
	assert.Equal(t, "(g_sched+gobuf_sp)", instructions[7].Operands[0].Offset)
	assert.Equal(t, []string{"", "0xf1"}, []string{instructions[8].Operands[1].Base, instructions[8].Operands[1].Offset})
	assert.Equal(t, "PC", instructions[9].Operands[0].Base)
	assert.Equal(t, OperandLabel, instructions[10].Operands[0].Kind)
	assert.Equal(t, "VPERMB", instructions[11].Opcode)
	assert.Equal(t, "Z", instructions[11].Suffix)
}

func TestParseAMD64RejectsUnrecognizedOperands(t *testing.T) {
	for _, source := range []string{
		"MOVQ [AX], BX",
		"MOVQ (AX)(BX*3), CX",
		"MOVQ 8(FP)(AX*8), CX",
		"MOVQ AX:BX, CX",
	} {
		_, err := ParseWithOptions(strings.NewReader("TEXT ·f(SB),4,$0\n\t"+source+"\n"), ParseOptions{Arch: "amd64"})
		assert.Error(t, err, source)
	}
	// The default grammar is unchanged: it still accepts these untyped.
	_, err := Parse(strings.NewReader("TEXT ·f(SB),4,$0\n\tMOVQ (SI)(BX*8), CX\n"))
	assert.NoError(t, err)
}

func TestParseSelectsAMD64GrammarFromDefines(t *testing.T) {
	_, err := ParseWithOptions(strings.NewReader("TEXT ·f(SB),4,$0\n\tMOVQ [AX], BX\n"), ParseOptions{
		Defines: map[string]string{"GOARCH_amd64": "1"},
	})
	assert.Error(t, err)
}

func TestTranslateAMD64SymbolContract(t *testing.T) {
	translation := compileAMD64Source(t, `#include "textflag.h"
TEXT ·internal<ABIInternal>(SB), NOSPLIT, $0
	CALL	·other(SB)
	CALL	·internal(SB)
	CALL	runtime·memmove(SB)
	CALL	·helper<>(SB)
	JMP	·noargs(SB)

TEXT ·noargs(SB), NOSPLIT, $0
	MOVQ	$·noargs(SB), AX
	MOVQ	$·internal(SB), BX
	MOVQ	$·data(SB), CX
	RET

TEXT ·witharg(SB), NOSPLIT, $0-8
	MOVQ	x+0(FP), AX
	RET

TEXT ·helper<>(SB), NOSPLIT, $0
	RET

GLOBL ·data(SB), RODATA, $8
`, AMD64Options{ABIInternalSymbols: map[string]bool{"runtime_memmove": true}})
	assembly := translation.Assembly

	assert.Equal(t, []string{
		// A function that calls saves BP even with a zero frame (obj6).
		"pushq %rbp",
		"movq %rsp, %rbp",
		"call example_other_abi0",
		"call example_internal",
		"call runtime_memmove",
		"call .Lexample_example_amd64_example_helper",
		"jmp example_noargs_abi0",
	}, amd64FunctionText(t, assembly, "example_internal"))
	assert.NotContains(t, assembly, "example_internal_abi0")

	// No arguments: the plain name labels the ABI0 body.
	assert.Contains(t, assembly, "example_noargs_abi0:\n.global example_noargs\n.type example_noargs, @function\nexample_noargs:\n")
	assert.Equal(t, []string{
		"leaq example_noargs_abi0(%rip), %rax",
		"leaq example_internal(%rip), %rbx",
		"leaq example_data(%rip), %rcx",
		"ret",
	}, amd64FunctionText(t, assembly, "example_noargs"))

	// Arguments: the plain name is the ABIInternal entry.
	assert.Equal(t, []string{
		"pushq %rbp",
		"movq %rsp, %rbp",
		"subq $8, %rsp",
		"movq %rax, (%rsp)",
		"call example_witharg_abi0",
		"addq $8, %rsp",
		"popq %rbp",
		"movq %fs:runtime_tlsg@tpoff, %r14",
		"xorps %xmm15, %xmm15",
		"ret",
	}, amd64FunctionText(t, assembly, "example_witharg"))
	assert.Contains(t, assembly, ".hidden example_witharg_abi0\n")

	assert.Equal(t, []string{"example_noargs", "example_other"}, translation.ABI0References)
	assert.Contains(t, translation.ExternalReferences, "runtime_memmove")
	assert.Equal(t, []string{"example_data"}, translation.GlobalDefinitions)

	names := make([]string, 0, len(translation.Functions))
	for _, function := range translation.Functions {
		names = append(names, function.Name)
	}
	assert.Equal(t, []string{
		"example_internal",
		"example_noargs_abi0",
		"example_witharg",
		"example_witharg_abi0",
		".Lexample_example_amd64_example_helper",
	}, names)
	assert.Equal(t, []string{".Lexample_example_amd64_example_helper", "example_internal", "example_noargs_abi0", "example_other_abi0", "runtime_memmove"}, translation.Functions[0].Calls)
	assert.Equal(t, []string{"example_data", "example_internal", "example_noargs_abi0"}, translation.Functions[1].AddressTaken)
	assert.Equal(t, []string{"example_witharg_abi0"}, translation.Functions[2].Calls)
}

func TestTranslateAMD64FrameFollowsObj6(t *testing.T) {
	translation := compileAMD64Source(t, `#include "textflag.h"
#include "funcdata.h"
// func f(x int64) int64
TEXT ·f(SB), NOSPLIT, $16-16
	NO_LOCAL_POINTERS
	MOVQ	x+0(FP), AX
	MOVQ	AX, local-8(SP)
	MOVQ	AX, 0(SP)
	PUSHQ	AX
	MOVQ	x+0(FP), BX
	MOVQ	local-8(SP), CX
	POPQ	AX
	ADJSP	$8
	MOVQ	x+0(FP), DX
	ADJSP	$-8
	CALL	·g(SB)
	MOVQ	AX, ret+8(FP)
	RET

TEXT ·leaf(SB), NOSPLIT|NOFRAME, $0-8
	MOVQ	x+0(FP), AX
	MOVQ	SP, BX
	ANDQ	$~15, SP
	MOVQ	BX, SP
	RET

TEXT ·entry(SB), NOSPLIT, $-8
	JMP	·start(SB)
`, AMD64Options{})
	assert.Equal(t, []string{
		"pushq %rbp",
		"movq %rsp, %rbp",
		"subq $16, %rsp",
		// x+0(FP): 16 locals + 8 saved BP + 8 return address.
		"movq 32(%rsp), %rax",
		// local-8(SP): the pseudo-SP is the top of the 16-byte local area.
		"movq %rax, 8(%rsp)",
		"movq %rax, (%rsp)",
		"pushq %rax",
		"movq 40(%rsp), %rbx",
		"movq 16(%rsp), %rcx",
		"popq %rax",
		"subq $8, %rsp",
		"movq 40(%rsp), %rdx",
		"addq $8, %rsp",
		"call example_g_abi0",
		"movq %rax, 40(%rsp)",
		"addq $16, %rsp",
		"popq %rbp",
		"ret",
	}, amd64FunctionText(t, translation.Assembly, "example_f_abi0"))
	body := translation.Functions[1]
	assert.Equal(t, "example_f_abi0", body.Name)
	assert.Equal(t, 24, body.Frame)
	assert.Equal(t, 8, body.FrameStart)
	assert.Equal(t, 16, body.Args)
	assert.True(t, body.NoLocalPointers)
	assert.Equal(t, []string{"NOSPLIT"}, body.Flags)

	assert.Equal(t, []string{
		"movq 8(%rsp), %rax",
		"movq %rsp, %rbx",
		"andq $-16, %rsp",
		"movq %rbx, %rsp",
		"ret",
	}, amd64FunctionText(t, translation.Assembly, "example_leaf_abi0"))
	leaf := translation.Functions[3]
	assert.Equal(t, 0, leaf.Frame)
	assert.Equal(t, []string{"NOSPLIT", "NOFRAME", "SPWRITE"}, leaf.Flags)

	assert.Equal(t, []string{"jmp example_start_abi0"}, amd64FunctionText(t, translation.Assembly, "example_entry"))
}

func TestTranslateAMD64OperandOrderAndSpellings(t *testing.T) {
	translation := compileAMD64Source(t, `#include "textflag.h"
TEXT ·f(SB), NOSPLIT, $0
	CMPQ	AX, $0
	CMPB	(SI), DL
	CMPPS	X1, X0, $4
	SHLQ	CX, AX
	SHRQ	$3, DX, BX
	MOVBLZX	(SI), AX
	MOVLQZX	DI, DI
	MOVQ	AX, X0
	MOVL	X1, BX
	PINSRD	$1, AX, X0
	SETEQ	ret+0(FP)
	CMOVQCC	BX, AX
	PSHUFL	$0x1b, X1, X2
	MOVOU	(SI), X3
	ADDQ	$0xffffffff, AX
	MOVQ	$0xffffffff, AX
	MOVQ	$-1, AX
	REP;	MOVSB
	LOCK
	XADDQ	AX, (BX)
	INT	$3
	BYTE	$0x90
	CRC32B	(SI), AX
	VPERMB.Z	Z4, Z6, K1, Z0
	JHI	done
	JCS	done
done:
	RET
`, AMD64Options{})
	assert.Equal(t, []string{
		"cmpq $0, %rax",
		"cmpb %dl, (%rsi)",
		"cmpps $4, %xmm1, %xmm0",
		"shlq %cl, %rax",
		"shrdq $3, %rdx, %rbx",
		"movzbl (%rsi), %eax",
		"movl %edi, %edi",
		"movq %rax, %xmm0",
		"movd %xmm1, %ebx",
		"pinsrd $1, %eax, %xmm0",
		"sete 8(%rsp)",
		"cmovae %rbx, %rax",
		"pshufd $27, %xmm1, %xmm2",
		"movdqu (%rsi), %xmm3",
		"addq $-1, %rax",
		"movl $4294967295, %eax",
		"movq $-1, %rax",
		"rep",
		"movsb",
		"lock",
		"xaddq %rax, (%rbx)",
		".byte 0xcd, 0x03",
		".byte 0x90",
		"crc32b (%rsi), %eax",
		"vpermb %zmm4, %zmm6, %zmm0{%k1}{z}",
		"ja .Lexample_example_amd64_0_done",
		"jb .Lexample_example_amd64_0_done",
		".Lexample_example_amd64_0_done:",
		"ret",
	}, amd64FunctionText(t, translation.Assembly, "example_f_abi0"))
}

func TestTranslateAMD64TLSUsesOneInstructionForm(t *testing.T) {
	translation := compileAMD64Source(t, `#include "textflag.h"
#include "go_tls.h"
TEXT ·setg(SB), NOSPLIT, $0
	get_tls(CX)
	MOVQ	DX, g(CX)
	MOVQ	(TLS), AX
	MOVQ	8(TLS), BX
	RET
`, AMD64Options{})
	assert.Equal(t, []string{
		"movq %rdx, %fs:runtime_tlsg@tpoff",
		"movq %fs:runtime_tlsg@tpoff, %rax",
		"movq %fs:runtime_tlsg@tpoff+8, %rbx",
		"ret",
	}, amd64FunctionText(t, translation.Assembly, "example_setg"))
	assert.Contains(t, translation.ExternalReferences, AMD64TLSSymbol)
	assert.Contains(t, AMD64TLSDefinition(), ".section .tbss")
	assert.Contains(t, AMD64TLSDefinition(), "runtime_tlsg:")
}

func TestTranslateAMD64FloatingPointImmediatesAndData(t *testing.T) {
	translation := compileAMD64Source(t, `#include "textflag.h"
TEXT ·f(SB), NOSPLIT, $0
	MOVSD	$0.0, X1
	MOVSD	$(-1.0), X2
	MULSD	$2.5, X3
	MOVSS	$1.5, X4
	RET

DATA ·table+0(SB)/8, $0.5
DATA ·table+8(SB)/4, $1.0
DATA ·table+12(SB)/4, $7
DATA ·table+16(SB)/8, $·f(SB)
GLOBL ·table(SB), RODATA, $24
`, AMD64Options{})
	assert.Equal(t, []string{
		"xorps %xmm1, %xmm1",
		"movsd .Lexample_example_amd64_f64_bff0000000000000(%rip), %xmm2",
		"mulsd .Lexample_example_amd64_f64_4004000000000000(%rip), %xmm3",
		"movss .Lexample_example_amd64_f32_3fc00000(%rip), %xmm4",
		"ret",
	}, amd64FunctionText(t, translation.Assembly, "example_f"))
	assert.Equal(t, []string{
		".quad 0x3fe0000000000000",
		".long 0x3f800000",
		".long 0x7",
		".quad example_f_abi0",
		".text",
	}, amd64FunctionText(t, translation.Assembly, "example_table"))
	assert.Contains(t, translation.Assembly, "\n.section .rodata\n\t.balign 16\n\t.global example_table\n")
	assert.Contains(t, translation.Assembly, ".section .note.GNU-stack,\"\",@progbits")
}

func TestTranslateAMD64WrapperPassesExtraResultsThroughPointers(t *testing.T) {
	translation := compileAMD64Source(t, `#include "textflag.h"
TEXT ·pair(SB), NOSPLIT, $0-40
	RET
`, AMD64Options{Signatures: map[string]AsmSignature{
		"example_pair": {
			Params: []AsmSlot{
				{Name: "x", Offset: 0, Width: 8},
				{Name: "s_base", Offset: 8, Width: 8, Group: 1},
				{Name: "s_len", Offset: 16, Width: 8, Group: 1},
			},
			Results: []AsmSlot{
				{Name: "r0", Offset: 24, Width: 4},
				{Name: "r1", Offset: 32, Width: 8, Float: true},
			},
		},
	}})
	assert.Equal(t, []string{
		"pushq %rbp",
		"movq %rsp, %rbp",
		"subq $48, %rsp",
		"movq %rax, (%rsp)",
		"movq %rbx, 8(%rsp)",
		"movq %rcx, 16(%rsp)",
		// The pointer to r1 is the next integer argument.
		"movq %rdi, 40(%rsp)",
		"call example_pair_abi0",
		"movl 24(%rsp), %eax",
		"movq 40(%rsp), %r12",
		"movq 32(%rsp), %r13",
		"movq %r13, (%r12)",
		"addq $48, %rsp",
		"popq %rbp",
		"movq %fs:runtime_tlsg@tpoff, %r14",
		"xorps %xmm15, %xmm15",
		"ret",
	}, amd64FunctionText(t, translation.Assembly, "example_pair"))
	wrapper := translation.Functions[0]
	assert.Equal(t, "example_pair", wrapper.Name)
	assert.Equal(t, 56, wrapper.Frame)
	assert.Equal(t, 8, wrapper.FrameStart)
	assert.Equal(t, []string{"example_pair_abi0"}, wrapper.Calls)
}

func TestTranslateAMD64RejectsUnsupportedInstructions(t *testing.T) {
	for _, source := range []string{
		"FADDD F0, F1",
		"VADDPD.BCST (AX), Z1, Z2",
		"PADDL M1, M2",
		"MOVQ $x+0(FP), (AX)",
	} {
		file, err := ParseWithOptions(strings.NewReader("TEXT ·f(SB),4,$0-8\n\t"+source+"\n\tRET\n"), ParseOptions{Arch: "amd64"})
		require.NoError(t, err, source)
		_, err = CompileAMD64(file, AMD64Options{PackagePath: "example", Filename: "x.s"})
		assert.Error(t, err, source)
	}
}
