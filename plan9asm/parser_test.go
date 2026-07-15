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
