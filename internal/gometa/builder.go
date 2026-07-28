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
// parameter.
const moduledataSize = 592

// textEndSymbol bounds the module's text range. It is moduledata.maxpc/etext and
// the sentinel entry at the end of functab, and is defined by the runtime
// support the backend links alongside the object.
const textEndSymbol = "runtime_gocTextEnd"

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
// the data GC program is built; the counts describe the module's inittasks and
// itablinks slices.
//
// The caller is responsible for the degenerate case of a module with no
// functions at all, which needs no metadata and only the moduledata symbol
// itself.
func AddObjectMetadata(arch Arch, options Options, object *obj.Object, functions, translatedFunctions []FunctionInfo, moduledata *ir.Data, pointerOffsets []uint64, moduleInitTaskCount, moduleItabLinkCount int) error {
	dataStart, ok := DataSymbolValue(object, ir.LinkerSymbol(".goc.runtime.datastart"))
	if !ok {
		return fmt.Errorf("Go runtime metadata: missing data-start symbol")
	}
	dataEnd, ok := DataSymbolValue(object, ir.LinkerSymbol(".goc.runtime.dataend"))
	if !ok {
		return fmt.Errorf("Go runtime metadata: missing data-end symbol")
	}
	gcProgram, err := GCProgram(dataStart, dataEnd, pointerOffsets)
	if err != nil {
		return fmt.Errorf("Go runtime metadata: %w", err)
	}
	noptrBSSName := ir.LinkerSymbol(".goc.runtime.dataend")
	noptrBSSSize := uint64(0)
	if symbol, found := DataSymbol(object, ir.LinkerSymbol("runtime.methodValueCallFrameObjs")); found {
		noptrBSSName = symbol.Name
		noptrBSSSize = symbol.Size
	}
	for len(object.Data)%8 != 0 {
		object.Data = append(object.Data, 0)
	}
	builder := NewBuilder(arch, options, object, object.Data)
	builder.Build(functions, translatedFunctions, moduledata, gcProgram, noptrBSSName, noptrBSSSize, moduleInitTaskCount, moduleItabLinkCount)
	object.Data = builder.data
	object.DataRelocs = append(object.DataRelocs, builder.relocs...)
	return nil
}

// Build lays out every metadata table and the moduledata record that names them.
func (builder *Builder) Build(functions, translatedFunctions []FunctionInfo, moduledata *ir.Data, gcProgram []byte, noptrBSSName string, noptrBSSSize uint64, moduleInitTaskCount, moduleItabLinkCount int) {
	endSymbol := textEndSymbol
	functions = append(functions, translatedFunctions...)
	functions = sortFunctionsByTextOffset(builder.object, functions)
	findFuncBuckets := findFuncBucketCount(builder.object, functions, endSymbol)
	stackMapIndexPoints := make([][]StackMapIndexPoint, len(functions))
	stackMapEntryCounts := make([]int, len(functions))
	for index, function := range functions {
		pointerMaps, indexPoints := FunctionStackMaps(function)
		stackMapEntryCounts[index] = len(pointerMaps)
		stackMapIndexPoints[index] = indexPoints
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
		builder.data = append(builder.data, builder.arch.StackMapPCData(function.FrameStart, stackMapIndexPoints[index])...)
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
		builder.stackMaps(words, ArgumentStackMaps(function, stackMapEntryCounts[index])...)
	}
	localOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		builder.align(4)
		localOffsets[index] = uint32(builder.position() - builder.labels[".goc.go.gofunc"])
		words := builder.arch.LocalStackMapWords(function)
		pointerMaps, _ := FunctionStackMaps(function)
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
	builder.data = append(builder.data, make([]byte, findFuncBuckets*20)...)
	builder.align(8)

	moduleStart := builder.position()
	builder.label(ir.LinkerSymbol(moduledata.Name))
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
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.datastart")) // noptrdata
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.datastart")) // enoptrdata
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.datastart")) // data
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.dataend"))   // edata
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.dataend"))   // bss
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.dataend"))   // ebss
	builder.externalPointer(noptrBSSName)                              // noptrbss
	builder.externalPointerOffset(noptrBSSName, int64(noptrBSSSize))   // enoptrbss
	builder.u64(0)                                                     // covctrs
	builder.u64(0)                                                     // ecovctrs
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.dataend"))   // end
	builder.pointer(".goc.go.gcdata")                                  // gcdata
	builder.pointer(".goc.go.gcbss")                                   // gcbss (empty bss)
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.datastart")) // types
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.dataend"))   // etypes
	builder.externalPointer(ir.LinkerSymbol(".goc.runtime.datastart")) // rodata
	builder.pointer(".goc.go.gofunc")
	builder.pointer(".goc.go.pclntable.end")
	builder.emptySlice() // textsectmap
	builder.emptySlice() // typelinks
	if moduleItabLinkCount > 0 {
		builder.externalPointer(ir.LinkerSymbol(ModuleItabLinksName))
		builder.u64(uint64(moduleItabLinkCount))
		builder.u64(uint64(moduleItabLinkCount))
	} else {
		builder.emptySlice()
	}
	builder.emptySlice() // ptab
	builder.u64(0)       // pluginpath
	builder.u64(0)
	builder.emptySlice() // pkghashes
	if moduleInitTaskCount > 0 {
		builder.externalPointer(ir.LinkerSymbol(ModuleInitTasksName))
		builder.u64(uint64(moduleInitTaskCount))
		builder.u64(uint64(moduleInitTaskCount))
	} else {
		builder.emptySlice()
	}
	// The tail of moduledata -- modulename through next -- is all zero, and the
	// runtime fills in what it needs at load time.
	builder.data = append(builder.data, make([]byte, moduledataSize-int(builder.position()-moduleStart))...)
	builder.object.Syms = append(builder.object.Syms, obj.Sym{
		Name: ir.LinkerSymbol(moduledata.Name), Section: obj.SecData,
		Value: moduleStart, Size: moduledataSize, Global: moduledata.Linkage.Export,
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
	builder.labels[name] = builder.position()
	if name != ir.LinkerSymbol("runtime.firstmoduledata") {
		builder.object.Syms = append(builder.object.Syms, obj.Sym{Name: name, Section: obj.SecData, Value: builder.position()})
	}
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
