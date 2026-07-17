package arm64

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

type goFunctionInfo struct {
	name                 string
	frameSize            int
	frameStart           int
	size                 uint64
	funcID               byte
	funcFlag             byte
	deferReturn          uint32
	pointerWords         []int
	argumentSize         int
	argumentPointerWords []int
}

func goRuntimeFunctionID(name string) byte {
	if strings.Contains(name, "_deferwrap_") || strings.Contains(name, "_gowrap_") || strings.Contains(name, "_methodvalue_") {
		return 23
	}
	switch name {
	case "runtime_gopanic":
		return 10
	case "runtime_main":
		return 17
	case "runtime_sigpanic":
		return 20
	default:
		return 0
	}
}

const (
	goFuncFlagTopFrame = 1
	goFuncFlagSPWrite  = 2
	goFuncFlagAsm      = 4
)

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

func addGoRuntimeObjectMetadata(object *obj.Object, functions, translatedFunctions []goFunctionInfo, moduledata *ir.Data, pointerOffsets []uint64, moduleInitTaskCount int) error {
	if len(functions) == 0 && len(translatedFunctions) == 0 {
		return addData(object, moduledata)
	}
	functions = sortGoFunctionsByTextOffset(object, functions)
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
	builder.build(functions, translatedFunctions, moduledata, gcProgram, noptrBSSName, noptrBSSSize, moduleInitTaskCount)
	object.Data = append(object.Data, builder.data...)
	object.DataRelocs = append(object.DataRelocs, builder.relocs...)
	return nil
}

func sortGoFunctionsByTextOffset(object *obj.Object, functions []goFunctionInfo) []goFunctionInfo {
	offsets := make(map[string]uint64, len(functions))
	for _, symbol := range object.Syms {
		if symbol.Section == obj.SecText && symbol.Func {
			offsets[symbol.Name] = symbol.Value
		}
	}
	sorted := append([]goFunctionInfo(nil), functions...)
	sort.SliceStable(sorted, func(left, right int) bool {
		leftOffset, leftFound := offsets[sorted[left].name]
		rightOffset, rightFound := offsets[sorted[right].name]
		if leftFound != rightFound {
			return leftFound
		}
		if !leftFound {
			return false
		}
		return leftOffset < rightOffset
	})
	return sorted
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

func (builder *goMetadataBuilder) build(functions, translatedFunctions []goFunctionInfo, moduledata *ir.Data, gcProgram []byte, noptrBSSName string, noptrBSSSize uint64, moduleInitTaskCount int) {
	const findFuncBuckets = 4096
	functions = append(functions, translatedFunctions...)

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
	unsafePointOffsets := make([]uint32, len(functions))
	for index := range functions {
		unsafePointOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
		builder.data = append(builder.data, goUnsafePointPCData()...)
	}
	stackMapOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		stackMapOffsets[index] = uint32(builder.offset(".goc.go.pctab"))
		builder.data = append(builder.data, goStackMapPCData(function.frameStart)...)
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
		words := (function.argumentSize + 7) / 8
		builder.stackMaps(words, function.argumentPointerWords, nil)
	}
	localOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		builder.align(4)
		localOffsets[index] = uint32(builder.position() - builder.labels[".goc.go.gofunc"])
		words := (function.frameSize - 16) / 8
		if words < 0 {
			words = 0
		}
		builder.stackMaps(words, nil, function.pointerWords)
	}
	builder.label(".goc.go.gofunc.end")
	builder.align(4)

	builder.label(".goc.go.pclntable")
	functionOffsets := make([]uint32, len(functions))
	for index, function := range functions {
		functionOffsets[index] = uint32(builder.offset(".goc.go.pclntable"))
		builder.reloc32(function.name)
		builder.u32(nameOffsets[index])
		builder.u32(uint32(function.argumentSize))
		builder.u32(function.deferReturn)
		builder.u32(pcspOffsets[index])
		builder.u32(0)
		builder.u32(0)
		builder.u32(2)
		builder.u32(0)
		builder.u32(0)
		builder.bytes(function.funcID, function.funcFlag, 0, 2)
		builder.u32(unsafePointOffsets[index])
		builder.u32(stackMapOffsets[index])
		builder.u32(argumentOffsets[index])
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
	appendVarint := func(value int) {
		encoded := uint32(value << 1)
		if value < 0 {
			encoded = ^encoded
		}
		appendUvarint(encoded)
	}

	type point struct {
		pc    int
		value int
	}
	points := make([]point, 0, 2)
	if frameStart > 0 {
		points = append(points, point{pc: 0, value: 0})
	}
	points = append(points, point{pc: frameStart, value: frameSize})

	value := -1
	for index, point := range points {
		appendVarint(point.value - value)
		value = point.value
		if index+1 < len(points) {
			appendUvarint(uint32((points[index+1].pc - point.pc) / 4))
		} else {
			appendUvarint(^uint32(0))
		}
	}
	appendUvarint(0) // end of this function's pc-value table
	return data
}

// goUnsafePointPCData marks the complete generated function as unsafe for
// asynchronous preemption. cg12 keeps managed references in registers between
// calls, while its Go stack maps describe the spill state at call safepoints.
// Cooperative preemption at calls remains available.
func goUnsafePointPCData() []byte {
	return []byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}
}

// goStackMapPCData selects the entry maps while the function is in its stack
// growth prologue, then switches to the body maps once the frame exists. The
// entry argument map describes the temporary register spills used by
// morestack. Those spill slots are not valid roots during ordinary execution.
func goStackMapPCData(frameStart int) []byte {
	var data []byte
	appendUvarint := func(value uint32) {
		for value >= 0x80 {
			data = append(data, byte(value)|0x80)
			value >>= 7
		}
		data = append(data, byte(value))
	}
	appendVarint := func(value int) {
		encoded := uint32(value << 1)
		if value < 0 {
			encoded = ^encoded
		}
		appendUvarint(encoded)
	}

	value := -1
	if frameStart > 0 {
		appendVarint(-value)
		value = 0
		appendUvarint(uint32(frameStart / 4))
	}
	appendVarint(1 - value)
	appendUvarint(^uint32(0))
	appendUvarint(0)
	return data
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

func (builder *goMetadataBuilder) stackMaps(words int, pointerWordMaps ...[]int) {
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
