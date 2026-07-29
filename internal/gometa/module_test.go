package gometa

import (
	"encoding/binary"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here cover what had to stop being image-wide before a goc image
// could carry a second Go module: the moduledata symbol name, the text-end
// symbol, moduledata.hasmain, moduledata.typelinks, and the reserved slot at
// offset zero of the function-name table.

// moduleObject is an object shaped like a Go module's: one function, and the
// data-start/data-end pair that bounds the module's type region.
func moduleObject(dataBytes int) *obj.Object {
	return &obj.Object{
		Machine: obj.EM_AARCH64,
		Data:    make([]byte, dataBytes),
		Syms: []obj.Sym{
			{Name: "only", Section: obj.SecText, Value: 0, Func: true},
			{Name: ir.LinkerSymbol(".goc.runtime.datastart"), Section: obj.SecData, Value: 0},
			{Name: ir.LinkerSymbol(".goc.runtime.dataend"), Section: obj.SecData, Value: uint64(dataBytes)},
		},
	}
}

func relocationsByOffset(object *obj.Object) map[uint64]obj.Reloc {
	byOffset := make(map[uint64]obj.Reloc, len(object.DataRelocs))
	for _, relocation := range object.DataRelocs {
		byOffset[relocation.Offset] = relocation
	}
	return byOffset
}

func moduleSymbol(t *testing.T, object *obj.Object, name string) obj.Sym {
	t.Helper()
	symbol, found := DataSymbol(object, name)
	require.Truef(t, found, "%s is not a data symbol of the object", name)
	return symbol
}

// A second module cannot be called runtime.firstmoduledata: that symbol is
// global and belongs to the module carrying the runtime's own state. The name is
// therefore a parameter, and nothing in the emitter may assume the default.
func TestModuledataIsEmittedUnderTheGivenName(t *testing.T) {
	object := moduleObject(16)
	module := Module{
		DataSymbol:      ir.LinkerSymbol(".goc.probe.moduledata"),
		DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
		DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
		TextEndSymbol:   TextEndSymbol(ir.LinkerSymbol(".goc.probe.moduledata")),
	}

	require.NoError(t, AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil))

	emitted := moduleSymbol(t, object, ir.LinkerSymbol(".goc.probe.moduledata"))
	assert.Equal(t, uint64(moduledataSize), emitted.Size)
	_, found := DataSymbol(object, DefaultModuleDataSymbol)
	assert.False(t, found, "the default moduledata name leaked into a module that did not ask for it")

	// The record is defined exactly once. label() used to emit a symbol for every
	// position it recorded and skip only the literal default name, so any other
	// name was defined twice.
	definitions := 0
	for _, symbol := range object.Syms {
		if symbol.Name == ir.LinkerSymbol(".goc.probe.moduledata") {
			definitions++
		}
	}
	assert.Equal(t, 1, definitions)
}

// The text-end symbol bounds moduledata.maxpc/etext and is the functab sentinel.
// A second module that shared the first module's symbol would take the first
// module's text end as its own maxpc, and runtime.findmoduledatap would never
// select it for a PC of its own.
func TestTextEndSymbolIsPerModule(t *testing.T) {
	assert.Equal(t, DefaultTextEndSymbol, TextEndSymbol(DefaultModuleDataSymbol))
	second := TextEndSymbol(ir.LinkerSymbol(".goc.probe.moduledata"))
	assert.NotEqual(t, DefaultTextEndSymbol, second)

	object := moduleObject(16)
	module := Module{
		DataSymbol:      ir.LinkerSymbol(".goc.probe.moduledata"),
		DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
		DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
		TextEndSymbol:   second,
	}

	require.NoError(t, AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil))

	moduledata := moduleSymbol(t, object, ir.LinkerSymbol(".goc.probe.moduledata"))
	relocations := relocationsByOffset(object)
	assert.Equal(t, second, relocations[moduledata.Value+168].Sym, "maxpc")
	assert.Equal(t, second, relocations[moduledata.Value+184].Sym, "etext")

	functab := moduleSymbol(t, object, ".goc.go.functab")
	assert.Equal(t, second, relocations[functab.Value+8].Sym, "functab sentinel")
}

// moduledata.hasmain is what runtime.modulesinit moves to the head of
// activeModules, which is the order runtime.typelinksinit resolves duplicate
// type descriptors in. gometa used to zero the whole tail of the record.
func TestModuledataRecordsHasMain(t *testing.T) {
	build := func(hasMain bool) (*obj.Object, obj.Sym) {
		object := moduleObject(16)
		module := Module{
			DataSymbol:      DefaultModuleDataSymbol,
			DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
			DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
			TextEndSymbol:   DefaultTextEndSymbol,
			HasMain:         hasMain,
		}
		require.NoError(t, AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil))
		return object, moduleSymbol(t, object, DefaultModuleDataSymbol)
	}

	withMain, moduledata := build(true)
	assert.Equal(t, byte(1), withMain.Data[moduledata.Value+moduledataHasMainOffset])
	// bad, the byte after it, stays zero: a module the runtime refused to load is
	// skipped entirely by modulesinit.
	assert.Equal(t, byte(0), withMain.Data[moduledata.Value+moduledataHasMainOffset+1])

	withoutMain, moduledata := build(false)
	assert.Equal(t, byte(0), withoutMain.Data[moduledata.Value+moduledataHasMainOffset])
}

// moduledata.typelinks is the list runtime.typelinksinit walks to give one Go
// type one identity across modules. Its entries are byte offsets from
// moduledata.types, so they have to be measured against the module's own base --
// which is the whole reason per-module regions work.
func TestModuledataTypelinksAreOffsetsFromTheModuleBase(t *testing.T) {
	const dataBytes = 64
	object := moduleObject(dataBytes)
	module := Module{
		DataSymbol:      DefaultModuleDataSymbol,
		DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
		DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
		TextEndSymbol:   DefaultTextEndSymbol,
		TypeDescriptors: []uint64{16, 40},
	}

	require.NoError(t, AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil))

	moduledata := moduleSymbol(t, object, DefaultModuleDataSymbol)
	typelinks := moduleSymbol(t, object, ".goc.go.typelinks")
	relocations := relocationsByOffset(object)
	assert.Equal(t, ".goc.go.typelinks", relocations[moduledata.Value+360].Sym)
	assert.Equal(t, uint64(2), binary.LittleEndian.Uint64(object.Data[moduledata.Value+368:]))
	assert.Equal(t, uint64(2), binary.LittleEndian.Uint64(object.Data[moduledata.Value+376:]))
	assert.Equal(t, uint32(16), binary.LittleEndian.Uint32(object.Data[typelinks.Value:]))
	assert.Equal(t, uint32(40), binary.LittleEndian.Uint32(object.Data[typelinks.Value+4:]))
}

// A module with no type descriptors keeps the empty slice it always had, so a
// single-module image is byte-identical in this field.
func TestModuledataTypelinksAreEmptyWithoutDescriptors(t *testing.T) {
	object := moduleObject(16)
	module := Module{
		DataSymbol:      DefaultModuleDataSymbol,
		DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
		DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
		TextEndSymbol:   DefaultTextEndSymbol,
	}

	require.NoError(t, AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil))

	moduledata := moduleSymbol(t, object, DefaultModuleDataSymbol)
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(object.Data[moduledata.Value+360:]))
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(object.Data[moduledata.Value+368:]))
}

// A descriptor outside the module's type region would hand
// runtime.typelinksinit an offset that does not address a type in this module,
// so it is refused at build time rather than becoming a wrong pointer at run
// time.
func TestTypeLinkOutsideTheModuleRegionIsRefused(t *testing.T) {
	object := moduleObject(16)
	module := Module{
		DataSymbol:      DefaultModuleDataSymbol,
		DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
		DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
		TextEndSymbol:   DefaultTextEndSymbol,
		TypeDescriptors: []uint64{4096},
	}

	err := AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the module's type region")
}

// runtime.moduledata.funcName reads a name offset of 0 as the empty string, so
// the function whose name landed at offset 0 was nameless in every traceback,
// runtime.Caller and runtime.FuncForPC result (RUNTIME_PLAN.md 5.10). Upstream's
// linker reserves the slot; so does this one now.
func TestFunctionNameTableReservesOffsetZero(t *testing.T) {
	object := moduleObject(16)
	module := Module{
		DataSymbol:      DefaultModuleDataSymbol,
		DataStartSymbol: ir.LinkerSymbol(".goc.runtime.datastart"),
		DataEndSymbol:   ir.LinkerSymbol(".goc.runtime.dataend"),
		TextEndSymbol:   DefaultTextEndSymbol,
	}

	require.NoError(t, AddObjectMetadata(testArch, Options{}, object, []FunctionInfo{{Name: "only", Size: 4}}, nil, module, nil))

	names := moduleSymbol(t, object, ".goc.go.funcnames")
	assert.Equal(t, byte(0), object.Data[names.Value], "offset 0 of the name table must stay reserved")
	assert.Equal(t, "only\x00", string(object.Data[names.Value+1:names.Value+6]))

	// The _func record's nameOff field is the second word of the record.
	pclntable := moduleSymbol(t, object, ".goc.go.pclntable")
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(object.Data[pclntable.Value+4:]))
}

// Joining a second module to an image is one absolute 64-bit data relocation
// into moduledata.next. Everything else the module needs travels in its own
// object.
func TestChainModuleWritesTheNextField(t *testing.T) {
	object := &obj.Object{
		Machine: obj.EM_AARCH64,
		Data:    make([]byte, 2*moduledataSize),
		Syms: []obj.Sym{
			{Name: "first", Section: obj.SecData, Value: 0, Size: moduledataSize},
			{Name: "second", Section: obj.SecData, Value: moduledataSize, Size: moduledataSize},
		},
	}

	require.NoError(t, ChainModule(object, "first", "second", obj.R_AARCH64_ABS64))

	require.Len(t, object.DataRelocs, 1)
	assert.Equal(t, uint64(ModuleNextOffset), object.DataRelocs[0].Offset)
	assert.Equal(t, "second", object.DataRelocs[0].Sym)
	assert.Equal(t, uint32(obj.R_AARCH64_ABS64), object.DataRelocs[0].Type)

	assert.Error(t, ChainModule(object, "missing", "second", obj.R_AARCH64_ABS64))
	assert.Error(t, ChainModule(object, "first", "missing", obj.R_AARCH64_ABS64))
}
