package sem

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/plan9asm"
	"github.com/stretchr/testify/require"
)

func TestBuildDissolvesFrameAndSymbolPseudoRegisters(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·sum(SB), NOSPLIT, $16-24
	MOVD left+0(FP), R0
	MOVD right+8(FP), R1
	ADD R1, R0, R0
	MOVD R0, result+16(FP)
	MOVD $table(SB), R2
	MOVD R2, 8(SP)
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{PackagePath: "example", Filename: "sum_arm64.s"})
	require.NoError(t, err)
	require.Len(t, unit.Funcs, 1)

	function := unit.Funcs[0]
	require.Equal(t, "example_sum", function.Sym.Name)
	require.Equal(t, Bindable, function.Kind)
	require.Equal(t, 16, function.FrameSize)
	require.Equal(t, []Slot{
		{Name: "left", Offset: 0, Width: 8},
		{Name: "right", Offset: 8, Width: 8},
	}, function.Signature.Params)
	require.Equal(t, []Slot{{Name: "result", Offset: 16, Width: 8}}, function.Signature.Results)

	first := function.Body[0].(Inst)
	require.Equal(t, ArgumentOperand, first.Operands[0].Kind)
	result := function.Body[3].(Inst)
	require.Equal(t, ResultOperand, result.Operands[1].Kind)
	symbol := function.Body[4].(Inst)
	require.Equal(t, SymbolAddressOperand, symbol.Operands[0].Kind)
	require.Equal(t, "table", symbol.Operands[0].Symbol.Name)
	local := function.Body[5].(Inst)
	require.Equal(t, LocalOperand, local.Operands[1].Kind)
	require.Equal(t, int64(8), local.Operands[1].Offset)
	require.Equal(t, []SymRef{{Name: "table"}}, unit.References)
}

func TestBuildClassifiesMachineStateFunctionsRaw(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT runtime·gogo(SB), NOSPLIT|NOFRAME, $0-8
	MOVD buf+0(FP), R5
	MOVD gobuf_sp(R5), RSP
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{
		PackagePath: "runtime",
		Filename:    "asm_arm64.s",
		Defines:     map[string]int64{"gobuf_sp": 0},
	})
	require.NoError(t, err)
	require.Len(t, unit.Funcs, 1)
	require.Equal(t, Raw, unit.Funcs[0].Kind)
	require.NotEmpty(t, unit.Funcs[0].Classification)
}

func TestBuildRequiresSignatureForABIInternalRegisterIntent(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·block<ABIInternal>(SB), NOSPLIT, $0
	ADD R1, R0, R0
	RET
`))
	require.NoError(t, err)

	withoutSignature, err := Build(file, Options{PackagePath: "internal/chacha8rand", Filename: "chacha8_arm64.s"})
	require.NoError(t, err)
	require.Equal(t, Raw, withoutSignature.Funcs[0].Kind)

	name := "internal_chacha8rand_block"
	withSignature, err := Build(file, Options{
		PackagePath: "internal/chacha8rand",
		Filename:    "chacha8_arm64.s",
		Signatures: map[string]Signature{
			name: {Params: []Slot{{Name: "seed", Width: 8}}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, Bindable, withSignature.Funcs[0].Kind)
}

func TestBuildBindsDeclaredResultAddresses(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·pipe2(SB), NOSPLIT, $0-20
	MOVD $r+8(FP), R0
	MOVW flags+0(FP), R1
	MOVW R1, errno+16(FP)
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{
		PackagePath: "runtime",
		Filename:    "sys_linux_arm64.s",
		Signatures: map[string]Signature{
			"runtime_pipe2": {
				Params: []Slot{{Name: "flags", Offset: 0, Width: 4}},
				Results: []Slot{
					{Name: "r", Offset: 8, Width: 4},
					{Name: "w", Offset: 12, Width: 4},
					{Name: "errno", Offset: 16, Width: 4},
				},
			},
		},
	})
	require.NoError(t, err)

	function := unit.Funcs[0]
	address := function.Body[0].(Inst).Operands[0]
	require.Equal(t, ResultAddressOperand, address.Kind)
	require.Equal(t, 0, address.Slot)
}

func TestBuildAllowsWidenedIntegerResultRegister(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·or32(SB), NOSPLIT, $0-20
	MOVD R2, ret+16(FP)
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{
		PackagePath: "atomic",
		Filename:    "atomic_arm64.s",
		Signatures: map[string]Signature{
			"atomic_or32": {
				Results: []Slot{{Name: "ret", Offset: 16, Width: 4}},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, ResultOperand, unit.Funcs[0].Body[0].(Inst).Operands[1].Kind)
	require.Equal(t, 4, unit.Funcs[0].Signature.Results[0].Width)
}

func TestBuildPreservesRawCallerFrameAddresses(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT runtime·walltime(SB), NOSPLIT, $24-12
	MOVD $ret-8(FP), R2
	MOVD R2, RSP
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{
		PackagePath: "runtime",
		Filename:    "sys_linux_arm64.s",
		Signatures: map[string]Signature{
			"runtime_walltime": {
				Results: []Slot{{Name: "sec", Offset: 0, Width: 8}},
			},
		},
	})
	require.NoError(t, err)

	function := unit.Funcs[0]
	require.Equal(t, Raw, function.Kind)
	address := function.Body[0].(Inst).Operands[0]
	require.Equal(t, CallerFrameAddressOperand, address.Kind)
	require.Equal(t, int64(-8), address.Offset)
}

func TestBuildMergesGlobalData(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
DATA table<>+0(SB)/8, $target+4(SB)
DATA table<>+8(SB)/4, $0x11223344
GLOBL table<>(SB), RODATA|NOPTR, $16
DATA ·message+0(SB)/8, $"hello"
GLOBL ·message(SB), DUPOK, $8
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{PackagePath: "example", Filename: "tables_arm64.s"})
	require.NoError(t, err)
	require.Len(t, unit.Data, 2)

	table := unit.Data[0]
	require.Equal(t, ".Lexample_tables_arm64_table", table.Sym.Name)
	require.True(t, table.Sym.Static)
	require.True(t, table.ReadOnly)
	require.True(t, table.NoPointers)
	require.Len(t, table.Items, 2)
	require.NotNil(t, table.Items[0].Symbol)
	require.Equal(t, "target", table.Items[0].Symbol.Name)
	require.Equal(t, int64(4), table.Items[0].Addend)
	require.Equal(t, int64(0x11223344), table.Items[1].Value)
	require.Equal(t, []SymRef{{Name: "target"}}, unit.References)

	message := unit.Data[1]
	require.Equal(t, "example_message", message.Sym.Name)
	require.True(t, message.Duplicate)
	require.Equal(t, []byte("hello"), message.Items[0].Bytes)
}

func TestBuildResolvesSymbolMemoryAddends(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·hasAtomics(SB), NOSPLIT, $0-1
	MOVBU internal∕cpu·ARM64+const_offsetARM64HasATOMICS(SB), R0
	MOVB R0, ret+0(FP)
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{
		PackagePath: "example",
		Filename:    "atomic_arm64.s",
		Defines: map[string]int64{
			"const_offsetARM64HasATOMICS": 135,
		},
	})
	require.NoError(t, err)

	operand := unit.Funcs[0].Body[0].(Inst).Operands[0]
	require.Equal(t, SymbolMemoryOperand, operand.Kind)
	require.Equal(t, "internal_cpu_ARM64", operand.Symbol.Name)
	require.Equal(t, int64(135), operand.Offset)
	require.Equal(t, []SymRef{{Name: "internal_cpu_ARM64"}}, unit.References)
}

func TestBuildClassifiesSymbolBranchesAsFunctions(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·alias(SB), NOSPLIT, $0-8
	B ·target(SB)
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{PackagePath: "example", Filename: "alias_arm64.s"})
	require.NoError(t, err)

	operand := unit.Funcs[0].Body[0].(Inst).Operands[0]
	require.Equal(t, FunctionOperand, operand.Kind)
	require.Equal(t, "example_target", operand.Symbol.Name)
}

func TestBuildResolvesFloatingPointImmediate(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·expLimit(SB), NOSPLIT, $0
	FMOVD $7.09782712893383973096e+02, F0
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{PackagePath: "math", Filename: "exp_arm64.s"})
	require.NoError(t, err)

	operand := unit.Funcs[0].Body[0].(Inst).Operands[0]
	require.Equal(t, FloatImmediateOperand, operand.Kind)
	require.InDelta(t, 709.782712893384, operand.Float, 1e-12)
}

func TestBuildPropagatesTailAliasSignatures(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·LoadAlias2(SB), NOSPLIT, $0
	B ·LoadAlias(SB)
TEXT ·LoadAlias(SB), NOSPLIT, $0
	B ·Load(SB)
TEXT ·Load(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	LDARW (R0), R0
	MOVW R0, ret+8(FP)
	RET
`))
	require.NoError(t, err)

	unit, err := Build(file, Options{PackagePath: "atomic", Filename: "atomic_arm64.s"})
	require.NoError(t, err)

	want := unit.Funcs[2].Signature
	require.Equal(t, want, unit.Funcs[0].Signature)
	require.Equal(t, want, unit.Funcs[1].Signature)
}
