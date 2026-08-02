package opt

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/ir"
)

// Shadow mode: run the IR escape analysis, with summaries, over the
// allocations goc's AST walk already decided, and report every place the two
// disagree.
//
// Nothing here changes what the compiler emits. It reads a finished module,
// finds the allocations the front end placed itself (ir.Func.PlacedAllocs), and
// asks the same analysis LowerHeapAllocations runs what it would have done with
// each of them. The point is to know, before any placement moves onto the IR,
// which way each of those decisions would go.
//
// The two directions are not symmetric and must not be totted up together:
//
//   - front end heap, IR frame. Permissive. Every one of these is either a
//     latent pessimisation in the AST walk or a hole in the IR analysis, and
//     which it is has to be argued per class before anything switches. This is
//     the direction ccwork/escape-analysis's 2724ac7 got wrong.
//   - front end frame, IR heap. Conservative. Each one costs an allocation if
//     the site moves onto the neutral op. These are the 11 255 placements the
//     plan says a summary-less IR pass would get wrong.

// PlacementDisagreement is one allocation the two analyses place differently.
type PlacementDisagreement struct {
	// Func is the IR function the allocation is in, after inlining.
	Func string
	// Pos is the source position of the allocating instruction.
	Pos ir.SrcPos
	// File is Pos's file name.
	File string
	// Site names the front-end decision site that placed it.
	Site string
	// FrontEnd is where goc's AST walk put it; IR is where the summary-fed IR
	// analysis would put it.
	FrontEnd ir.AllocPlacement
	IR       ir.AllocPlacement
	// Reason names the use that made the IR analysis escape the allocation. It
	// is empty when the IR analysis kept it in the frame -- there is no single
	// use to name for that -- and is the classification key for the other
	// direction.
	Reason string
}

// Key is the disagreement's stable identity: position, function, site and the
// two verdicts, with no IR temporary names in it.
func (disagreement PlacementDisagreement) Key() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s -> %s\t%s",
		disagreement.location(), disagreement.Func, disagreement.Site,
		disagreement.FrontEnd, disagreement.IR, disagreement.Reason)
}

// Class is the direction plus the reason, which is what a reviewer sorts by.
func (disagreement PlacementDisagreement) Class() string {
	if disagreement.IR == ir.AllocInFrame {
		return "front end heap, IR frame: " + disagreement.Site
	}
	return "front end frame, IR heap: " + disagreement.Reason
}

func (disagreement PlacementDisagreement) location() string {
	if !disagreement.Pos.Valid() {
		return "?"
	}
	file := disagreement.File
	if file == "" {
		file = "?"
	}
	return fmt.Sprintf("%s:%d:%d", file, disagreement.Pos.Line, disagreement.Pos.Col)
}

// ShadowCounts is what the shadow run saw in total, so a disagreement count can
// be read against the number of decisions it is a fraction of.
type ShadowCounts struct {
	// Placements is how many front-end placements the shadow evaluated.
	Placements int
	FrontFrame int
	FrontHeap  int
	// Agreements, and the two directions of disagreement.
	Agree         int
	FrameToIRHeap int // front end frame, IR heap: the conservative direction
	HeapToIRFrame int // front end heap, IR frame: the permissive direction
}

// ShadowPlacement evaluates every front-end placement in module against the IR
// analysis fed by facts, and returns the disagreements together with the totals
// they came out of.
//
// It does not modify module.
func ShadowPlacement(module *ir.Module, facts *EscapeFacts) ([]PlacementDisagreement, ShadowCounts) {
	byName := moduleFuncsByName(module, facts)
	var disagreements []PlacementDisagreement
	var counts ShadowCounts

	for _, function := range module.Funcs {
		if function.Start == nil || len(function.PlacedAllocs) == 0 {
			continue
		}
		seeds := make([]uint32, 0, len(function.PlacedAllocs))
		for id := range function.PlacedAllocs {
			seeds = append(seeds, id)
		}
		sort.Slice(seeds, func(i, j int) bool { return seeds[i] < seeds[j] })

		definitions := allocationDefinitions(function, function.PlacedAllocs)
		analysis := analyzeCandidateEscapes(function, byName, facts, seeds, true)
		for _, id := range seeds {
			placed := function.PlacedAllocs[id]
			counts.Placements++
			if placed.Placement == ir.AllocInFrame {
				counts.FrontFrame++
			} else {
				counts.FrontHeap++
			}
			verdict := ir.AllocInFrame
			if analysis.escapes(id) {
				verdict = ir.AllocOnHeap
			}
			if verdict == placed.Placement {
				counts.Agree++
				continue
			}
			if verdict == ir.AllocOnHeap {
				counts.FrameToIRHeap++
			} else {
				counts.HeapToIRFrame++
			}
			position := definitions[id]
			disagreements = append(disagreements, PlacementDisagreement{
				Func:     function.Name,
				Pos:      position,
				File:     module.FileName(position.File),
				Site:     placed.Site,
				FrontEnd: placed.Placement,
				IR:       verdict,
				Reason:   analysis.reason(id),
			})
		}
	}
	sort.Slice(disagreements, func(i, j int) bool {
		return disagreements[i].Key() < disagreements[j].Key()
	})
	return disagreements, counts
}

// FrontEndPlacementSites lists every front-end placement in module by the same
// identity a disagreement uses -- position, function, site, placement -- so a
// count of disagreements has a denominator that counts the same way.
//
// The totals in ShadowCounts are per compile: a stdlib function compiled into
// 385 corpus programs contributes 385 times. These keys deduplicate to sites.
func FrontEndPlacementSites(module *ir.Module) []string {
	var sites []string
	for _, function := range module.Funcs {
		if function.Start == nil || len(function.PlacedAllocs) == 0 {
			continue
		}
		positions := allocationDefinitions(function, function.PlacedAllocs)
		for id, placed := range function.PlacedAllocs {
			position := positions[id]
			where := "?"
			if position.Valid() {
				file := module.FileName(position.File)
				if file == "" {
					file = "?"
				}
				where = fmt.Sprintf("%s:%d:%d", file, position.Line, position.Col)
			}
			sites = append(sites, fmt.Sprintf("%s\t%s\t%s\t%s",
				where, function.Name, placed.Site, placed.Placement))
		}
	}
	sort.Strings(sites)
	return sites
}

// allocationDefinitions finds the source position of each placed allocation, by
// looking at the instruction that defines it.
func allocationDefinitions(function *ir.Func, placed map[uint32]ir.PlacedAlloc) map[uint32]ir.SrcPos {
	positions := make(map[uint32]ir.SrcPos, len(placed))
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.To.Kind != ir.RefTemp {
				continue
			}
			if _, tracked := placed[instruction.To.ID]; !tracked {
				continue
			}
			positions[instruction.To.ID] = instruction.Pos
		}
	}
	return positions
}
