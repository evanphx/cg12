package arm64

import (
	"encoding/binary"
	"testing"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The architecture-neutral half of the Go metadata emitter, and the tests that
// pin its byte-level output, live in internal/gometa. What is left here is
// arm64's own PCDATA slot 5 plus the end-to-end checks that go through
// CompileToObject and so exercise arm64's whole parameterization at once.

func TestGoRuntimeModuledataReferencesModuleInitTasks(t *testing.T) {
	module := ir.NewModule()
	module.Runtime = true
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.CallConv = ir.CallConvGoInternal
	schedinit.ManagedFrame = true
	schedinit.Entry().RetVoid()
	module.Data = append(module.Data,
		&ir.Data{Name: ".goc.runtime.datastart", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: goModuleInitTasksName, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}},
		&ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "runtime.firstmoduledata", Align: 8},
	)

	object, err := CompileToObject(module)
	require.NoError(t, err)
	moduledata, ok := gometa.DataSymbol(object, sanitize("runtime.firstmoduledata"))
	require.True(t, ok)
	relocations := make(map[uint64]obj.Reloc)
	for _, relocation := range object.DataRelocs {
		relocations[relocation.Offset] = relocation
	}
	assert.Equal(t, sanitize(goModuleInitTasksName), relocations[moduledata.Value+472].Sym)
	assert.Equal(t, uint64(1), binary.LittleEndian.Uint64(object.Data[moduledata.Value+480:]))
	assert.Equal(t, uint64(1), binary.LittleEndian.Uint64(object.Data[moduledata.Value+488:]))
}

func TestGoRuntimeModuledataReferencesInterfaceTables(t *testing.T) {
	module := ir.NewModule()
	module.Runtime = true
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.CallConv = ir.CallConvGoInternal
	schedinit.ManagedFrame = true
	schedinit.Entry().RetVoid()
	module.Data = append(module.Data,
		&ir.Data{Name: ".goc.runtime.datastart", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: goModuleItabLinksName, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Ints: []int64{0}},
			{Sub: ir.SubL, Ints: []int64{0}},
		}},
		&ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "runtime.firstmoduledata", Align: 8},
	)

	object, err := CompileToObject(module)
	require.NoError(t, err)
	moduledata, ok := gometa.DataSymbol(object, sanitize("runtime.firstmoduledata"))
	require.True(t, ok)
	relocations := make(map[uint64]obj.Reloc)
	for _, relocation := range object.DataRelocs {
		relocations[relocation.Offset] = relocation
	}
	assert.Equal(t, sanitize(goModuleItabLinksName), relocations[moduledata.Value+384].Sym)
	assert.Equal(t, uint64(2), binary.LittleEndian.Uint64(object.Data[moduledata.Value+392:]))
	assert.Equal(t, uint64(2), binary.LittleEndian.Uint64(object.Data[moduledata.Value+400:]))
}

func TestGoRuntimeMetadataIncludesTranslatedAssemblyFunctions(t *testing.T) {
	module := ir.NewModule()
	module.Runtime = true
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.CallConv = ir.CallConvGoInternal
	schedinit.ManagedFrame = true
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

func TestGoAAPCSFramePCDataMarksManagedFrameBody(t *testing.T) {
	assert.Equal(t, []byte{0, 2, 98, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goAAPCSFramePCData(8, 48))
}

func TestGoAAPCSFramePCDataLeavesAssemblyUnmarked(t *testing.T) {
	assert.Equal(t, []byte{0, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goAAPCSFramePCData(4, -1))
}

// The shared emitter reaches goAAPCSFramePCData through the extra-PCDATA hook,
// which is where a function's managed-frame outgoing area turns into the -1 that
// marks a plain Go assembly frame.
func TestGoAAPCSFrameSlotDistinguishesManagedFrames(t *testing.T) {
	managed := gometa.FunctionInfo{FrameStart: 8, ManagedFrame: true, OutgoingSize: 48}
	assert.Equal(t, goAAPCSFramePCData(8, 48), goAAPCSFrameSlotPCData(managed))

	assembly := gometa.FunctionInfo{FrameStart: 4, OutgoingSize: 48}
	assert.Equal(t, goAAPCSFramePCData(4, -1), goAAPCSFrameSlotPCData(assembly))
}

func TestGoRuntimeModuledataDescribesScannedGlobals(t *testing.T) {
	module := ir.NewModule()
	module.Runtime = true
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.CallConv = ir.CallConvGoInternal
	schedinit.ManagedFrame = true
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
	dataStart, ok := gometa.DataSymbolValue(object, sanitize(".goc.runtime.datastart"))
	require.True(t, ok)
	dataEnd, ok := gometa.DataSymbolValue(object, sanitize(".goc.runtime.dataend"))
	require.True(t, ok)
	gcdata, ok := gometa.DataSymbolValue(object, ".goc.go.gcdata")
	require.True(t, ok)
	pointer, ok := gometa.DataSymbolValue(object, sanitize("runtime.pointer"))
	require.True(t, ok)
	pointerSymbol, ok := gometa.DataSymbol(object, sanitize("runtime.pointer"))
	require.True(t, ok)
	assert.Equal(t, obj.SecData, pointerSymbol.Section)

	expectedProgram, err := gometa.GCProgram(dataStart, dataEnd, []uint64{pointer})
	require.NoError(t, err)
	assert.Equal(t, expectedProgram, object.Data[gcdata:gcdata+uint64(len(expectedProgram))])

	moduledata, ok := gometa.DataSymbol(object, sanitize("runtime.firstmoduledata"))
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
