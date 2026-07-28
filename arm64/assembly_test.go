package arm64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareAssemblyCollectsCodeReferencesAndFunctions(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/example_arm64.s",
		Source: `TEXT ·example<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-8
	MOVBU runtime·flag(SB), R0
	RET
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	assert.Contains(t, bundle.source, ".global runtime_example")
	assert.True(t, bundle.references["runtime_flag"])
	require.Len(t, bundle.functions, 1)
	assert.Equal(t, "runtime_example", bundle.functions[0].Name)
	assert.Equal(t, 8, bundle.functions[0].ArgumentSize)
	assert.Equal(t, byte(goFuncFlagAsm), bundle.functions[0].FuncFlag)
}

func TestPrepareAssemblyCarriesNoLocalPointersMetadata(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/large_arm64.s",
		Source: `TEXT runtime·large<ABIInternal>(SB),WRAPPER,$134217728-48
	FUNCDATA $1, no_pointers_stackmap(SB)
	RET
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	require.Len(t, bundle.functions, 1)
	assert.True(t, bundle.functions[0].NoLocalPointers)
}

func TestAssemblyGlobalReplacesIRDataDefinition(t *testing.T) {
	module := ir.NewModule()
	module.Data = append(module.Data, &ir.Data{
		Name:  "runtime.table",
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}},
	})
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/table.s",
		Source: `DATA ·table+0(SB)/8, $42
GLOBL ·table(SB), RODATA, $8
`,
	})

	assembly, err := TranslateAssembly(module)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(assembly, "runtime_table:\n"))
	assert.Contains(t, assembly, "\t.quad 0x2a\n")

	object, err := CompileToObject(module)
	require.NoError(t, err)
	for _, symbol := range object.Syms {
		assert.NotEqual(t, "runtime_table", symbol.Name)
	}
}

func TestPrepareAssemblyMarksAsyncPreemptForRuntimeStackScanning(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/preempt_arm64.s",
		Source: `TEXT ·asyncPreempt(SB),NOSPLIT|NOFRAME,$0-0
	MOVD R30, -240(RSP)
	SUB $240, RSP
	RET
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	require.Len(t, bundle.functions, 1)
	assert.Equal(t, "runtime_asyncPreempt", bundle.functions[0].Name)
	assert.Equal(t, 240, bundle.functions[0].FrameSize)
	assert.Equal(t, 8, bundle.functions[0].FrameStart)
	assert.Equal(t, byte(3), bundle.functions[0].FuncID)
	assert.Equal(t, byte(goFuncFlagAsm), bundle.functions[0].FuncFlag)
}

func TestPrepareAssemblyMarksMorestackForRuntimeUnwinder(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/asm_arm64.s",
		Source: `TEXT runtime·goexit(SB),NOSPLIT|NOFRAME,$0-0
	RET

TEXT runtime·morestack(SB),NOSPLIT|NOFRAME,$0-0
	RET

TEXT runtime·morestack_noctxt(SB),NOSPLIT|NOFRAME,$0-0
	MOVD RSP, RSP
	B runtime·morestack(SB)
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	require.Len(t, bundle.functions, 3)

	assert.Equal(t, "runtime_goexit", bundle.functions[0].Name)
	assert.Equal(t, byte(8), bundle.functions[0].FuncID)
	assert.Equal(t, byte(goFuncFlagAsm|goFuncFlagTopFrame), bundle.functions[0].FuncFlag)

	assert.Equal(t, "runtime_morestack", bundle.functions[1].Name)
	assert.Equal(t, byte(13), bundle.functions[1].FuncID)
	assert.Equal(t, byte(goFuncFlagAsm), bundle.functions[1].FuncFlag)

	assert.Equal(t, "runtime_morestack_noctxt", bundle.functions[2].Name)
	assert.Equal(t, byte(13), bundle.functions[2].FuncID)
	assert.Equal(t, byte(goFuncFlagAsm|goFuncFlagSPWrite), bundle.functions[2].FuncFlag)
}

func TestPrepareAssemblyBuildsGoABI0EntryWrapper(t *testing.T) {
	module := ir.NewModule()
	args := module.NewFunc("runtime.args", ir.ClsW)
	args.CallConv = ir.CallConvGoInternal
	args.ManagedFrame = true
	args.Param("argc", ir.ClsW)
	args.Param("argv", ir.ClsL)
	args.Entry().Ret(args.Word(0))
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/caller_arm64.s",
		Source: `TEXT ·caller<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-0
	CALL runtime·args(SB)
	RET
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	assert.Contains(t, bundle.source, "\tbl runtime_args_abi0\n")
	assert.Contains(t, bundle.source, "runtime_args_abi0:\n")
	assert.Contains(t, bundle.source, "\tldr w0, [sp, #24]\n")
	assert.Contains(t, bundle.source, "\tldr x1, [sp, #32]\n")
	assert.Contains(t, bundle.source, "\tbl runtime_args\n")
	assert.Contains(t, bundle.source, "\tstr w0, [sp, #40]\n")
	assert.True(t, bundle.abi0References["runtime_args"])
}
