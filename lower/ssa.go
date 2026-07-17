package lower

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// SSA destruction: replacing phis with copies on the incoming edges, and the
// edge splitting that has to precede it.
//
// None of this knows anything about a machine. It was written twice, once per
// backend, and the two copies were identical but for their comments -- which is
// what a third backend would have inherited, and a fourth after that. The phi
// rule is the same everywhere: the copies happen on the edge, they happen at
// once, and a machine has no opinion about either.

// SplitCriticalEdges breaks every edge from a block with several successors to a
// block with several predecessors, by putting a new block on it.
//
// A phi's copies belong on its incoming edge. A critical edge has nowhere to
// put them: the source block's other successors would run them too, and the
// target's other predecessors would not. The new block is that nowhere.
func SplitCriticalEdges(f *ir.Func) {
	analysis.BuildCFG(f) // fills Preds
	// Snapshot the block list; we append split blocks as we go.
	blocks := append([]*ir.Block(nil), f.Blocks...)
	for _, u := range blocks {
		if u.Jmp.Kind != ir.JmpJnz {
			continue
		}
		if u.Jmp.To == u.Jmp.To2 {
			continue // degenerate: both edges to the same block
		}
		for _, edge := range []**ir.Block{&u.Jmp.To, &u.Jmp.To2} {
			v := *edge
			if len(v.Preds) < 2 {
				continue
			}
			s := f.NewBlock(u.Name + "_" + v.Name + "_edge")
			s.Goto(v)
			*edge = s
			// Redirect phi sources in v from u to the new split block.
			for _, p := range v.Phis {
				for k, b := range p.Blocks {
					if b == u {
						p.Blocks[k] = s
					}
				}
			}
		}
	}
}

// DestructSSA replaces every phi with copies at the end of each predecessor.
//
// The copies on one edge are parallel -- a phi reads the values as they were on
// entry, so they all happen at once -- which is what sequentialize is for.
// Requires SplitCriticalEdges to have run.
func DestructSSA(f *ir.Func) {
	for _, v := range f.Blocks {
		if len(v.Phis) == 0 {
			continue
		}
		// Group phi (dst <- src) pairs by predecessor edge.
		perPred := map[*ir.Block][]movePair{}
		var order []*ir.Block
		for _, p := range v.Phis {
			for k, pred := range p.Blocks {
				if _, ok := perPred[pred]; !ok {
					order = append(order, pred)
				}
				perPred[pred] = append(perPred[pred], movePair{dst: p.To, src: p.Args[k]})
			}
		}
		for _, pred := range order {
			seq := sequentialize(f, perPred[pred])
			for _, mv := range seq {
				pred.Instrs = append(pred.Instrs, ir.Instr{
					Op:   ir.OCopy,
					Cls:  f.ClassOf(mv.dst),
					To:   mv.dst,
					Args: []ir.Ref{mv.src},
				})
			}
		}
		v.Phis = nil
	}
}

type movePair struct{ dst, src ir.Ref }

// sequentialize turns a set of parallel copies (all dsts distinct) into an
// ordered sequence with the same effect, breaking cycles with a fresh temp.
func sequentialize(f *ir.Func, pairs []movePair) []movePair {
	var work []movePair
	for _, p := range pairs {
		if p.dst != p.src {
			work = append(work, p)
		}
	}
	var out []movePair
	for len(work) > 0 {
		// Prefer a copy whose destination is not still needed as a source.
		idx := -1
		for i, p := range work {
			needed := false
			for _, q := range work {
				if q.src == p.dst {
					needed = true
					break
				}
			}
			if !needed {
				idx = i
				break
			}
		}
		if idx >= 0 {
			out = append(out, work[idx])
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Every remaining destination is still read: break a cycle by saving one
		// value into a fresh temporary and rerouting its readers.
		p := work[0]
		tmp := f.NewTemp("", f.ClassOf(p.dst))
		out = append(out, movePair{dst: tmp, src: p.dst})
		for i := range work {
			if work[i].src == p.dst {
				work[i].src = tmp
			}
		}
	}
	return out
}
