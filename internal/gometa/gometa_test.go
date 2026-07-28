package gometa

import (
	"encoding/binary"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testArch is arm64's parameter set -- this package's first consumer -- so the
// byte-level expectations below are the ones these tables have always produced.
var testArch = Arch{
	PCQuantum:      4,
	Reloc32:        obj.R_AARCH64_ABS32,
	Reloc64:        obj.R_AARCH64_ABS64,
	FrameBaseBytes: 16,
}

func TestGoGCProgramEncodesExactPointerWords(t *testing.T) {
	program, err := GCProgram(8, 32, []uint64{8, 24})
	require.NoError(t, err)
	assert.Equal(t, []byte{3, 0b00000101, 0}, program)
}

func TestGoFunctionMetadataFollowsEmittedTextOffsets(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "later", Section: obj.SecText, Value: 64, Func: true},
		{Name: "earlier", Section: obj.SecText, Value: 16, Func: true},
	}}
	functions := []FunctionInfo{{Name: "later"}, {Name: "earlier"}}

	sorted := sortFunctionsByTextOffset(object, functions)
	require.Len(t, sorted, 2)
	assert.Equal(t, "earlier", sorted[0].Name)
	assert.Equal(t, "later", sorted[1].Name)
}

func TestGoFunctionMetadataSortsTranslatedAssemblyWithGeneratedFunctions(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "generated_later", Section: obj.SecText, Value: 64, Func: true},
		{Name: "translated_earlier", Section: obj.SecText, Value: 16, Func: true},
		{Name: "runtime_gocTextEnd", Section: obj.SecText, Value: 80, Func: true},
	}}
	builder := NewBuilder(testArch, Options{}, object, nil)

	builder.Build(
		[]FunctionInfo{{Name: "generated_later", Size: 4}},
		[]FunctionInfo{{Name: "translated_earlier", Size: 4}},
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
	functions := []FunctionInfo{{Name: "first"}}

	buckets := findFuncBucketCount(object, functions, "runtime_gocTextEnd")
	assert.Greater(t, buckets, 4096)
}

func TestGoFindFuncBucketCountUsesConservativeFallbackWhenTextEndIsExternal(t *testing.T) {
	object := &obj.Object{Syms: []obj.Sym{
		{Name: "first", Section: obj.SecText, Value: 0x400580, Func: true},
	}}
	functions := []FunctionInfo{{Name: "first"}}

	buckets := findFuncBucketCount(object, functions, "runtime_gocTextEnd")
	assert.Equal(t, minimumFindFuncBucketCap, buckets)
}

func TestGoFunctionMetadataPreservesRuntimeFunctionID(t *testing.T) {
	builder := NewBuilder(testArch, Options{}, &obj.Object{}, nil)
	functions := []FunctionInfo{
		{
			Name:   "runtime_gopanic",
			FuncID: 10,
		},
	}

	builder.Build(functions, nil, &ir.Data{Name: "runtime.firstmoduledata"}, []byte{0}, ".goc.runtime.dataend", 0, 0, 0)
	pclntableOffset := builder.labels[".goc.go.pclntable"] - builder.base
	const funcIDOffset = 40

	assert.Equal(t, byte(10), builder.data[pclntableOffset+funcIDOffset])
}

// A target-private PCDATA table lands after the Go-defined slots and shows up in
// the _func record's npcdata; a target without one emits only the Go slots. This
// is the seam an amd64 backend uses to opt out of arm64's frame table.
func TestExtraPCDataAddsOneSlotToFunctionRecords(t *testing.T) {
	const npcdataOffset = 28

	build := func(options Options) *Builder {
		object := &obj.Object{Syms: []obj.Sym{
			{Name: "only", Section: obj.SecText, Value: 0, Func: true},
		}}
		builder := NewBuilder(testArch, options, object, nil)
		builder.Build([]FunctionInfo{{Name: "only", Size: 4}}, nil,
			&ir.Data{Name: "runtime.firstmoduledata"}, []byte{0}, ".goc.runtime.dataend", 0, 0, 0)
		return builder
	}

	neutral := build(Options{})
	pclntable := neutral.labels[".goc.go.pclntable"] - neutral.base
	assert.Equal(t, uint32(GoPCDataSlots), binary.LittleEndian.Uint32(neutral.data[pclntable+npcdataOffset:]))

	marker := []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}
	extended := build(Options{ExtraPCData: func(FunctionInfo) []byte { return marker }})
	pclntable = extended.labels[".goc.go.pclntable"] - extended.base
	assert.Equal(t, uint32(GoPCDataSlots+1), binary.LittleEndian.Uint32(extended.data[pclntable+npcdataOffset:]))

	// The extra table is the last thing written into pctab, and the _func record
	// names it in the slot after the Go-defined ones.
	pctab := extended.labels[".goc.go.pctab"]
	extraOffset := binary.LittleEndian.Uint32(extended.data[pclntable+44+4*GoPCDataSlots:])
	assert.Equal(t, marker, extended.data[pctab+uint64(extraOffset):pctab+uint64(extraOffset)+uint64(len(marker))])
}

func TestGoPCSPTerminatesBeforeFollowingFunction(t *testing.T) {
	assert.Equal(t, []byte{66, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, testArch.PCSP(0, 32))
	assert.Equal(t, []byte{2, 9, 64, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, testArch.PCSP(36, 32))
}

// A single-instruction PC quantum shortens every delta but leaves the value
// sequence alone, which is the whole of what an x86-64 backend changes here.
func TestGoPCSPCountsDeltasInPCQuantumUnits(t *testing.T) {
	byteQuantum := Arch{PCQuantum: 1, FrameBaseBytes: 16}
	assert.Equal(t, []byte{2, 36, 64, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, byteQuantum.PCSP(36, 32))
}

func TestGoLocalStackMapExcludesManagedOutgoingArea(t *testing.T) {
	function := FunctionInfo{
		FrameSize:    96,
		ManagedFrame: true,
		OutgoingSize: 32,
	}

	assert.Equal(t, 6, testArch.LocalStackMapWords(function))
}

func TestGoUnsafePointPCDataDisablesAsyncPreemption(t *testing.T) {
	assert.Equal(t, []byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, UnsafePointPCData())
}

func TestGoStackMapPCDataSwitchesAfterPrologue(t *testing.T) {
	assert.Equal(t, []byte{4, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, testArch.StackMapPCData(0, nil))
	assert.Equal(t, []byte{2, 9, 2, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}, testArch.StackMapPCData(36, nil))
	assert.Equal(t, []byte{
		2, 9,
		2, 11,
		2, 10,
		1, 0xff, 0xff, 0xff, 0xff, 0x0f,
		0,
	}, testArch.StackMapPCData(36, []StackMapIndexPoint{
		{PC: 80, Index: 2},
		{PC: 120, Index: 1},
	}))
}

func TestGoStackMapPCDataCoalescesDuplicateFrameStartPoint(t *testing.T) {
	assert.Equal(t,
		[]byte{2, 0xff, 0xff, 0xff, 0xff, 0x0f, 0},
		testArch.StackMapPCData(52, []StackMapIndexPoint{{PC: 52, Index: 0}}),
	)
}

// A safepoint reports the roots live at that safepoint and nothing else. The
// conservative body map stays available as map 1 for the prologue window, but
// unioning it into a safepoint would report every pointer the frame ever held as
// live for as long as the frame exists -- the over-retention bug of
// RUNTIME_PLAN.md 5.3.
func TestGoFunctionStackMapsUseOnlyTheRootsLiveAtEachSafepoint(t *testing.T) {
	pointerMaps, indexPoints := FunctionStackMaps(FunctionInfo{
		LocalPointerWords: []int{1},
		StackMapPoints: []StackMapPoint{
			{PC: 80, PointerWords: []int{4}},
			{PC: 120, PointerWords: []int{1}},
			{PC: 160, PointerWords: nil},
		},
	})

	assert.Equal(t, [][]int{nil, {1}, {4}}, pointerMaps)
	assert.Equal(t, []StackMapIndexPoint{
		{PC: 80, Index: 2},
		{PC: 120, Index: 1},
		{PC: 160, Index: 0},
	}, indexPoints)
}

func TestGoFunctionStackMapsNormalizeSafepointPointerWords(t *testing.T) {
	pointerMaps, indexPoints := FunctionStackMaps(FunctionInfo{
		StackMapPoints: []StackMapPoint{
			{PC: 80, PointerWords: []int{4, 1, 4}},
			{PC: 120, PointerWords: []int{1, 4}},
		},
	})

	assert.Equal(t, [][]int{nil, nil, {1, 4}}, pointerMaps)
	assert.Equal(t, []StackMapIndexPoint{
		{PC: 80, Index: 2},
		{PC: 120, Index: 2},
	}, indexPoints)
}

func TestGoStackMapsSeparateEntryAndBodyRoots(t *testing.T) {
	builder := NewBuilder(testArch, Options{}, nil, nil)
	builder.stackMaps(8, []int{1}, []int{6})
	assert.Equal(t, []byte{
		2, 0, 0, 0,
		8, 0, 0, 0,
		0b00000010,
		0b01000000,
	}, builder.data)
}

func TestNoLocalPointersAssemblyUsesZeroBitStackMap(t *testing.T) {
	function := FunctionInfo{
		FrameSize:       134217744,
		NoLocalPointers: true,
	}
	assert.Equal(t, 0, testArch.LocalStackMapWords(function))

	builder := NewBuilder(testArch, Options{}, nil, nil)
	pointerMaps, _ := FunctionStackMaps(function)
	builder.stackMaps(testArch.LocalStackMapWords(function), pointerMaps...)
	assert.Equal(t, []byte{
		2, 0, 0, 0,
		0, 0, 0, 0,
	}, builder.data)
}
