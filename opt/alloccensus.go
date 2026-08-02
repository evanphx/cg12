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
// orders of magnitude. A front-end change that turns a heap allocation into such
// a slot still fails the baseline -- the heap line disappears -- it just reads as
// a site that went away rather than a site that moved.
func AllocationCensus(module *ir.Module) []Allocation {
	seen := make(map[string]Allocation)
	record := func(allocation Allocation) {
		if _, exists := seen[allocation.Key()]; !exists {
			seen[allocation.Key()] = allocation
		}
	}

	for _, decision := range module.AllocDecisions {
		if decision.Placement != ir.AllocInFrame {
			// Heap placements are read out of the finished IR below, so that a
			// candidate lowered to a call and a call the front end emitted itself
			// produce the same record rather than two spellings of one site.
			continue
		}
		record(Allocation{
			Func:      decision.Func,
			Pos:       decision.Pos,
			File:      module.FileName(decision.Pos.File),
			Allocator: decision.Allocator,
			Type:      AllocationTypeName(decision.Type),
			Placement: ir.AllocInFrame,
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

	census := make([]Allocation, 0, len(seen))
	for _, allocation := range seen {
		census = append(census, allocation)
	}
	sort.Slice(census, func(i, j int) bool { return census[i].Key() < census[j].Key() })
	return census
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
