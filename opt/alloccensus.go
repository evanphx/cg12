package opt

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// heapAllocators are the runtime entry points that return newly allocated heap
// storage for a typed Go object, keyed by the argument index of the type
// descriptor (or -1 when the entry point takes none).
//
// Only allocators whose placement is a decision are listed. runtime.growslice is
// deliberately absent: it reallocates storage that already exists, no escape
// decision chooses it, and the front end emits it without a source position, so
// including it would add a hundred positionless lines to the census that no
// change to the escape analysis could ever move.
var heapAllocators = map[string]int{
	"runtime.newobject":     1,
	"runtime.newarray":      1,
	"runtime.makeslice":     1,
	"runtime.makeslice64":   1,
	"runtime.makechan":      1,
	"runtime.makechan64":    1,
	"runtime.makemap":       1,
	"runtime.makemap64":     1,
	"runtime.makemap_small": -1,
}

// conversionHelpers are the runtime entry points LowerHeapAllocations calls in
// place of an allocator when a candidate that leaves the frame is an interface
// payload it can build directly -- see ir.Block.HeapAllocConverted.
//
// They are counted as heap placements here, and are listed separately from
// heapAllocators for two reasons. The first is mechanical: the call the pass
// emits carries the value rather than a type descriptor, so the type has to come
// from the AllocDecision instead of being read back out of the IR.
//
// The second is what a reader of this file needs to know. A conversion helper
// allocates only for a value the runtime's static table cannot hold: the same
// site is an allocation for one value and free for the next, and neither
// "frame" nor "heap" is true of it on its own. The placement recorded is "heap"
// because that is the escape decision -- the payload did not stay in the frame,
// which is exactly what gc's -m reports at the same source line -- and the
// allocator column names the helper, so the nuance is on the line rather than
// lost. Dropping these sites instead was measured: it removed 552 rows and moved
// 33 corpus lines into the gc differential's permissive set, which is the set
// that is supposed to mean "goc framed something gc could not".
var conversionHelpers = map[string]bool{
	"runtime.convT16": true,
	"runtime.convT32": true,
	"runtime.convT64": true,
}

// Allocation is one allocation site in a compiled module together with where
// the object it allocates was placed.
//
// A site is identified by its source position, the IR function containing it,
// the allocator involved and the type allocated. Placement is deliberately not
// part of that identity: the whole point of the census is to notice that a site
// which used to be one thing is now the other, and that is only expressible if
// the site keeps its name across the move.
type Allocation struct {
	// Func is the IR function the allocation happens in, after inlining.
	Func string
	// Pos is the source position of the allocating instruction, when the front
	// end recorded one.
	Pos ir.SrcPos
	// File is Pos's file name, resolved against the module's file table.
	File string
	// Allocator names the runtime allocator a heap placement calls, or that a
	// frame placement avoided calling.
	Allocator string
	// Type is the allocated type, as the type-descriptor symbol names it with
	// the compiler's prefix and content hash removed.
	Type string
	// Placement is where the object landed.
	Placement ir.AllocPlacement
}

// Site is the allocation's identity without its placement: the thing that stays
// the same when an object moves between the frame and the heap.
func (allocation Allocation) Site() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s", allocation.location(), allocation.Func, allocation.Allocator, allocation.Type)
}

// Key is Site plus the placement. It is one line of the census.
func (allocation Allocation) Key() string {
	return allocation.Site() + "\t" + string(allocation.Placement)
}

// String renders one allocation as a sentence for a diagnostic.
func (allocation Allocation) String() string {
	return fmt.Sprintf("%s: %s: %s %s on the %s", allocation.location(), allocation.Func, allocation.Allocator, allocation.Type, allocation.Placement)
}

func (allocation Allocation) location() string {
	if !allocation.Pos.Valid() {
		return "?"
	}
	file := allocation.File
	if file == "" {
		file = "?"
	}
	return fmt.Sprintf("%s:%d:%d", file, allocation.Pos.Line, allocation.Pos.Col)
}

// AllocationCensus reports where every allocation in module landed: one record
// per distinct site, sorted, with no duplicates.
//
// It is the reviewable form of the escape decision. FrameEscapes answers "is any
// frame address published past its frame", which is a correctness question with
// a yes/no answer; this answers "which objects are on the heap", which has no
// answer that is right in isolation. Moving an object to the heap costs an
// allocation and a collector's attention; moving one to the frame is how
// ccwork/escape-analysis's 2724ac7 put a dead frame address into a live heap
// object. Neither direction is safe to accept without someone looking, and
// neither is visible in a pass/fail test, so the census exists to be diffed.
//
// The two halves come from different places because the finished IR only
// remembers one of them:
//
//   - Heap placements are read out of the IR. Every one of them is a call to a
//     named runtime allocator carrying the type descriptor as an argument, and
//     that is true whether the front end decided the object escaped or
//     LowerHeapAllocations lowered a candidate to the same call. The census
//     cannot tell those apart and should not: the object is on the heap either
//     way.
//
//   - Frame placements come from module.AllocDecisions, which LowerHeapAllocations
//     writes as it rewrites. They cannot be read back out: a promoted candidate
//     becomes an OAlloc holding a byte size, with its type gone and nothing to
//     distinguish it from the thousands of ordinary local slots around it.
//
// The census therefore covers every heap allocation and every frame allocation
// that came from an escape decision. It does not cover ordinary front-end frame
// slots, which are not decisions and outnumber everything else here by two
// orders of magnitude.
//
// It does cover the frame slots the front end chose deliberately, when
// AllocationCensusOptions.IncludeFrontEndFrameSlots asks for them. Those are not
// ordinary local slots -- see there for why the distinction matters and what
// leaving them out cost.
func AllocationCensus(module *ir.Module) []Allocation {
	return AllocationCensusWith(module, AllocationCensusOptions{})
}

// AllocationCensusOptions selects what AllocationCensusWith counts.
type AllocationCensusOptions struct {
	// IncludeFrontEndFrameSlots adds the frame placements goc's AST walk made
	// itself: ir.Func.PlacedAllocs with placement "frame".
	//
	// Without it the census sees only one side of a front-end frame slot. The
	// heap side of such a site is an allocator call and is read out of the IR, so
	// an object that moves from a front-end frame slot to the heap has no line
	// before and a heap line after -- which the reporter files under "appeared",
	// a bucket that means "new code or a new allocation", when the truth is
	// "frame -> heap". Measured over the whole corpus against a run that made
	// every front-end escape predicate answer "escapes": 451 moves, all 451
	// reported as "appeared", and the frame-to-heap bucket empty. With this
	// option, 317 of them are named "frame -> heap" and the rest are local
	// variable slots, which are the ordinary kind and still not recorded.
	// CCWORK_REPORT.md, "Part 2", has the tables.
	//
	// These are not the ordinary front-end frame slots the census leaves out.
	// Those are local variables' storage: no type, no allocator, no decision, and
	// tens of thousands per program. These are the sites where a source-level
	// escape predicate chose a frame over a call, each carrying the type it
	// placed and the allocator it declined -- which is what lets the frame record
	// and the heap record of one site share an identity, so the move is one line
	// changing rather than two lines swapping.
	//
	// A front-end frame placement with no allocator is still left out, because
	// there is nothing for it to pair with: the string-conversion buffer's heap
	// form is a nil argument and an allocation inside the runtime helper, which
	// is not a census site on either side.
	IncludeFrontEndFrameSlots bool
}

// AllocationCensusWith is AllocationCensus with the counting rules stated
// explicitly. See AllocationCensus for what a census is and AllocationCensusOptions
// for what the options change.
func AllocationCensusWith(module *ir.Module, options AllocationCensusOptions) []Allocation {
	seen := make(map[string]Allocation)
	record := func(allocation Allocation) {
		if _, exists := seen[allocation.Key()]; !exists {
			seen[allocation.Key()] = allocation
		}
	}

	for _, decision := range module.AllocDecisions {
		if decision.Placement != ir.AllocInFrame && !conversionHelpers[decision.Allocator] {
			// Heap placements are read out of the finished IR below, so that a
			// candidate lowered to a call and a call the front end emitted itself
			// produce the same record rather than two spellings of one site. A
			// conversion helper is the exception: its call carries the value
			// instead of a type descriptor, so this is the only place the type
			// still exists.
			continue
		}
		record(Allocation{
			Func:      decision.Func,
			Pos:       decision.Pos,
			File:      module.FileName(decision.Pos.File),
			Allocator: decision.Allocator,
			Type:      AllocationTypeName(decision.Type),
			Placement: decision.Placement,
		})
	}

	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
					continue
				}
				callee := constSymbolName(function, instruction.Args[0])
				descriptorIndex, allocates := heapAllocators[callee]
				if !allocates {
					continue
				}
				descriptor := ""
				if descriptorIndex >= 0 && descriptorIndex < len(instruction.Args) {
					descriptor = constSymbolName(function, instruction.Args[descriptorIndex])
				}
				record(Allocation{
					Func:      function.Name,
					Pos:       instruction.Pos,
					File:      module.FileName(instruction.Pos.File),
					Allocator: callee,
					Type:      AllocationTypeName(descriptor),
					Placement: ir.AllocOnHeap,
				})
			}
		}
	}

	if options.IncludeFrontEndFrameSlots {
		for _, function := range module.Funcs {
			if function.Start == nil || len(function.PlacedAllocs) == 0 {
				continue
			}
			positions := allocationDefinitions(function, function.PlacedAllocs)
			for _, id := range sortedAllocationIDs(function.PlacedAllocs) {
				placed := function.PlacedAllocs[id]
				if placed.Placement != ir.AllocInFrame {
					// A front-end heap placement is an allocator call and was read
					// out of the IR above, where it is one record whether the front
					// end or the IR pass put it there.
					continue
				}
				if _, counted := heapAllocators[placed.Allocator]; !counted {
					// No allocator, or one the census does not count: there is no
					// heap record this frame record could ever pair with, so a line
					// for it could not express a direction either.
					continue
				}
				position := positions[id]
				record(Allocation{
					Func:      function.Name,
					Pos:       position,
					File:      module.FileName(position.File),
					Allocator: placed.Allocator,
					Type:      AllocationTypeName(placed.Type),
					Placement: ir.AllocInFrame,
				})
			}
		}
	}

	census := make([]Allocation, 0, len(seen))
	for _, allocation := range seen {
		census = append(census, allocation)
	}
	sort.Slice(census, func(i, j int) bool { return census[i].Key() < census[j].Key() })
	return census
}

// sortedAllocationIDs orders a PlacedAllocs map's keys, so that a census built
// from it does not depend on Go's map iteration order. The result is
// deduplicated by key and sorted before it is returned, so the order only
// decides which of two identical records is kept -- but a census whose
// determinism rests on "the duplicates happened to be identical" is one
// unexamined field away from being non-reproducible.
func sortedAllocationIDs(placed map[uint32]ir.PlacedAlloc) []uint32 {
	ids := make([]uint32, 0, len(placed))
	for id := range placed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// typeSymbolHash matches the content hash contentSymbolName appends to an
// interned type-descriptor symbol.
var typeSymbolHash = regexp.MustCompile(`\.[0-9a-f]{16}$`)

// typeSymbolPrefixes are the interned-symbol prefixes goc gives a runtime type
// descriptor: one for the descriptor a `new` or `make` passes to the allocator,
// one for the reflect-visible type record.
var typeSymbolPrefixes = []string{".goc.runtime.type.", ".goc.type."}

// AllocationTypeName renders a type-descriptor symbol as the type it describes:
// the compiler's prefix and its trailing content hash removed.
//
// The hash goes because it is a digest of the type's full definition, so leaving
// it in would rewrite every census line mentioning a struct whenever a field of
// that struct changed -- churn that has nothing to do with where anything is
// allocated, in a file whose only purpose is to be read as a diff. What is left
// is the readable name contentSymbolName built, which is truncated at 48
// characters and so is not unique; that is acceptable because a site is
// identified by its position and function as well.
func AllocationTypeName(symbol string) string {
	if symbol == "" {
		return "-"
	}
	name := symbol
	for _, prefix := range typeSymbolPrefixes {
		if trimmed := strings.TrimPrefix(name, prefix); trimmed != name {
			name = trimmed
			break
		}
	}
	return typeSymbolHash.ReplaceAllString(name, "")
}
