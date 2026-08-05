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
	"encoding/binary"
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

	// ArgumentSize is the byte size of the argument frame the caller reserves for
	// this function: its stack-passed arguments and stack results, followed by the
	// home slots the stack-growth prologue spills register arguments into.
	ArgumentSize int

	// ArgumentPointerWords is the argument frame's pointer map in the entry
	// window, which is the stack-growth prologue and therefore the call to
	// morestack. Every pointer word the frame can hold belongs here: the caller
	// has written the stack-passed arguments, and the prologue has just spilled
	// the register arguments to their home slots.
	ArgumentPointerWords []int

	// SafepointArgumentPointerWords is the argument frame's pointer map at every
	// safepoint in the body. It is the subset of ArgumentPointerWords the caller
	// initialised -- the stack-passed arguments -- and excludes the register home
	// slots, which the prologue writes only on the path that calls morestack and
	// which hold whatever the caller's stack held everywhere else. Scanning those
	// as roots would treat uninitialised stack memory as pointers.
	SafepointArgumentPointerWords []int

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

// ChainModule records the relocation that links the moduledata named parent onto
// the one named child, so runtime.modulesinit walks both.
//
// That is the whole of what joining a second module to an image takes: one
// absolute 64-bit data relocation into moduledata.next. Everything else the
// second module needs -- its own type region, its own pclntab, its own text
// bounds -- travels inside the module's own object, which is the point of
// per-module regions.
func ChainModule(object *obj.Object, parent, child string, reloc64 uint32) error {
	if _, found := ObjectSymbol(object, child); !found {
		return fmt.Errorf("chain Go module: %s is not defined in this object", child)
	}
	return ChainModuleToExternal(object, parent, child, reloc64)
}

// ChainModuleToExternal is ChainModule for a child module that lives in a
// different object, which is the shape the driver split produces: the prebuilt
// runtime object is written before the program that will be linked against it
// even exists, so it names its successor rather than pointing at it.
//
// The relocation is the ordinary absolute one either way. All that changes is
// who resolves it: the system linker rather than this object's own writer. The
// child's name is therefore a contract between the two compilations, and the
// link fails loudly if the program does not define it.
func ChainModuleToExternal(object *obj.Object, parent, child string, reloc64 uint32) error {
	symbol, found := DataSymbol(object, parent)
	if !found {
		return fmt.Errorf("chain Go module: %s is not a data symbol of this object", parent)
	}
	object.DataRelocs = append(object.DataRelocs, obj.Reloc{
		Offset: symbol.Value + ModuleNextOffset, Sym: child, Type: reloc64,
	})
	return nil
}

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

// findFuncBucketCount sizes moduledata.findfunctab.
//
// runtime.findfunc indexes it with the PC's offset from the module's minpc,
// divided by the bucket size, and runtime.findmoduledatap has already confirmed
// the PC is inside [minpc, maxpc). So (maxpc-minpc)/bucket + 1 entries is exactly
// enough -- when the module's span is known here.
//
// It usually is not. A module whose last text is the translated Plan 9 sidecar
// ends in a *different* object, so this one cannot see where; the 512 MB-covering
// floor is the safety net for that. But a module bounded entirely by its own
// object -- which is what a program module compiled against a prebuilt runtime is
// -- knows its span exactly, and paying the floor there costs 2.6 MB of zeroes in
// every image.
func findFuncBucketCount(object *obj.Object, functions []FunctionInfo, endSymbol string) int {
	if len(functions) == 0 {
		return minimumFindFuncBucketCap
	}
	offsets := textOffsets(object)
	minPC, minFound := offsets[functions[0].Name]
	endPC, endFound := offsets[endSymbol]
	if !minFound || !endFound || endPC <= minPC {
		return minimumFindFuncBucketCap
	}
	return int((endPC-minPC)/funcTabBucketSize) + 1
}

// textOffsets is every text symbol this object defines, by name, at its offset
// within the object's single .text section.
func textOffsets(object *obj.Object) map[string]uint64 {
	offsets := make(map[string]uint64, len(object.Syms))
	for _, symbol := range object.Syms {
		if symbol.Section == obj.SecText && symbol.Func {
			offsets[symbol.Name] = symbol.Value
		}
	}
	return offsets
}

const (
	// findFuncSubbuckets and findFuncSubbucketSize divide each bucket, matching
	// runtime.findfuncbucket's [16]byte subbuckets.
	findFuncSubbuckets    = 16
	findFuncSubbucketSize = funcTabBucketSize / findFuncSubbuckets

	// findFuncBucketBytes is sizeof(runtime.findfuncbucket): a uint32 base plus
	// the sixteen byte-sized deltas.
	findFuncBucketBytes = 4 + findFuncSubbuckets
)

// FindFuncTab builds moduledata.findfunctab: the table runtime.findfunc uses to
// start its functab scan near the answer instead of at index 0.
//
// The runtime indexes it by the PC's distance above moduledata.minpc, in
// 4096-byte buckets of sixteen 256-byte subbuckets, and reads
//
//	idx := bucket.idx + uint32(bucket.subbuckets[sub])
//	for ftab[idx+1].entryoff <= pcOff { idx++ }
//
// so the stored index is a *lower bound* on the answer, and the error is
// one-sided: an index below the true one only costs scan steps, while an index
// above it returns the wrong function. Every choice below keeps to the low side
// -- the clamp when a delta will not fit in a byte, the fill for a subbucket no
// function starts in, and the buckets past the last function this object
// defines.
//
// Leaving it zero, as cg12 did before, is the extreme of that safe side: every
// lookup scans functab from index 0. That is O(functions in the image) per call,
// which is 5,208 iterations in a hello-world-sized program, and
// runtime.newproc1 and runtime.gdestroy each call findfunc once per goroutine
// through isSystemGoroutine.
//
// The offsets are this object's, and moduledata.minpc is functions[0]; the two
// agree at run time because a single .text input section is placed contiguously,
// so every function in it moves by one common base. Functions this object does
// not define -- on arm64 the translated Plan 9 assembly, which is linked as a
// second object after this one -- have no offset here. They sort last, and the
// buckets past the last known function hold that function's index, which is a
// lower bound for every PC above it. runtime.moduledataverify1 rejects a functab
// that is not in ascending entry order, so that ordering is checked at startup
// rather than assumed.
func FindFuncTab(object *obj.Object, functions []FunctionInfo, endSymbol string, buckets int) []byte {
	table := make([]byte, buckets*findFuncBucketBytes)
	if buckets == 0 || len(functions) == 0 {
		return table
	}
	offsets := textOffsets(object)
	minPC, found := offsets[functions[0].Name]
	if !found {
		return table
	}

	// The prefix of functions this object defines, in ascending text order.
	// sortFunctionsByTextOffset has already put them first; a prefix that is not
	// ascending would mean the two disagree, so give up rather than guess.
	starts := make([]uint64, 0, len(functions))
	for _, function := range functions {
		offset, ok := offsets[function.Name]
		if !ok {
			break
		}
		if len(starts) > 0 && offset < starts[len(starts)-1] {
			return table
		}
		starts = append(starts, offset)
	}

	// Where the known run ends. When this object defines every function, the
	// module's text-end symbol says exactly; otherwise the last known function
	// covers only the subbucket it starts in, and the constant fill takes over
	// from there.
	knownEnd := starts[len(starts)-1] + 1
	if len(starts) == len(functions) {
		if end, ok := offsets[endSymbol]; ok && end > starts[len(starts)-1] {
			knownEnd = end
		}
	}

	// Only the buckets the known run reaches are computed per subbucket; the
	// rest take the constant record below.
	filled := int((knownEnd-1-minPC)/(findFuncSubbucketSize*findFuncSubbuckets)) + 1
	if filled > buckets {
		filled = buckets
	}
	indexes := make([]int32, filled*findFuncSubbuckets)
	for i := range indexes {
		indexes[i] = -1
	}
	mark := func(subbucket int, index int32) {
		if subbucket < 0 || subbucket >= len(indexes) {
			return
		}
		if indexes[subbucket] < 0 || indexes[subbucket] > index {
			indexes[subbucket] = index
		}
	}
	for i, start := range starts {
		end := knownEnd
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if end <= start {
			end = start + 1
		}
		for p := start; p < end; p += findFuncSubbucketSize {
			mark(int((p-minPC)/findFuncSubbucketSize), int32(i))
		}
		mark(int((end-1-minPC)/findFuncSubbucketSize), int32(i))
	}

	// A subbucket no function starts in belongs to whichever function was still
	// running when it began, which is the previous subbucket's answer.
	previous := int32(0)
	for i := range indexes {
		if indexes[i] < 0 {
			indexes[i] = previous
		}
		previous = indexes[i]
	}

	for bucket := 0; bucket < buckets; bucket++ {
		record := table[bucket*findFuncBucketBytes:]
		if bucket >= filled {
			binary.LittleEndian.PutUint32(record, uint32(previous))
			continue
		}
		base := indexes[bucket*findFuncSubbuckets]
		binary.LittleEndian.PutUint32(record, uint32(base))
		for sub := 0; sub < findFuncSubbuckets; sub++ {
			delta := indexes[bucket*findFuncSubbuckets+sub] - base
			// A bucket spanning more than 256 functions cannot state its last
			// ones exactly. 255 is still below every one of them, so the lookup
			// stays correct and pays a few scan steps.
			if delta > 255 {
				delta = 255
			}
			record[4+sub] = byte(delta)
		}
	}
	return table
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

// EntryStackMapIndex is the map the runtime uses when a frame's pc is exactly
// the function's entry: runtime.stkframe.getStackMap short-circuits the
// PCDATA_StackMapIndex lookup there and hardcodes index 0. Nothing in the
// function has executed at that pc, so this map may describe only the words the
// *caller* wrote -- the stack-passed arguments -- and never a callee-written
// word such as a register home slot. See BodyStackMapIndex and
// FunctionStackMaps for the other reserved indexes.
const EntryStackMapIndex = 0

// BodyStackMapIndex is the conservative whole-frame map selected once the frame
// exists and before the first safepoint.
const BodyStackMapIndex = 1

// FunctionStackMaps builds a function's deduplicated pointer maps, the
// per-safepoint indexes into them, and the index that describes the
// stack-growth prologue window.
//
// Map 0 is empty and map 1 is the function's conservative body map: every
// pointer-bearing word the frame ever uses, which is also the set the prologue
// zeroes. The function selects map 1 once the frame exists, because the
// temporary spills morestack uses at entry are not valid roots during ordinary
// execution. No safepoint falls between the frame being built and the first
// call, so map 1 describes a window a scan cannot observe; it is kept
// conservative rather than empty so that window stays safe if one ever appears.
//
// Every safepoint then gets exactly the roots live there, deduplicated so
// identical maps share an index. It must NOT be the union with map 1: a local
// whose last use has passed is not a root, and unioning the body map in would
// report every pointer the frame ever held as live at every call for as long as
// the frame exists. That is an unbounded leak in any long-running frame, and it
// is what kept an object registered by an inlined helper alive forever so its
// cleanup and finalizer never ran (RUNTIME_PLAN.md 5.3).
//
// Index 0 is reserved for the entry window and a safepoint never resolves to it,
// even when the safepoint holds no roots at all and its locals map would be
// identical. The runtime reads one PCDATA_StackMapIndex value for both the
// locals and the argument maps (runtime.stkframe.getStackMap), so a body
// safepoint sharing index 0 would inherit the entry window's argument map. A
// rootless safepoint therefore gets its own empty map instead.
//
// The growth index is the last map, appended only when the entry and safepoint
// argument maps differ -- that is, when the function has register home slots
// that only the stack-growth prologue writes. Those slots are valid roots for
// exactly the window between the prologue's spill and its reload around
// morestack, and that window has to be a *different* index from the one the
// runtime uses at pc == entry, because a goroutine created by runtime.newproc
// and not yet scheduled is stopped at pc == entry with the whole argument frame
// still holding whatever the previous user of that recycled stack left there
// (RUNTIME_PLAN.md 5.11).
//
// This is pure set algebra over word indexes -- see the package comment on why
// no register numbers ever appear here.
func FunctionStackMaps(function FunctionInfo) ([][]int, []StackMapIndexPoint, int) {
	pointerMaps := [][]int{nil, normalizePointerWords(function.LocalPointerWords)}
	indexPoints := make([]StackMapIndexPoint, 0, len(function.StackMapPoints))
	for _, point := range function.StackMapPoints {
		pointerWords := normalizePointerWords(point.PointerWords)
		index := pointerMapIndex(pointerMaps[1:], pointerWords)
		if index < 0 {
			index = len(pointerMaps)
			pointerMaps = append(pointerMaps, pointerWords)
		} else {
			index++ // pointerMapIndex searched from map 1
		}
		indexPoints = append(indexPoints, StackMapIndexPoint{PC: point.PC, Index: index})
	}

	growthIndex := EntryStackMapIndex
	if !samePointerWords(function.ArgumentPointerWords, function.SafepointArgumentPointerWords) {
		// The prologue window has no frame yet, so it holds no locals; only its
		// argument map differs from the entry window's.
		growthIndex = len(pointerMaps)
		pointerMaps = append(pointerMaps, nil)
	}
	return pointerMaps, indexPoints, growthIndex
}

// ArgumentStackMaps returns one argument pointer map per locals stack-map entry,
// which is what keeps the two tables indexable by the same
// PCDATA_StackMapIndex value.
//
// The growth index gets the full argument map, because the stack-growth prologue
// has just spilled the register arguments into their home slots there. Every
// other index -- including the entry index the runtime hardcodes at pc == entry
// -- gets the caller-initialised subset. Writing the argument map at one index
// alone, which left every other index with an all-zero argument bitmap, hid the
// caller's stack-passed pointer arguments from the collector for the whole call.
func ArgumentStackMaps(function FunctionInfo, entries int, growthIndex int) [][]int {
	argumentMaps := make([][]int, entries)
	for index := range argumentMaps {
		if index == growthIndex {
			argumentMaps[index] = function.ArgumentPointerWords
			continue
		}
		argumentMaps[index] = function.SafepointArgumentPointerWords
	}
	return argumentMaps
}

// samePointerWords reports whether two already-sorted word lists describe the
// same set. Both argument maps come from argumentPointerWords, which sorts.
func samePointerWords(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
