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

func TestGoFunctionMetadataFollowsEmittedTextOffsets(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "later", Section: obj.SecText, Value: 64, Func: true},
		{Name: "earlier", Section: obj.SecText, Value: 16, Func: true},
	}}
	functions := []goFunctionInfo{{name: "later"}, {name: "earlier"}}

	sorted := sortGoFunctionsByTextOffset(object, functions)
	require.Len(t, sorted, 2)
	assert.Equal(t, "earlier", sorted[0].name)
	assert.Equal(t, "later", sorted[1].name)
}

func TestGoFunctionMetadataSortsTranslatedAssemblyWithGeneratedFunctions(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "generated_later", Section: obj.SecText, Value: 64, Func: true},
		{Name: "translated_earlier", Section: obj.SecText, Value: 16, Func: true},
		{Name: "runtime_gocTextEnd", Section: obj.SecText, Value: 80, Func: true},
	}}
	builder := &goMetadataBuilder{
		object: object,
		labels: make(map[string]uint64),
	}

	builder.build(
		[]goFunctionInfo{{name: "generated_later", size: 4}},
		[]goFunctionInfo{{name: "translated_earlier", size: 4}},
		&ir.Data{Name: "runtime.firstmoduledata"},
		[]byte{0},
		".goc.runtime.dataend",
		0,
		0,
		0,
	)

	functab := builder.labels[".goc.go.functab"]
	relocations := make(map[uint64]string)
	for _, relocation := range builder.relocs {
		relocations[relocation.Offset] = relocation.Sym
	}
	assert.Equal(t, "translated_earlier", relocations[functab])
	assert.Equal(t, "generated_later", relocations[functab+8])
	assert.Equal(t, "runtime_gocTextEnd", relocations[functab+16])
}

func TestGoFindFuncBucketCountCoversLargeTextRanges(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "first", Section: obj.SecText, Value: 0x400580, Func: true},
		{Name: "runtime_gocTextEnd", Section: obj.SecText, Value: 0x14aa5a8, Func: true},
	}}
	functions := []goFunctionInfo{{name: "first"}}

	buckets := goFindFuncBucketCount(object, functions, "runtime_gocTextEnd")
	assert.Greater(t, buckets, 4096)
}

func TestGoFindFuncBucketCountUsesConservativeFallbackWhenTextEndIsExternal(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "first", Section: obj.SecText, Value: 0x400580, Func: true},
	}}
	functions := []goFunctionInfo{{name: "first"}}

	buckets := goFindFuncBucketCount(object, functions, "runtime_gocTextEnd")
	assert.Equal(t, minimumGoFindFuncBucketCap, buckets)
}

func TestGoFunctionMetadataPreservesRuntimeFunctionID(t *testing.T) {
	builder := &goMetadataBuilder{
		object: &obj.Object{},
		labels: make(map[string]uint64),
	}
	functions := []goFunctionInfo{
		{
			name:   "runtime_gopanic",
			funcID: 10,
		},
	}

	builder.build(functions, nil, &ir.Data{Name: "runtime.firstmoduledata"}, []byte{0}, ".goc.runtime.dataend", 0, 0, 0)
	pclntableOffset := builder.labels[".goc.go.pclntable"] - builder.base
	const funcIDOffset = 40

	assert.Equal(t, byte(10), builder.data[pclntableOffset+funcIDOffset])
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

func TestGoRuntimeModuledataReferencesInterfaceTables(t *testing.T) {
	module := ir.NewModule()
	schedinit := module.NewFuncVoid("runtime.schedinit")
	schedinit.GoABI = true
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
	moduledata, ok := dataSymbol(object, sanitize("runtime.firstmoduledata"))
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

func TestGoAAPCSFramePCDataMarksManagedFrameBody(t *testing.T) {
	assert.Equal(t, []byte{0, 2, 98, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goAAPCSFramePCData(8, 48))
}

func TestGoAAPCSFramePCDataLeavesAssemblyUnmarked(t *testing.T) {
	assert.Equal(t, []byte{0, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goAAPCSFramePCData(4, -1))
}

func TestGoLocalStackMapExcludesAAPCSOutgoingArea(t *testing.T) {
	function := goFunctionInfo{
		frameSize:    96,
		managedAAPCS: true,
		outgoingSize: 32,
	}

	assert.Equal(t, 6, goLocalStackMapWords(function))
}

func TestGoUnsafePointPCDataDisablesAsyncPreemption(t *testing.T) {
	assert.Equal(t, []byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goUnsafePointPCData())
}

func TestGoStackMapPCDataSwitchesAfterPrologue(t *testing.T) {
	assert.Equal(t, []byte{4, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goStackMapPCData(0, nil))
	assert.Equal(t, []byte{2, 9, 2, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, goStackMapPCData(36, nil))
	assert.Equal(t, []byte{
		2, 9,
		2, 11,
		2, 10,
		1, 0xff, 0xff, 0xff, 0xff, 0x0f,
		0,
	}, goStackMapPCData(36, []goStackMapIndexPoint{
		{pc: 80, index: 2},
		{pc: 120, index: 1},
	}))
}

func TestGoStackMapPCDataCoalescesDuplicateFrameStartPoint(t *testing.T) {
	assert.Equal(t,
		[]byte{2, 0xff, 0xff, 0xff, 0xff, 0x0f, 0},
		goStackMapPCData(52, []goStackMapIndexPoint{{pc: 52, index: 0}}),
	)
}

func TestGoFunctionStackMapsKeepSafepointRootsPrecise(t *testing.T) {
	pointerMaps, indexPoints := goFunctionStackMaps(goFunctionInfo{
		localPointerWords: []int{1},
		stackMapPoints: []goStackMapPoint{
			{pc: 80, pointerWords: []int{4}},
			{pc: 120, pointerWords: []int{1}},
		},
	})

	assert.Equal(t, [][]int{nil, {1}, {4}}, pointerMaps)
	assert.Equal(t, []goStackMapIndexPoint{
		{pc: 80, index: 2},
		{pc: 120, index: 1},
	}, indexPoints)
}

func TestGoStackMapsSeparateEntryAndBodyRoots(t *testing.T) {
	builder := &goMetadataBuilder{}
	builder.stackMaps(8, []int{1}, []int{6})
	assert.Equal(t, []byte{
		2, 0, 0, 0,
		8, 0, 0, 0,
		0b00000010,
		0b01000000,
	}, builder.data)
}

func TestNoLocalPointersAssemblyUsesZeroBitStackMap(t *testing.T) {
	function := goFunctionInfo{
		frameSize:       134217744,
		noLocalPointers: true,
	}
	assert.Equal(t, 0, goLocalStackMapWords(function))

	builder := &goMetadataBuilder{}
	pointerMaps, _ := goFunctionStackMaps(function)
	builder.stackMaps(goLocalStackMapWords(function), pointerMaps...)
	assert.Equal(t, []byte{
		2, 0, 0, 0,
		0, 0, 0, 0,
	}, builder.data)
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
