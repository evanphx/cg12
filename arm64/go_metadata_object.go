package arm64

import (
	"encoding/binary"
	"fmt"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

type goFunctionInfo struct {
	name         string
	frameSize    int
	frameStart   int
	size         uint64
	funcID       byte
	funcFlag     byte
	pointerWords []int
}

const goModuleInitTasksName = ".goc.module.inittasks"

func goModuleInitTaskCount(module *ir.Module) int {
	for _, data := range module.Data {
		if data.Name == goModuleInitTasksName {
			return len(data.Items)
		}
	}
	return 0
}

func moduleUsesGoRuntime(module *ir.Module) bool {
	for _, function := range module.Funcs {
		if function.Name == "runtime.schedinit" {
			return true
		}
	}
	return false
}

type goMetadataBuilder struct {
	object *obj.Object
	base   uint64
	data   []byte
	labels map[string]uint64
	relocs []obj.Reloc
}

func addGoRuntimeObjectMetadata(object *obj.Object, functions []goFunctionInfo, moduledata *ir.Data, pointerOffsets []uint64, moduleInitTaskCount int) error {
	if len(functions) == 0 {
		return addData(object, moduledata)
	}
	dataStart, ok := dataSymbolValue(object, sanitize(".goc.runtime.datastart"))
	if !ok {
		return fmt.Errorf("Go runtime metadata: missing data-start symbol")
	}
	dataEnd, ok := dataSymbolValue(object, sanitize(".goc.runtime.dataend"))
	if !ok {
		return fmt.Errorf("Go runtime metadata: missing data-end symbol")
	}
	gcProgram, err := goGCProgram(dataStart, dataEnd, pointerOffsets)
	if err != nil {
		return fmt.Errorf("Go runtime metadata: %w", err)
	}
	noptrBSSName := sanitize(".goc.runtime.dataend")
	noptrBSSSize := uint64(0)
	if symbol, found := dataSymbol(object, sanitize("runtime.methodValueCallFrameObjs")); found {
		noptrBSSName = symbol.Name
		noptrBSSSize = symbol.Size
	}
	for len(object.Data)%8 != 0 {
		object.Data = append(object.Data, 0)
	}
	builder := &goMetadataBuilder{object: object, base: uint64(len(object.Data)), labels: make(map[string]uint64)}
	builder.build(functions, moduledata, gcProgram, noptrBSSName, noptrBSSSize, moduleInitTaskCount)
	object.Data = append(object.Data, builder.data...)
	object.DataRelocs = append(object.DataRelocs, builder.relocs...)
	return nil
}

func dataSymbolValue(object *obj.Object, name string) (uint64, bool) {
	symbol, ok := dataSymbol(object, name)
	return symbol.Value, ok
}

func dataSymbol(object *obj.Object, name string) (obj.Sym, bool) {
	for _, symbol := range object.Syms {
		if symbol.Name == name && (symbol.Section == obj.SecData || symbol.Section == obj.SecBss) {
			return symbol, true
		}
	}
	return obj.Sym{}, false
}

func goGCProgram(dataStart, dataEnd uint64, pointerOffsets []uint64) ([]byte, error) {
	if dataEnd < dataStart || (dataEnd-dataStart)%8 != 0 {
		return nil, fmt.Errorf("data range [%d, %d) is not word aligned", dataStart, dataEnd)
	}
	words := int((dataEnd - dataStart) / 8)
	bitmap := make([]byte, (words+7)/8)
	for _, offset := range pointerOffsets {
		if offset < dataStart || offset >= dataEnd {
			continue
		}
		if (offset-dataStart)%8 != 0 {
			return nil, fmt.Errorf("pointer at byte offset %d is not word aligned", offset-dataStart)
		}
		word := int((offset - dataStart) / 8)
		bitmap[word/8] |= 1 << (word % 8)
	}

	// A GC program literal holds at most 127 bits. Use byte-aligned chunks so
	// each instruction can copy directly from the packed bitmap.
	program := make([]byte, 0, len(bitmap)+len(bitmap)/15+2)
	for first := 0; first < words; {
		count := words - first
		if count > 120 {
			count = 120
		}
		program = append(program, byte(count))
		bytes := (count + 7) / 8
		program = append(program, bitmap[first/8:first/8+bytes]...)
		first += count
	}
	program = append(program, 0)
	return program, nil
}

func (builder *goMetadataBuilder) build(functions []goFunctionInfo, moduledata *ir.Data, gcProgram []byte, noptrBSSName string, noptrBSSSize uint64, moduleInitTaskCount int) {
	const findFuncBuckets = 4096
	functions = append(functions, goAssemblyFunctionInfo()...)

	builder.label(".goc.go.gcbss")
	builder.bytes(0)
	builder.align(8)
	builder.label(".goc.go.gcdata")
	builder.data = append(builder.data, gcProgram...)
	builder.align(8)

	builder.label(".goc.go.pcheader")
	builder.u32(0xfffffff1)
	builder.bytes(0, 0, 4, 8)
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
		builder.data = append(builder.data, function.name...)
		builder.data = append(builder.data, 0)
	}
	builder.label(".goc.go.funcnames.end")
	builder.align(4)

	builder.label(".goc.go.pctab")
	builder.bytes(0)
	pcspOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		pcspOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
		builder.data = append(builder.data, goPCSP(function.frameStart, function.frameSize)...)
	}
	builder.label(".goc.go.pctab.end")
	builder.align(4)

	// Build gofunc before pclntable so its offsets are known when _func records
	// are written. The section ordering is immaterial; moduledata names each one.
	builder.label(".goc.go.gofunc")
	emptyStackMap := builder.position()
	builder.u32(1)
	builder.u32(0)
	localOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		builder.align(4)
		localOffsets[index] = uint32(builder.position() - builder.labels[".goc.go.gofunc"])
		words := (function.frameSize - 16) / 8
		if words < 0 {
			words = 0
		}
		builder.u32(1)
		builder.u32(uint32(words))
		bitmap := make([]byte, (words+7)/8)
		for _, word := range function.pointerWords {
			if word >= 0 && word < words {
				bitmap[word/8] |= 1 << (word % 8)
			}
		}
		builder.data = append(builder.data, bitmap...)
	}
	builder.label(".goc.go.gofunc.end")
	builder.align(4)

	builder.label(".goc.go.pclntable")
	functionOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		functionOffsets[index] = uint32(builder.offset(".goc.go.pclntable"))
		builder.reloc32(function.name)
		builder.u32(nameOffsets[index])
		builder.u32(0)
		builder.u32(0)
		builder.u32(pcspOffsets[index])
		builder.u32(0)
		builder.u32(0)
		builder.u32(0)
		builder.u32(0)
		builder.u32(0)
		builder.bytes(function.funcID, function.funcFlag, 0, 2)
		builder.u32(uint32(emptyStackMap - builder.labels[".goc.go.gofunc"]))
		builder.u32(localOffsets[index])
	}
	builder.label(".goc.go.pclntable.end")
	builder.align(4)

	builder.label(".goc.go.functab")
	for index, function := range functions {
		builder.reloc32(function.name)
		builder.u32(functionOffsets[index])
	}
	endSymbol := "runtime_gocTextEnd"
	builder.reloc32(endSymbol)
	builder.u32(0)
	builder.label(".goc.go.functab.end")
	builder.align(4)

	builder.label(".goc.go.findfunctab")
	builder.data = append(builder.data, make([]byte, findFuncBuckets*20)...)
	builder.align(8)

	moduleStart := builder.position()
	builder.label(sanitize(moduledata.Name))
	builder.pointer(".goc.go.pcheader")
	builder.slice(".goc.go.funcnames", ".goc.go.funcnames.end")
	builder.emptySlice()
	builder.emptySlice()
	builder.slice(".goc.go.pctab", ".goc.go.pctab.end")
	builder.slice(".goc.go.pclntable", ".goc.go.pclntable.end")
	builder.sliceCount(".goc.go.functab", len(functions)+1)
	builder.pointer(".goc.go.findfunctab")
	builder.externalPointer(functions[0].name)
	builder.externalPointer(endSymbol)
	builder.u64(0) // text base: function entry offsets contain absolute addresses
	builder.externalPointer(endSymbol)
	builder.externalPointer(sanitize(".goc.runtime.datastart"))      // noptrdata
	builder.externalPointer(sanitize(".goc.runtime.datastart"))      // enoptrdata
	builder.externalPointer(sanitize(".goc.runtime.datastart"))      // data
	builder.externalPointer(sanitize(".goc.runtime.dataend"))        // edata
	builder.externalPointer(sanitize(".goc.runtime.dataend"))        // bss
	builder.externalPointer(sanitize(".goc.runtime.dataend"))        // ebss
	builder.externalPointer(noptrBSSName)                            // noptrbss
	builder.externalPointerOffset(noptrBSSName, int64(noptrBSSSize)) // enoptrbss
	builder.u64(0)                                                   // covctrs
	builder.u64(0)                                                   // ecovctrs
	builder.externalPointer(sanitize(".goc.runtime.dataend"))        // end
	builder.pointer(".goc.go.gcdata")                                // gcdata
	builder.pointer(".goc.go.gcbss")                                 // gcbss (empty bss)
	builder.externalPointer(sanitize(".goc.runtime.datastart"))      // types
	builder.externalPointer(sanitize(".goc.runtime.dataend"))        // etypes
	builder.externalPointer(sanitize(".goc.runtime.datastart"))      // rodata
	builder.pointer(".goc.go.gofunc")
	builder.pointer(".goc.go.pclntable.end")
	for range 4 {
		builder.emptySlice()
	}
	builder.u64(0) // pluginpath
	builder.u64(0)
	builder.emptySlice() // pkghashes
	if moduleInitTaskCount > 0 {
		builder.externalPointer(sanitize(goModuleInitTasksName))
		builder.u64(uint64(moduleInitTaskCount))
		builder.u64(uint64(moduleInitTaskCount))
	} else {
		builder.emptySlice()
	}
	builder.data = append(builder.data, make([]byte, 96)...)
	builder.object.Syms = append(builder.object.Syms, obj.Sym{
		Name: sanitize(moduledata.Name), Section: obj.SecData,
		Value: moduleStart, Size: 592, Global: moduledata.Linkage.Export,
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

func goPCSP(frameStart, frameSize int) []byte {
	var data []byte
	appendUvarint := func(value uint32) {
		for value >= 0x80 {
			data = append(data, byte(value)|0x80)
			value >>= 7
		}
		data = append(data, byte(value))
	}
	if frameStart > 0 {
		appendUvarint(2) // initial value: -1 -> 0 before frame allocation
		appendUvarint(uint32(frameStart / 4))
		appendUvarint(uint32(frameSize) * 2)
	} else {
		appendUvarint(uint32(frameSize+1) * 2)
	}
	appendUvarint(^uint32(0))
	appendUvarint(0) // end of this function's pc-value table
	return data
}

// goAssemblyFunctionInfo splits the assembly support code at every function
// that changes how the runtime must unwind the stack. Leaf helpers between
// these entries can safely share the preceding zero-frame Asm record.
//
// Keep this list in text order. In particular, morestack_restore_end aliases
// morestack_noctxt and cannot have its own functab entry: making that alias a
// TopFrame used to classify all following scheduler assembly as a terminal
// frame and made GC stack walks stop early.
func goAssemblyFunctionInfo() []goFunctionInfo {
	const (
		funcFlagTopFrame = 1
		funcFlagSPWrite  = 2
		funcFlagAsm      = 4

		funcIDAsmCGOCall        = 2
		funcIDGoexit            = 8
		funcIDGogo              = 9
		funcIDMcall             = 12
		funcIDMstart            = 14
		funcIDSystemstack       = 21
		funcIDSystemstackSwitch = 22
	)

	return []goFunctionInfo{
		{name: "runtime_gocPrintString", funcFlag: funcFlagAsm},
		{name: "runtime_gogo", funcID: funcIDGogo, funcFlag: funcFlagSPWrite | funcFlagAsm},
		{name: "runtime_mcall", funcID: funcIDMcall, funcFlag: funcFlagSPWrite | funcFlagAsm},
		// morestack has no x29 frame setup, so its locals bitmap starts at sp+8.
		// It keeps untyped register values unscanned and mirrors only pointer
		// arguments into words 27 through 34.
		{name: "runtime_morestack_restore", frameSize: 320, funcFlag: funcFlagAsm, pointerWords: []int{25, 26, 27, 28, 29, 30, 31, 32, 33, 34}},
		{name: "runtime_morestack_noctxt", funcFlag: funcFlagAsm},
		{name: "runtime_systemstack", funcID: funcIDSystemstack, funcFlag: funcFlagSPWrite | funcFlagAsm},
		{name: "runtime_systemstack_switch", funcID: funcIDSystemstackSwitch, funcFlag: funcFlagAsm},
		{name: "runtime_mstart", funcID: funcIDMstart, funcFlag: funcFlagTopFrame | funcFlagAsm},
		{name: "runtime_goexit", funcID: funcIDGoexit, funcFlag: funcFlagTopFrame | funcFlagAsm},
		{name: "runtime_asmcgocall", funcID: funcIDAsmCGOCall, funcFlag: funcFlagTopFrame | funcFlagAsm},
	}
}

func (builder *goMetadataBuilder) position() uint64 {
	return builder.base + uint64(len(builder.data))
}

func (builder *goMetadataBuilder) label(name string) {
	builder.labels[name] = builder.position()
	if name != sanitize("runtime.firstmoduledata") {
		builder.object.Syms = append(builder.object.Syms, obj.Sym{Name: name, Section: obj.SecData, Value: builder.position()})
	}
}

func (builder *goMetadataBuilder) offset(label string) uint64 {
	return builder.position() - builder.labels[label]
}

func (builder *goMetadataBuilder) align(alignment int) {
	for len(builder.data)%alignment != 0 {
		builder.data = append(builder.data, 0)
	}
}

func (builder *goMetadataBuilder) bytes(values ...byte) {
	builder.data = append(builder.data, values...)
}

func (builder *goMetadataBuilder) u32(value uint32) {
	var bytes [4]byte
	binary.LittleEndian.PutUint32(bytes[:], value)
	builder.data = append(builder.data, bytes[:]...)
}

func (builder *goMetadataBuilder) u64(value uint64) {
	var bytes [8]byte
	binary.LittleEndian.PutUint64(bytes[:], value)
	builder.data = append(builder.data, bytes[:]...)
}

func (builder *goMetadataBuilder) uvarint(value uint32) {
	for value >= 0x80 {
		builder.bytes(byte(value) | 0x80)
		value >>= 7
	}
	builder.bytes(byte(value))
}

func (builder *goMetadataBuilder) reloc32(symbol string) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: symbol, Type: obj.R_AARCH64_ABS32})
	builder.u32(0)
}

func (builder *goMetadataBuilder) pointer(label string) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: label, Type: obj.R_AARCH64_ABS64})
	builder.u64(0)
}

func (builder *goMetadataBuilder) externalPointer(symbol string) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: symbol, Type: obj.R_AARCH64_ABS64})
	builder.u64(0)
}

func (builder *goMetadataBuilder) externalPointerOffset(symbol string, offset int64) {
	builder.relocs = append(builder.relocs, obj.Reloc{Offset: builder.position(), Sym: symbol, Type: obj.R_AARCH64_ABS64, Addend: offset})
	builder.u64(0)
}

func (builder *goMetadataBuilder) slice(start, end string) {
	builder.pointer(start)
	length := builder.labels[end] - builder.labels[start]
	builder.u64(length)
	builder.u64(length)
}

func (builder *goMetadataBuilder) sliceCount(start string, count int) {
	builder.pointer(start)
	builder.u64(uint64(count))
	builder.u64(uint64(count))
}

func (builder *goMetadataBuilder) emptySlice() {
	builder.u64(0)
	builder.u64(0)
	builder.u64(0)
}
