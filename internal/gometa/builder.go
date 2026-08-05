package gometa

import (
	"encoding/binary"
	"fmt"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// moduledataSize is the fixed size of runtime.moduledata on a 64-bit target. See
// the package comment: the record is the same 592 bytes with the same field
// offsets on every 64-bit architecture, so it is a constant rather than a
// parameter. The field offsets below are from the same record, transcribed into
// a standalone program and checked with unsafe.Offsetof against the host
// toolchain rather than counted by eye.
const moduledataSize = 592

const (
	// moduledataHasMainOffset is the byte offset of moduledata.hasmain, the flag
	// runtime.modulesinit uses to move the module holding main to the front of
	// activeModules.
	moduledataHasMainOffset = 536

	// ModuleNextOffset is the byte offset of moduledata.next, the field that
	// chains one module onto the next. Writing it is all it takes for
	// runtime.modulesinit to pick a second module up.
	ModuleNextOffset = 584
)

// DefaultTextEndSymbol is the text-end symbol of the module that carries the
// runtime's own runtime.firstmoduledata. The Go runtime support the backend
// assembles alongside the object defines it, once, under this name.
const DefaultTextEndSymbol = "runtime_gocTextEnd"

// DefaultModuleDataSymbol is the linker name of the runtime's own moduledata
// record, the head of the module chain. It is ir.LinkerSymbol applied to
// runtime.firstmoduledata, spelled out so it stays a constant;
// TestDefaultModuleDataSymbolMatchesTheRuntimeName keeps the two in step.
const DefaultModuleDataSymbol = "runtime_firstmoduledata"

// TextEndSymbol is the text-end symbol of the module whose moduledata record is
// named dataSymbol.
//
// The name has to be derived per module rather than fixed. It bounds
// moduledata.maxpc/etext and is the sentinel entry at the end of functab, so a
// second module that shared the first module's symbol would claim the first
// module's text end as its own maxpc, and runtime.findmoduledatap would never
// select it for any PC of its own.
func TextEndSymbol(dataSymbol string) string {
	if dataSymbol == DefaultModuleDataSymbol {
		return DefaultTextEndSymbol
	}
	return dataSymbol + "_gocTextEnd"
}

// Module is the per-module half of the metadata emitter's input: the symbols
// that bound this module's regions, and the facts moduledata records about it.
//
// Every name here was a package constant until a goc image could hold more than
// one Go module. They have to be per module because a module resolves its own
// NameOff/TypeOff values against its own type base
// (runtime.resolveNameOff/resolveTypeOff pick the module containing the
// referring type), so the base -- and everything else that bounds the module --
// is a property of the module rather than of the image.
type Module struct {
	// DataSymbol is the linker name of the runtime.moduledata record this
	// metadata fills in. Only the module carrying the runtime's own
	// firstmoduledata may use DefaultModuleDataSymbol; a second module needs a
	// name of its own, because that symbol is global.
	DataSymbol string

	// DataExport makes the moduledata symbol global. The runtime's own
	// firstmoduledata is referenced by name from the runtime source; a second
	// module's is reached through the chain, so it need not be.
	DataExport bool

	// DataStartSymbol and DataEndSymbol bound the module's data, which is also
	// its type region: they become moduledata.data/edata and types/etypes.
	DataStartSymbol string
	DataEndSymbol   string

	// TextEndSymbol bounds the module's text: moduledata.maxpc/etext and the
	// sentinel entry at the end of functab. See TextEndSymbol.
	TextEndSymbol string

	// HasMain marks the module that defines main. runtime.modulesinit moves it
	// to the front of activeModules, which is the order typelinksinit resolves
	// duplicate type descriptors in.
	HasMain bool

	// TypeDescriptors are the data-section offsets of this module's complete Go
	// type descriptors, as obj.Sym.Value records them. They become
	// moduledata.typelinks, the list runtime.typelinksinit walks to build the
	// typemap that gives one Go type one identity across modules.
	TypeDescriptors []uint64

	// InitTaskCount and ItabLinkCount describe the module's inittasks and
	// itablinks slices.
	InitTaskCount int
	ItabLinkCount int
}

// Builder appends the Go metadata to an object's data section, tracking the
// labels the tables reference each other by and the relocations they need. It
// records positions as absolute .data offsets, so a label doubles as the value
// of the local symbol emitted for it.
type Builder struct {
	arch        Arch
	extraPCData func(FunctionInfo) []byte
	object      *obj.Object
	base        uint64
	data        []byte
	labels      map[string]uint64
	relocs      []obj.Reloc
}

// NewBuilder starts a builder that appends to data -- normally the object's
// existing data section, already padded to an 8-byte boundary.
func NewBuilder(arch Arch, options Options, object *obj.Object, data []byte) *Builder {
	return &Builder{
		arch:        arch,
		extraPCData: options.ExtraPCData,
		object:      object,
		data:        data,
		labels:      make(map[string]uint64),
	}
}

// AddObjectMetadata appends the module's complete Go metadata to the object.
// pointerOffsets are the data-section byte offsets holding pointers, from which
// the data GC program is built.
//
// The caller is responsible for the degenerate case of a module with no
// functions at all, which needs no metadata and only the moduledata symbol
// itself.
func AddObjectMetadata(arch Arch, options Options, object *obj.Object, functions, translatedFunctions []FunctionInfo, module Module, pointerOffsets []uint64) error {
	dataStart, ok := DataSymbolValue(object, module.DataStartSymbol)
	if !ok {
		return fmt.Errorf("Go runtime metadata: missing data-start symbol %s", module.DataStartSymbol)
	}
	dataEnd, ok := DataSymbolValue(object, module.DataEndSymbol)
	if !ok {
		return fmt.Errorf("Go runtime metadata: missing data-end symbol %s", module.DataEndSymbol)
	}
	gcProgram, err := GCProgram(dataStart, dataEnd, pointerOffsets)
	if err != nil {
		return fmt.Errorf("Go runtime metadata: %w", err)
	}
	typeLinks, err := typeLinkOffsets(module.TypeDescriptors, dataStart, dataEnd)
	if err != nil {
		return fmt.Errorf("Go runtime metadata: %w", err)
	}
	noptrBSSName := module.DataEndSymbol
	noptrBSSSize := uint64(0)
	if symbol, found := DataSymbol(object, ir.LinkerSymbol("runtime.methodValueCallFrameObjs")); found {
		noptrBSSName = symbol.Name
		noptrBSSSize = symbol.Size
	}
	for len(object.Data)%8 != 0 {
		object.Data = append(object.Data, 0)
	}
	builder := NewBuilder(arch, options, object, object.Data)
	builder.Build(functions, translatedFunctions, module, typeLinks, gcProgram, noptrBSSName, noptrBSSSize)
	object.Data = builder.data
	object.DataRelocs = append(object.DataRelocs, builder.relocs...)
	return nil
}

// typeLinkOffsets converts the data-section offsets of a module's type
// descriptors into the module-relative offsets moduledata.typelinks holds.
//
// A descriptor outside [dataStart, dataEnd) would give runtime.typelinksinit an
// offset that does not address a type in this module's region, so it is refused
// here rather than turned into a wrong pointer at run time.
func typeLinkOffsets(descriptors []uint64, dataStart, dataEnd uint64) ([]uint32, error) {
	if len(descriptors) == 0 {
		return nil, nil
	}
	offsets := make([]uint32, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor < dataStart || descriptor >= dataEnd {
			return nil, fmt.Errorf("type descriptor at data offset %d is outside the module's type region [%d, %d)", descriptor, dataStart, dataEnd)
		}
		offsets = append(offsets, uint32(descriptor-dataStart))
	}
	return offsets, nil
}

// Build lays out every metadata table and the moduledata record that names them.
func (builder *Builder) Build(functions, translatedFunctions []FunctionInfo, module Module, typeLinks []uint32, gcProgram []byte, noptrBSSName string, noptrBSSSize uint64) {
	endSymbol := module.TextEndSymbol
	functions = append(functions, translatedFunctions...)
	functions = sortFunctionsByTextOffset(builder.object, functions)
	findFuncBuckets := findFuncBucketCount(builder.object, functions, endSymbol)
	stackMapIndexPoints := make([][]StackMapIndexPoint, len(functions))
	stackMapEntryCounts := make([]int, len(functions))
	growthStackMapIndexes := make([]int, len(functions))
	for index, function := range functions {
		pointerMaps, indexPoints, growthIndex := FunctionStackMaps(function)
		stackMapEntryCounts[index] = len(pointerMaps)
		stackMapIndexPoints[index] = indexPoints
		growthStackMapIndexes[index] = growthIndex
	}
	pcDataSlots := GoPCDataSlots
	if builder.extraPCData != nil {
		pcDataSlots++
	}

	builder.label(".goc.go.gcbss")
	builder.bytes(0)
	builder.align(8)
	builder.label(".goc.go.gcdata")
	builder.data = append(builder.data, gcProgram...)
	builder.align(8)

	builder.label(".goc.go.pcheader")
	builder.u32(0xfffffff1)
	builder.bytes(0, 0, byte(builder.arch.PCQuantum), 8)
	builder.u64(uint64(len(functions)))
	builder.u64(0)
	builder.u64(0)
	for range 5 {
		builder.u64(0)
	}

	builder.label(".goc.go.funcnames")
	// Offset 0 of the name table is reserved. runtime.moduledata.funcName reads a
	// name offset of 0 as the empty string, so whichever function landed at
	// offset 0 was nameless in every traceback, runtime.Caller and
	// runtime.FuncForPC result (RUNTIME_PLAN.md 5.10). Upstream's linker reserves
	// the slot for exactly this reason; one sentinel byte is the whole fix.
	builder.bytes(0)
	nameOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		nameOffsets[index] = uint32(builder.offset(".goc.go.funcnames"))
		builder.data = append(builder.data, function.Name...)
		builder.data = append(builder.data, 0)
	}
	builder.label(".goc.go.funcnames.end")
	builder.align(4)

	builder.label(".goc.go.pctab")
	builder.bytes(0)
	pcspOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		pcspOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
		builder.data = append(builder.data, builder.arch.PCSP(function.FrameStart, function.FrameSize)...)
	}
	unsafePointOffsets := make([]uint32, len(functions))
	for index := range functions {
		unsafePointOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
		builder.data = append(builder.data, UnsafePointPCData()...)
	}
	stackMapOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		stackMapOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
		builder.data = append(builder.data, builder.arch.StackMapPCData(function.FrameStart, growthStackMapIndexes[index], stackMapIndexPoints[index])...)
	}
	extraPCDataOffsets := make([]uint32, len(functions))
	if builder.extraPCData != nil {
		for index, function := range functions {
			extraPCDataOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
			builder.data = append(builder.data, builder.extraPCData(function)...)
		}
	}
	builder.label(".goc.go.pctab.end")
	builder.align(4)

	// Build gofunc before pclntable so its offsets are known when _func records
	// are written. The section ordering is immaterial; moduledata names each one.
	builder.label(".goc.go.gofunc")
	argumentOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		builder.align(4)
		argumentOffsets[index] = uint32(builder.position() - builder.labels[".goc.go.gofunc"])
		words := (function.ArgumentSize + 7) / 8
		builder.stackMaps(words, ArgumentStackMaps(function, stackMapEntryCounts[index], growthStackMapIndexes[index])...)
	}
	localOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		builder.align(4)
		localOffsets[index] = uint32(builder.position() - builder.labels[".goc.go.gofunc"])
		words := builder.arch.LocalStackMapWords(function)
		pointerMaps, _, _ := FunctionStackMaps(function)
		builder.stackMaps(words, pointerMaps...)
	}
	builder.label(".goc.go.gofunc.end")
	builder.align(4)

	builder.label(".goc.go.pclntable")
	functionOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		functionOffsets[index] = uint32(builder.offset(".goc.go.pclntable"))
		builder.reloc32(function.Name)
		builder.u32(nameOffsets[index])
		builder.u32(uint32(function.ArgumentSize))
		builder.u32(function.DeferReturn)
		builder.u32(pcspOffsets[index])
		builder.u32(0)
		builder.u32(0)
		builder.u32(uint32(pcDataSlots))
		builder.u32(0)
		builder.u32(0)
		builder.bytes(function.FuncID, function.FuncFlag, 0, FuncDataSlots)
		builder.u32(unsafePointOffsets[index])
		builder.u32(stackMapOffsets[index])
		builder.u32(0)
		builder.u32(0)
		builder.u32(0)
		if builder.extraPCData != nil {
			builder.u32(extraPCDataOffsets[index])
		}
		builder.u32(argumentOffsets[index])
		builder.u32(localOffsets[index])
	}
	builder.label(".goc.go.pclntable.end")
	builder.align(4)

	builder.label(".goc.go.functab")
	for index, function := range functions {
		builder.reloc32(function.Name)
		builder.u32(functionOffsets[index])
	}
	builder.reloc32(endSymbol)
	builder.u32(0)
	builder.label(".goc.go.functab.end")
	builder.align(4)

	builder.label(".goc.go.findfunctab")
	builder.data = append(builder.data, FindFuncTab(builder.object, functions, endSymbol, findFuncBuckets)...)
	builder.align(8)

	// moduledata.typelinks: one module-relative offset per complete Go type
	// descriptor the module defines. runtime.typelinksinit walks it to build the
	// typemap that keeps one Go type to one *_type across modules, which is what
	// stops two modules that each describe the same type from disagreeing about
	// reflect.TypeOf identity. It is only consulted when the chain is longer than
	// one module, so a single-module image pays the table's bytes and nothing
	// else.
	builder.align(4)
	builder.label(".goc.go.typelinks")
	for _, offset := range typeLinks {
		builder.u32(offset)
	}
	builder.label(".goc.go.typelinks.end")
	builder.align(8)

	moduleStart := builder.position()
	builder.labelOnly(module.DataSymbol)
	builder.pointer(".goc.go.pcheader")
	builder.slice(".goc.go.funcnames", ".goc.go.funcnames.end")
	builder.emptySlice()
	builder.emptySlice()
	builder.slice(".goc.go.pctab", ".goc.go.pctab.end")
	builder.slice(".goc.go.pclntable", ".goc.go.pclntable.end")
	builder.sliceCount(".goc.go.functab", len(functions)+1)
	builder.pointer(".goc.go.findfunctab")
	builder.externalPointer(functions[0].Name)
	builder.externalPointer(endSymbol)
	builder.u64(0) // text base: function entry offsets contain absolute addresses
	builder.externalPointer(endSymbol)
	builder.externalPointer(module.DataStartSymbol)                  // noptrdata
	builder.externalPointer(module.DataStartSymbol)                  // enoptrdata
	builder.externalPointer(module.DataStartSymbol)                  // data
	builder.externalPointer(module.DataEndSymbol)                    // edata
	builder.externalPointer(module.DataEndSymbol)                    // bss
	builder.externalPointer(module.DataEndSymbol)                    // ebss
	builder.externalPointer(noptrBSSName)                            // noptrbss
	builder.externalPointerOffset(noptrBSSName, int64(noptrBSSSize)) // enoptrbss
	builder.u64(0)                                                   // covctrs
	builder.u64(0)                                                   // ecovctrs
	builder.externalPointer(module.DataEndSymbol)                    // end
	builder.pointer(".goc.go.gcdata")                                // gcdata
	builder.pointer(".goc.go.gcbss")                                 // gcbss (empty bss)
	builder.externalPointer(module.DataStartSymbol)                  // types
	builder.externalPointer(module.DataEndSymbol)                    // etypes
	builder.externalPointer(module.DataStartSymbol)                  // rodata
	builder.pointer(".goc.go.gofunc")
	builder.pointer(".goc.go.pclntable.end")
	builder.emptySlice() // textsectmap
	if len(typeLinks) > 0 {
		builder.sliceCount(".goc.go.typelinks", len(typeLinks))
	} else {
		builder.emptySlice() // typelinks
	}
	if module.ItabLinkCount > 0 {
		builder.externalPointer(ir.LinkerSymbol(ModuleItabLinksName))
		builder.u64(uint64(module.ItabLinkCount))
		builder.u64(uint64(module.ItabLinkCount))
	} else {
		builder.emptySlice()
	}
	builder.emptySlice() // ptab
	builder.u64(0)       // pluginpath
	builder.u64(0)
	builder.emptySlice() // pkghashes
	if module.InitTaskCount > 0 {
		builder.externalPointer(ir.LinkerSymbol(ModuleInitTasksName))
		builder.u64(uint64(module.InitTaskCount))
		builder.u64(uint64(module.InitTaskCount))
	} else {
		builder.emptySlice()
	}
	// modulename and modulehashes: both empty, and both left for the runtime.
	builder.u64(0)
	builder.u64(0)
	builder.emptySlice()
	// hasmain, then bad. The rest of the tail -- gcdatamask, gcbssmask, typemap
	// and next -- is zero, and the runtime fills in what it needs at load time.
	// next is what a second module is chained onto; see ModuleNextOffset.
	hasMain := byte(0)
	if module.HasMain {
		hasMain = 1
	}
	builder.bytes(hasMain, 0)
	builder.data = append(builder.data, make([]byte, moduledataSize-int(builder.position()-moduleStart))...)
	builder.object.Syms = append(builder.object.Syms, obj.Sym{
		Name: module.DataSymbol, Section: obj.SecData,
		Value: moduleStart, Size: moduledataSize, Global: module.DataExport,
	})

	// Fill pcHeader offsets now that every table position is known.
	header := builder.labels[".goc.go.pcheader"] - builder.base
	for index, label := range []string{".goc.go.funcnames", "", "", ".goc.go.pctab", ".goc.go.pclntable"} {
		if label == "" {
			continue
		}
		offset := builder.labels[label] - builder.labels[".goc.go.pcheader"]
		binary.LittleEndian.PutUint64(builder.data[header+32+uint64(index*8):], offset)
	}
}

func (builder *Builder) position() uint64 {
	return builder.base + uint64(len(builder.data))
}

func (builder *Builder) label(name string) {
	builder.labelOnly(name)
	builder.object.Syms = append(builder.object.Syms, obj.Sym{Name: name, Section: obj.SecData, Value: builder.position()})
}

// labelOnly records a position without emitting a symbol for it, for a label
// whose symbol the caller emits itself. moduledata is the only one: it carries a
// size and a linkage of its own, so label would define it a second time.
func (builder *Builder) labelOnly(name string) {
	builder.labels[name] = builder.position()
}

func (builder *Builder) offset(label string) uint64 {
	return builder.position() - builder.labels[label]
}

func (builder *Builder) align(alignment int) {
	for len(builder.data)%alignment != 0 {
		builder.data = append(builder.data, 0)
	}
}

func (builder *Builder) bytes(values ...byte) {
	builder.data = append(builder.data, values...)
}

func (builder *Builder) stackMaps(words int, pointerWordMaps ...[]int) {
	builder.u32(uint32(len(pointerWordMaps)))
	builder.u32(uint32(words))
	for _, pointerWords := range pointerWordMaps {
		bitmap := make([]byte, (words+7)/8)
		for _, word := range pointerWords {
			if word >= 0 && word < words {
				bitmap[word/8] |= 1 << (word % 8)
			}
		}
		builder.data = append(builder.data, bitmap...)
	}
}

func (builder *Builder) u32(value uint32) {
	var bytes [4]byte
	binary.LittleEndian.PutUint32(bytes[:], value)
	builder.data = append(builder.data, bytes[:]...)
}

func (builder *Builder) u64(value uint64) {
	var bytes [8]byte
	binary.LittleEndian.PutUint64(bytes[:], value)
	builder.data = append(builder.data, bytes[:]...)
}

// uvarint has no caller today -- the pc-value tables encode through pcValueTable
// rather than into the data stream -- but it belongs with the other encoders for
// the next table that needs a varint written inline.
func (builder *Builder) uvarint(value uint32) {
	for value >= 0x80 {
		builder.bytes(byte(value) | 0x80)
		value >>= 7
	}
	builder.bytes(byte(value))
}

func (builder *Builder) reloc32(symbol string) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: symbol, Type: builder.arch.Reloc32})
	builder.u32(0)
}

func (builder *Builder) pointer(label string) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: label, Type: builder.arch.Reloc64})
	builder.u64(0)
}

func (builder *Builder) externalPointer(symbol string) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: symbol, Type: builder.arch.Reloc64})
	builder.u64(0)
}

func (builder *Builder) externalPointerOffset(symbol string, offset int64) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: symbol, Type: builder.arch.Reloc64, Addend: offset})
	builder.u64(0)
}

func (builder *Builder) slice(start, end string) {
	builder.pointer(start)
	length := builder.labels[end] - builder.labels[start]
	builder.u64(length)
	builder.u64(length)
}

func (builder *Builder) sliceCount(start string, count int) {
	builder.pointer(start)
	builder.u64(uint64(count))
	builder.u64(uint64(count))
}

func (builder *Builder) emptySlice() {
	builder.u64(0)
	builder.u64(0)
	builder.u64(0)
}
