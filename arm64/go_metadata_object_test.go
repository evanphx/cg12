package arm64

import (
	"encoding/binary"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoGCProgramEncodesExactPointerWords(t *testing.T) {
	program, err := goGCProgram(8, 32, []uint64{8, 24})
	require.NoError(t, err)
	assert.Equal(t, []byte{3, 0b00000101, 0}, program)
}

func TestGoRuntimeModuledataReferencesModuleInitTasks(t *testing.T) {
	module := ir.NewModule()
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.GoABI = true
	schedinit.Entry().RetVoid()
	module.Data = append(module.Data,
		&ir.Data{Name: ".goc.runtime.datastart", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: goModuleInitTasksName, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}},
		&ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "runtime.firstmoduledata", Align: 8},
	)

	object, err := CompileToObject(module)
	require.NoError(t, err)
	moduledata, ok := dataSymbol(object, sanitize("runtime.firstmoduledata"))
	require.True(t, ok)
	relocations := make(map[uint64]obj.Reloc)
	for _, relocation := range object.DataRelocs {
		relocations[relocation.Offset] = relocation
	}
	assert.Equal(t, sanitize(goModuleInitTasksName), relocations[moduledata.Value+472].Sym)
	assert.Equal(t, uint64(1), binary.LittleEndian.Uint64(object.Data[moduledata.Value+480:]))
	assert.Equal(t, uint64(1), binary.LittleEndian.Uint64(object.Data[moduledata.Value+488:]))
}

func TestGoRuntimeMetadataIncludesTranslatedAssemblyFunctions(t *testing.T) {
	module := ir.NewModule()
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.GoABI = true
	schedinit.Entry().RetVoid()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/example_arm64.s",
		Source:      "TEXT ·translated<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-0\n\tRET\n",
	})
	module.Data = append(module.Data,
		&ir.Data{Name: ".goc.runtime.datastart", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "runtime.firstmoduledata", Align: 8},
	)

	object, err := CompileToObject(module)
	require.NoError(t, err)
	found := false
	for _, relocation := range object.DataRelocs {
		if relocation.Sym == "runtime_translated" {
			found = true
			break
		}
	}
	assert.True(t, found, "translated TEXT declaration is absent from runtime functab metadata")
}

func TestGoPCSPTerminatesBeforeFollowingFunction(t *testing.T) {
	assert.Equal(t, []byte{66, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goPCSP(0, 32))
	assert.Equal(t, []byte{2, 9, 64, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goPCSP(36, 32))
}

func TestGoRuntimeModuledataDescribesScannedGlobals(t *testing.T) {
	module := ir.NewModule()
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.GoABI = true
	schedinit.Entry().RetVoid()
	module.Data = append(module.Data,
		&ir.Data{Name: ".goc.runtime.datastart", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "runtime.pointerFree", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{17}}}},
		&ir.Data{Name: "runtime.pointer", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}, PointerWords: []int{0}},
		&ir.Data{Name: "runtime.methodValueCallFrameObjs", Align: 4, Items: []ir.DataItem{{Zero: 16}}},
		&ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "runtime.firstmoduledata", Align: 8},
	)

	object, err := CompileToObject(module)
	require.NoError(t, err)
	dataStart, ok := dataSymbolValue(object, sanitize(".goc.runtime.datastart"))
	require.True(t, ok)
	dataEnd, ok := dataSymbolValue(object, sanitize(".goc.runtime.dataend"))
	require.True(t, ok)
	gcdata, ok := dataSymbolValue(object, ".goc.go.gcdata")
	require.True(t, ok)
	pointer, ok := dataSymbolValue(object, sanitize("runtime.pointer"))
	require.True(t, ok)
	pointerSymbol, ok := dataSymbol(object, sanitize("runtime.pointer"))
	require.True(t, ok)
	assert.Equal(t, obj.SecData, pointerSymbol.Section)

	expectedProgram, err := goGCProgram(dataStart, dataEnd, []uint64{pointer})
	require.NoError(t, err)
	assert.Equal(t, expectedProgram, object.Data[gcdata:gcdata+uint64(len(expectedProgram))])

	moduledata, ok := dataSymbol(object, sanitize("runtime.firstmoduledata"))
	require.True(t, ok)
	relocations := make(map[uint64]obj.Reloc)
	for _, relocation := range object.DataRelocs {
		relocations[relocation.Offset] = relocation
	}
	assert.Equal(t, sanitize(".goc.runtime.datastart"), relocations[moduledata.Value+208].Sym)
	assert.Equal(t, sanitize(".goc.runtime.dataend"), relocations[moduledata.Value+216].Sym)
	assert.Equal(t, ".goc.go.gcdata", relocations[moduledata.Value+280].Sym)
	assert.Equal(t, ".goc.go.gcbss", relocations[moduledata.Value+288].Sym)
	assert.Equal(t, sanitize("runtime.methodValueCallFrameObjs"), relocations[moduledata.Value+240].Sym)
	assert.Equal(t, int64(16), relocations[moduledata.Value+248].Addend)
	assert.Greater(t, moduledata.Value, dataEnd)
}
