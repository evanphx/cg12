package plan9asm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTextLabelsInstructionsAndGlobal(t *testing.T) {
	file, err := Parse(strings.NewReader(`
#include "textflag.h"
TEXT runtime·memclrNoHeapPointers<ABIInternal>(SB),NOSPLIT,$0-16
loop:
	STP.P (ZR, ZR), 16(R0)
	TBNZ $31, R5, loop
GLOBL block_size<>(SB), NOPTR, $8
`))
	require.NoError(t, err)
	require.Len(t, file.Statements, 6)

	text, ok := file.Statements[1].(*Text)
	require.True(t, ok)
	assert.Equal(t, "runtime·memclrNoHeapPointers", text.Symbol.Name)
	assert.Equal(t, "ABIInternal", text.Symbol.ABI)
	assert.Equal(t, []string{"NOSPLIT"}, text.Flags)
	assert.Equal(t, "0", text.Frame)
	assert.Equal(t, "16", text.Args)

	store, ok := file.Statements[3].(*Instruction)
	require.True(t, ok)
	assert.Equal(t, "STP", store.Opcode)
	assert.Equal(t, "P", store.Suffix)
	assert.Equal(t, OperandRegisterPair, store.Operands[0].Kind)
	assert.Equal(t, []string{"ZR", "ZR"}, store.Operands[0].Registers)
	assert.Equal(t, OperandMemory, store.Operands[1].Kind)
	assert.Equal(t, "16", store.Operands[1].Offset)
	assert.Equal(t, "R0", store.Operands[1].Base)

	global, ok := file.Statements[5].(*Directive)
	require.True(t, ok)
	assert.Equal(t, "GLOBL", global.Name)
	assert.True(t, global.Operands[0].Symbol.Static)
}

func TestParseRejectsUnbalancedOperands(t *testing.T) {
	_, err := Parse(strings.NewReader("MOVD (R0, R1\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unbalanced delimiters")
}

func TestParseSplitsSemicolonStatements(t *testing.T) {
	file, err := Parse(strings.NewReader("TEXT ·f(SB),$0-0; MOVD R0, R1; RET\n"))
	require.NoError(t, err)
	require.Len(t, file.Statements, 3)
	_, ok := file.Statements[0].(*Text)
	assert.True(t, ok)
	_, ok = file.Statements[1].(*Instruction)
	assert.True(t, ok)
}

func TestParseExpandsMultilineFunctionMacro(t *testing.T) {
	file, err := Parse(strings.NewReader(`#define QR(A, B, C) \
	VADD A.S4, B.S4, A.S4; \
	VEOR C.B16, A.B16, C.B16
TEXT ·block<ABIInternal>(SB), NOSPLIT, $0
	QR(V0, V4, V8)
`))
	require.NoError(t, err)
	require.Len(t, file.Statements, 3)

	add, ok := file.Statements[1].(*Instruction)
	require.True(t, ok)
	assert.Equal(t, "VADD", add.Opcode)
	assert.Equal(t, "V0.S4", add.Operands[0].Text)
	assert.Equal(t, "V4.S4", add.Operands[1].Text)

	xor, ok := file.Statements[2].(*Instruction)
	require.True(t, ok)
	assert.Equal(t, "VEOR", xor.Opcode)
	assert.Equal(t, "V8.B16", xor.Operands[0].Text)
	assert.Equal(t, "V0.B16", xor.Operands[1].Text)
}

func TestParseExpandsNestedFunctionMacros(t *testing.T) {
	file, err := Parse(strings.NewReader(`#define INNER(A, B) VADD A.D2, B.D2, B.D2
#define OUTER(A, B, C) INNER(A, B); VEOR C.B16, B.B16, C.B16
TEXT ·block<ABIInternal>(SB), NOSPLIT, $0
	OUTER(V0, V4, V8)
`))
	require.NoError(t, err)
	require.Len(t, file.Statements, 3)

	add, ok := file.Statements[1].(*Instruction)
	require.True(t, ok)
	assert.Equal(t, "VADD", add.Opcode)
	assert.Equal(t, "V0.D2", add.Operands[0].Text)
	assert.Equal(t, "V4.D2", add.Operands[1].Text)

	xor, ok := file.Statements[2].(*Instruction)
	require.True(t, ok)
	assert.Equal(t, "VEOR", xor.Opcode)
	assert.Equal(t, "V8.B16", xor.Operands[0].Text)
	assert.Equal(t, "V4.B16", xor.Operands[1].Text)
}

func TestParseExpandsIncludedObjectAndFunctionMacros(t *testing.T) {
	file, err := ParseWithOptions(strings.NewReader(`#include "arch.h"
TEXT ·save(SB), NOSPLIT|NOFRAME, $0
	SAVE(8*4)
	READ_TLS
`), ParseOptions{
		Defines: map[string]string{"GOOS_linux": "1"},
		Includes: map[string]string{
			"arch.h": `#define NOSPLIT 4
#define NOFRAME 512
#define OFFSET 8
#define SAVE(base) MOVD R0, ((base)+OFFSET)(RSP)
#ifdef GOOS_linux
#define READ_TLS WORD $0xd53bd040
#endif
`,
		},
	})
	require.NoError(t, err)
	require.Len(t, file.Statements, 3)

	text := file.Statements[0].(*Text)
	assert.Equal(t, []string{"NOSPLIT", "NOFRAME"}, text.Flags)
	store := file.Statements[1].(*Instruction)
	assert.Equal(t, "MOVD", store.Opcode)
	assert.Equal(t, "((8*4)+8)", store.Operands[1].Offset)
	word := file.Statements[2].(*Instruction)
	assert.Equal(t, "WORD", word.Opcode)
	assert.Equal(t, "0xd53bd040", word.Operands[0].Immediate)
}

func TestParseSelectsConditionalSource(t *testing.T) {
	file, err := Parse(strings.NewReader(`#ifndef GOARM64_LSE
MOVD R0, R1
#else
MOVD R2, R3
#endif
RET
`))
	require.NoError(t, err)
	require.Len(t, file.Statements, 2)

	move := file.Statements[0].(*Instruction)
	assert.Equal(t, "R0", move.Operands[0].Register)
	assert.Equal(t, "R1", move.Operands[1].Register)
}

func TestParseSelectsConditionalIntroducedByMacroExpansion(t *testing.T) {
	file, err := ParseWithOptions(strings.NewReader(`#define BREAK \
#ifdef GOOS_windows \
	BRK $0xf000 \
#else \
	BRK \
#endif
BREAK
`), ParseOptions{Defines: map[string]string{"GOOS_linux": "1"}})
	require.NoError(t, err)
	require.Len(t, file.Statements, 1)

	breakInstruction, ok := file.Statements[0].(*Instruction)
	require.True(t, ok)
	assert.Equal(t, "BRK", breakInstruction.Opcode)
	assert.Empty(t, breakInstruction.Operands)
}

func TestParseIndexedMemoryOperand(t *testing.T) {
	file, err := Parse(strings.NewReader("MOVD (R0)(R6), R4\n"))
	require.NoError(t, err)
	require.Len(t, file.Statements, 1)

	instruction, ok := file.Statements[0].(*Instruction)
	require.True(t, ok)
	require.Len(t, instruction.Operands, 2)
	assert.Equal(t, OperandMemory, instruction.Operands[0].Kind)
	assert.Equal(t, "R0", instruction.Operands[0].Base)
	assert.Equal(t, "R6", instruction.Operands[0].Index)
}

func TestParseVectorRegistersAndLists(t *testing.T) {
	file, err := Parse(strings.NewReader("VLD1.P (R0), [V0.D2, V1.D2]\nVMOV V8.D[1], R5\n"))
	require.NoError(t, err)
	require.Len(t, file.Statements, 2)

	load := file.Statements[0].(*Instruction)
	assert.Equal(t, OperandVectorList, load.Operands[1].Kind)
	assert.Equal(t, []VectorRegister{
		{Register: "V0", Arrangement: "D2"},
		{Register: "V1", Arrangement: "D2"},
	}, load.Operands[1].Vectors)

	move := file.Statements[1].(*Instruction)
	assert.Equal(t, OperandVectorRegister, move.Operands[0].Kind)
	assert.Equal(t, VectorRegister{Register: "V8", Arrangement: "D", Index: "1"}, move.Operands[0].Vector)
}

func TestParseExtendedRegister(t *testing.T) {
	file, err := Parse(strings.NewReader("CMP R2.UXTB, R5\nADD R11.UXTW<<3, R7, R8\n"))
	require.NoError(t, err)
	require.Len(t, file.Statements, 2)

	compare := file.Statements[0].(*Instruction)
	assert.Equal(t, OperandExtendedRegister, compare.Operands[0].Kind)
	assert.Equal(t, "R2", compare.Operands[0].Register)
	assert.Equal(t, "UXTB", compare.Operands[0].Extend)

	add := file.Statements[1].(*Instruction)
	assert.Equal(t, OperandExtendedRegister, add.Operands[0].Kind)
	assert.Equal(t, "UXTW", add.Operands[0].Extend)
	assert.Equal(t, "3", add.Operands[0].ShiftAmount)
}
