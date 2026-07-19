package pe

import (
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// loopInfo describes a natural loop the engine residualizes rather than unrolls: a
// loop whose body carries a runtime trip count and so has no merge-point marker to
// memoize at. Green loops (a fully-static trip count) never match -- they unroll
// through the ordinary merge-point machinery.
type loopInfo struct {
	header  *ir.Block
	blocks  map[*ir.Block]bool
	carried []uint32 // alloca temp IDs written in the loop -- its loop-carried scalar cells
}

// analyzeLoops finds each natural loop whose body contains no merge-point marker,
// keyed by its header. A back-edge is an edge whose target dominates its source; the
// loop is the header plus everything that reaches the back-edge without passing back
// through the header.
func analyzeLoops(f *ir.Func, isMerge func(*ir.Instr) bool) map[*ir.Block]*loopInfo {
	cfg := analysis.BuildCFG(f)
	dom := cfg.Dominators()

	// The alloca temps, so a store straight to one is recognized as a scalar cell.
	alloc := map[uint32]bool{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp {
				alloc[in.To.ID] = true
			}
		}
	}

	out := map[*ir.Block]*loopInfo{}
	for _, b := range f.Blocks {
		if !cfg.Reachable(b) {
			continue
		}
		for _, s := range b.Succs() {
			if !dom.Dominates(s, b) {
				continue // not a back-edge
			}
			li := out[s]
			if li == nil {
				li = &loopInfo{header: s, blocks: map[*ir.Block]bool{s: true}}
				out[s] = li
			}
			collectLoopBody(li, b)
		}
	}

	for h, li := range out {
		if !mergeFree(li, isMerge) {
			delete(out, h) // a marked loop -- the interpreter's dispatch loop -- memoizes instead
			continue
		}
		li.carried = carriedCells(li, alloc)
	}
	return out
}

// collectLoopBody adds to li every block that reaches the back-edge source tail
// without passing through the header (a backward walk that stops at the header).
func collectLoopBody(li *loopInfo, tail *ir.Block) {
	stack := []*ir.Block{tail}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if li.blocks[n] {
			continue
		}
		li.blocks[n] = true
		if n == li.header {
			continue
		}
		stack = append(stack, n.Preds...)
	}
}

func mergeFree(li *loopInfo, isMerge func(*ir.Instr) bool) bool {
	for bl := range li.blocks {
		for k := range bl.Instrs {
			if isMerge(&bl.Instrs[k]) {
				return false
			}
		}
	}
	return true
}

// carriedCells returns the alloca temp IDs a store in the loop writes directly --
// the loop's mutable scalar state, which becomes a phi at the residual header.
func carriedCells(li *loopInfo, alloc map[uint32]bool) []uint32 {
	seen := map[uint32]bool{}
	var out []uint32
	for bl := range li.blocks {
		for k := range bl.Instrs {
			in := &bl.Instrs[k]
			if in.Op.IsStore() && len(in.Args) >= 2 && in.Args[1].Kind == ir.RefTemp && alloc[in.Args[1].ID] {
				if !seen[in.Args[1].ID] {
					seen[in.Args[1].ID] = true
					out = append(out, in.Args[1].ID)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
