// Package gometa emits the Go runtime's object-level metadata: the pcHeader,
// funcnametab, pctab, the `_func` records and their functab/findfunctab indexes,
// moduledata, the data/bss GC programs, and the argument and local stack maps.
//
// Everything here is architecture-neutral, which is not obvious and is worth
// recording because it is what makes one implementation serve every 64-bit
// backend. Three facts carry it:
//
//   - moduledata's layout does not depend on the architecture. Its fields are all
//     word-size-derived -- pointers, uintptrs, 3-word slice headers, 2-word
//     strings, a map word, plus one byte pair and two {int32, *uint8} bitvectors
//     -- so on any 64-bit target the record is the same 592 bytes with the same
//     field offsets. (It is 296 bytes on 32-bit targets; this package assumes
//     8-byte words throughout and is not usable on a 32-bit backend.)
//   - moduledata itself carries no architecture discriminator. The only one in
//     the whole module is one indirection away, in pcHeader.minLC and
//     pcHeader.ptrSize, which runtime.moduledataverify1 checks against
//     sys.PCQuantum and goarch.PtrSize. minLC is this package's PCQuantum
//     parameter.
//   - Go stack maps carry no register numbers. The wire format is
//     runtime.stackmap -- {n int32, nbit int32, then n byte-aligned bitmaps} --
//     indexed purely by frame word. Register roots never reach this package: the
//     backend filters them out when it builds StackMapPoint.PointerWords, because
//     a managed function spills every GC reference that is live across a
//     safepoint to a stack slot.
//
// The architecture-dependent remainder is small enough to pass in as parameters;
// see Arch. A backend that needs a private PCDATA table beyond the Go-defined
// slots supplies it through Options.ExtraPCData.
package gometa

import (
	"fmt"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Arch is the complete set of architecture-dependent parameters this package
// needs. Everything else about the emitted metadata is identical across
// backends.
type Arch struct {
	// PCQuantum is the target's minimum instruction size in bytes, written to
	// pcHeader.minLC and used as the divisor that turns a byte-valued PC delta
	// into the PC units a pc-value table encodes. AArch64 is 4; x86-64 is 1.
	PCQuantum int

	// Reloc32 and Reloc64 are the target's absolute 32- and 64-bit relocation
	// types. The metadata needs only absolute references: `_func`/functab entry
	// offsets are 32-bit, every moduledata pointer is 64-bit.
	Reloc32 uint32
	Reloc64 uint32

	// FrameBaseBytes is how much of a managed frame the saved frame record
	// occupies before the first local slot, so a local's byte offset from the
	// frame base converts to a stack-map word index as
	// (offset-FrameBaseBytes)/8. AArch64 saves {FP, LR}, so 16.
	FrameBaseBytes int
}

// Options carries the target's optional extensions to the metadata.
type Options struct {
	// ExtraPCData, when non-nil, encodes one target-private PCDATA table that is
	// appended after the Go-defined slots (so it lands at PCDATA index
	// GoPCDataSlots) and raises the `_func` record's npcdata by one. arm64 uses
	// it to describe its managed-frame outgoing area. A target that needs no
	// such table leaves this nil and emits only the Go-defined slots.
	ExtraPCData func(FunctionInfo) []byte
}

// FunctionInfo is the per-function fact set the backend's emit loop gathers and
// this package assembles into pclntab. It is the loop-to-finisher boundary, so
// it holds only finished numbers -- no IR and no machine state.
type FunctionInfo struct {
	Name       string
	FrameSize  int
	FrameStart int

	// ManagedFrame marks a function using the backend's managed Go frame, whose
	// outgoing call area sits outside the locals the stack map describes.
	ManagedFrame bool
	OutgoingSize int

	Size        uint64
	FuncID      byte
	FuncFlag    byte
	DeferReturn uint32

	LocalPointerWords []int
	StackMapPoints    []StackMapPoint

	ArgumentSize         int
	ArgumentPointerWords []int

	// NoLocalPointers suppresses the locals stack map entirely, for assembly
	// functions that declare they hold no roots.
	NoLocalPointers bool
}

// StackMapPoint is one safepoint's live frame roots, as word indexes into the
// locals area. The backend has already dropped register roots and converted
// byte offsets to word indexes, which is why nothing below needs to know the
// target's frame-base convention or register numbering.
type StackMapPoint struct {
	PC           int
	PointerWords []int
}

// StackMapIndexPoint is one safepoint's index into the function's deduplicated
// list of pointer maps, as the PCDATA_StackMapIndex table encodes it.
type StackMapIndexPoint struct {
	PC    int
	Index int
}

const (
	// GoPCDataSlots is how many PCDATA slots Go itself defines
	// (abi.PCDATA_UnsafePoint through abi.PCDATA_PanicBounds). A target's
	// Options.ExtraPCData table, if any, occupies the next index.
	GoPCDataSlots = 5

	// FuncDataSlots is how many FUNCDATA slots each `_func` record carries:
	// abi.FUNCDATA_ArgsPointerMaps and abi.FUNCDATA_LocalsPointerMaps.
	FuncDataSlots = 2
)

const (
	funcTabBucketSize        = 4096
	minimumFindFuncBucketCap = 131072
)

// Go's runtime special-cases a handful of functions by abi.FuncID during
// traceback and panic handling. The names are the mangled spellings of the
// runtime functions cg12 emits.
func RuntimeFunctionID(name string) byte {
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

// abi.FuncFlag bits.
const (
	FuncFlagTopFrame = 1
	FuncFlagSPWrite  = 2
	FuncFlagAsm      = 4
)

const ModuleInitTasksName = ".goc.module.inittasks"
const ModuleItabLinksName = ".goc.module.itablinks"

func ModuleInitTaskCount(module *ir.Module) int {
	for _, data := range module.Data {
		if data.Name == ModuleInitTasksName {
			return len(data.Items)
		}
	}
	return 0
}

func ModuleItabLinkCount(module *ir.Module) int {
	for _, data := range module.Data {
		if data.Name == ModuleItabLinksName {
			return len(data.Items)
		}
	}
	return 0
}

// ModuleUsesGoRuntime reports whether the module is a Go module needing this
// metadata at all. The flag is set by the frontend and serialized in the unit
// format, so it survives a round trip through the module cache.
func ModuleUsesGoRuntime(module *ir.Module) bool {
	return module.Runtime
}

func DataSymbolValue(object *obj.Object, name string) (uint64, bool) {
	symbol, ok := DataSymbol(object, name)
	return symbol.Value, ok
}

func DataSymbol(object *obj.Object, name string) (obj.Sym, bool) {
	symbol, ok := ObjectSymbol(object, name)
	if !ok || (symbol.Section != obj.SecData && symbol.Section != obj.SecBss) {
		return obj.Sym{}, false
	}
	return symbol, true
}

func ObjectSymbol(object *obj.Object, name string) (obj.Sym, bool) {
	for _, symbol := range object.Syms {
		if symbol.Name == name {
			return symbol, true
		}
	}
	return obj.Sym{}, false
}

func sortFunctionsByTextOffset(object *obj.Object, functions []FunctionInfo) []FunctionInfo {
	offsets := make(map[string]uint64, len(functions))
	for _, symbol := range object.Syms {
		if symbol.Section == obj.SecText && symbol.Func {
			offsets[symbol.Name] = symbol.Value
		}
	}
	sorted := append([]FunctionInfo(nil), functions...)
	sort.SliceStable(sorted, func(left, right int) bool {
		leftOffset, leftFound := offsets[sorted[left].Name]
		rightOffset, rightFound := offsets[sorted[right].Name]
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

func findFuncBucketCount(object *obj.Object, functions []FunctionInfo, endSymbol string) int {
	if len(functions) == 0 {
		return minimumFindFuncBucketCap
	}
	offsets := make(map[string]uint64, len(functions)+1)
	for _, symbol := range object.Syms {
		if symbol.Section == obj.SecText && symbol.Func {
			offsets[symbol.Name] = symbol.Value
		}
	}
	minPC, minFound := offsets[functions[0].Name]
	endPC, endFound := offsets[endSymbol]
	if !minFound || !endFound || endPC <= minPC {
		return minimumFindFuncBucketCap
	}
	buckets := int((endPC-minPC)/funcTabBucketSize) + 1
	if buckets < minimumFindFuncBucketCap {
		return minimumFindFuncBucketCap
	}
	return buckets
}

// GCProgram encodes the pointer bitmap of the [dataStart, dataEnd) range as a
// runtime GC program, for moduledata's gcdata and gcbss. Only word-size
// arithmetic is involved, and the program's opcode encoding has no
// architecture-dependent part.
func GCProgram(dataStart, dataEnd uint64, pointerOffsets []uint64) ([]byte, error) {
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

// FunctionStackMaps builds a function's deduplicated pointer maps and the
// per-safepoint indexes into them.
//
// Map 0 is empty and map 1 is the function's conservative body map: every
// pointer-bearing word the frame ever uses, which is also the set the prologue
// zeroes. The function selects map 0 while it is still in its stack-growth
// prologue and map 1 once the frame exists, because the temporary spills
// morestack uses at entry are not valid roots during ordinary execution. No
// safepoint falls between the frame being built and the first call, so map 1
// describes a window a scan cannot observe; it is kept conservative rather than
// empty so that window stays safe if one ever appears.
//
// Every safepoint then gets exactly the roots live there, deduplicated so
// identical maps share an index. It must NOT be the union with map 1: a local
// whose last use has passed is not a root, and unioning the body map in would
// report every pointer the frame ever held as live at every call for as long as
// the frame exists. That is an unbounded leak in any long-running frame, and it
// is what kept an object registered by an inlined helper alive forever so its
// cleanup and finalizer never ran (RUNTIME_PLAN.md 5.3).
//
// This is pure set algebra over word indexes -- see the package comment on why
// no register numbers ever appear here.
func FunctionStackMaps(function FunctionInfo) ([][]int, []StackMapIndexPoint) {
	pointerMaps := [][]int{nil, normalizePointerWords(function.LocalPointerWords)}
	indexPoints := make([]StackMapIndexPoint, 0, len(function.StackMapPoints))
	for _, point := range function.StackMapPoints {
		pointerWords := normalizePointerWords(point.PointerWords)
		index := pointerMapIndex(pointerMaps, pointerWords)
		if index < 0 {
			index = len(pointerMaps)
			pointerMaps = append(pointerMaps, pointerWords)
		}
		indexPoints = append(indexPoints, StackMapIndexPoint{PC: point.PC, Index: index})
	}
	return pointerMaps, indexPoints
}

// normalizePointerWords returns a sorted, duplicate-free copy, which is the form
// pointerMapIndex compares against.
func normalizePointerWords(words []int) []int {
	if len(words) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(words))
	for _, word := range words {
		seen[word] = true
	}
	normalized := make([]int, 0, len(seen))
	for word := range seen {
		normalized = append(normalized, word)
	}
	sort.Ints(normalized)
	return normalized
}

func pointerMapIndex(pointerMaps [][]int, candidate []int) int {
	for index, pointerMap := range pointerMaps {
		if len(pointerMap) != len(candidate) {
			continue
		}
		equal := true
		for word := range pointerMap {
			if pointerMap[word] != candidate[word] {
				equal = false
				break
			}
		}
		if equal {
			return index
		}
	}
	return -1
}

// LocalStackMapWords is how many frame words the locals stack map must cover.
// The saved frame record at the base of the frame is not described, and neither
// is the outgoing call area of a managed frame, which lies outside the locals.
func (arch Arch) LocalStackMapWords(function FunctionInfo) int {
	if function.NoLocalPointers {
		return 0
	}
	frameSize := function.FrameSize
	if function.ManagedFrame {
		frameSize -= function.OutgoingSize
	}
	words := (frameSize - arch.FrameBaseBytes) / 8
	if words < 0 {
		return 0
	}
	return words
}
