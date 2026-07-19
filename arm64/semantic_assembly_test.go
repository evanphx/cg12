package arm64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/plan9asm"
	plan9sem "github.com/evanphx/cg12/plan9asm/sem"
	"github.com/stretchr/testify/require"
)

func TestLowerSemanticAssemblyBindsFrameSlotsToIRValues(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·Load(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	LDARW (R0), R0
	MOVW R0, ret+8(FP)
	RET
`))
	require.NoError(t, err)

	unit, err := plan9sem.Build(file, plan9sem.Options{
		PackagePath: "atomic",
		Filename:    "atomic_arm64.s",
		Signatures: map[string]plan9sem.Signature{
			"atomic_Load": {
				Params:  []plan9sem.Slot{{Name: "ptr", Offset: 0, Width: 8, GCRef: true}},
				Results: []plan9sem.Slot{{Name: "ret", Offset: 8, Width: 4}},
			},
		},
	})
	require.NoError(t, err)

	functions, err := lowerSemanticAssembly(unit, semanticAssemblyPolicy{
		CallConv:     ir.CallConvGoInternal,
		ManagedFrame: true,
	})
	require.NoError(t, err)
	require.Len(t, functions, 1)

	function := functions[0]
	require.Equal(t, ir.CallConvGoInternal, function.CallConv)
	require.True(t, function.ManagedFrame)
	require.True(t, function.NoSplit)
	require.Len(t, function.Params, 1)
	require.True(t, function.Params[0].GCRef)

	assembly := function.Entry().Instrs[0]
	require.Equal(t, ir.OAsm, assembly.Op)
	require.NotContains(t, assembly.Asm.Template, "(FP)")
	require.Contains(t, assembly.Asm.Template, "mov x0, %x1")
	require.Contains(t, assembly.Asm.Template, "mov %w0, w0")
	require.Contains(t, assembly.Asm.Template, "ldar w0, [x0]")

	module := ir.NewModule()
	module.Funcs = functions
	_, err = CompileToObject(module)
	require.NoError(t, err)
}

func TestPrepareAssemblyDoesNotEmitMigratedAtomicSidecars(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "sync/atomic",
		Path:        "sync/atomic/asm.s",
		Source: `
TEXT ·LoadUint32(SB), NOSPLIT, $0
	JMP internal∕runtime∕atomic·Load(SB)
`,
		Signatures: map[string]ir.AsmSignature{
			"sync_atomic_LoadUint32": {
				Params:  []ir.AsmSlot{{Name: "addr", Offset: 0, Width: 8, Cls: ir.ClsP, GCRef: true}},
				Results: []ir.AsmSlot{{Name: "val", Offset: 8, Width: 4, Cls: ir.ClsW}},
			},
		},
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	require.Empty(t, bundle.source)
	require.Len(t, bundle.lowered, 1)
	require.Equal(t, "sync_atomic_LoadUint32", bundle.lowered[0].Name)
	require.True(t, bundle.references["internal_runtime_atomic_Load"])
}

func TestLowerSemanticDataPreservesLayoutAndRelocations(t *testing.T) {
	target := plan9sem.SymRef{Name: "target"}
	data, err := lowerSemanticData(&plan9sem.Data{
		Sym:      plan9sem.SymRef{Name: "table"},
		Size:     24,
		ReadOnly: true,
		Items: []plan9sem.DataItem{
			{Offset: 0, Width: 8, Symbol: &target, Addend: 4},
			{Offset: 12, Width: 4, Value: 0x11223344},
			{Offset: 16, Width: 8, Bytes: []byte("hello")},
		},
	})
	require.NoError(t, err)
	require.Equal(t, ".rodata", data.Linkage.Section)
	require.True(t, data.Linkage.Export)
	require.Equal(t, 8, data.Align)
	require.Equal(t, []int{0}, data.PointerWords)
	require.Equal(t, "target", data.Items[0].Sym)
	require.Equal(t, int64(4), data.Items[0].Off)
	require.Equal(t, 4, data.Items[1].Zero)
	require.Equal(t, []int64{0x11223344}, data.Items[2].Ints)
	require.Equal(t, "hello", data.Items[3].Str)
	require.Equal(t, 3, data.Items[4].Zero)
}

func TestLowerSemanticAssemblyWritesAdditionalResultsThroughPointers(t *testing.T) {
	file, err := plan9asm.Parse(strings.NewReader(`
TEXT ·three(SB), NOSPLIT, $0-16
	MOVW value+0(FP), R0
	MOVW R0, first+4(FP)
	MOVW R0, second+8(FP)
	MOVW R0, third+12(FP)
	RET
`))
	require.NoError(t, err)

	unit, err := plan9sem.Build(file, plan9sem.Options{
		PackagePath: "example",
		Filename:    "three_arm64.s",
		Signatures: map[string]plan9sem.Signature{
			"example_three": {
				Params: []plan9sem.Slot{{Name: "value", Offset: 0, Width: 4}},
				Results: []plan9sem.Slot{
					{Name: "first", Offset: 4, Width: 4},
					{Name: "second", Offset: 8, Width: 4},
					{Name: "third", Offset: 12, Width: 4},
				},
			},
		},
	})
	require.NoError(t, err)

	functions, err := lowerSemanticAssembly(unit, semanticAssemblyPolicy{})
	require.NoError(t, err)
	function := functions[0]
	require.Len(t, function.Params, 3)
	template := function.Entry().Instrs[0].Asm.Template
	require.Contains(t, template, "mov %w0, w0")
	require.Contains(t, template, "str w0, [%x2]")
	require.Contains(t, template, "str w0, [%x3]")

	module := ir.NewModule()
	module.Funcs = functions
	_, err = CompileToObject(module)
	require.NoError(t, err)
}
