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

// CoalescePhis dissolves the phis that sit at a computed goto's targets, so that
// DestructSSA need not emit a copy for them -- which it could not do without
// exploding. It coalesces such a phi's result with the arguments arriving on the
// indirect edges, renaming them to a single temporary; the phi then reads
// x = phi(x, ..., x) on those edges and DestructSSA drops it with no copy.
//
// This is what lets -O run on a computed-goto interpreter. Its handlers form a
// near-complete-bipartite CFG whose critical edges cannot be split, so a phi left
// standing there would force O(handlers^2) copies (see opt.Mem2Reg). But a phi
// argument on such an edge can only reach its block through that phi -- SSA
// dominance forbids any other use -- so its live range ends at the edge and never
// overlaps the phi result. It therefore always coalesces, and the copies that
// would have exploded never appear.
//
// Only the indirect edges are touched. Ordinary phi copies sit on splittable
// edges and cost one copy each, which DestructSSA handles; coalescing them too
// would lengthen live ranges and raise register pressure for no benefit, so a
// function without a computed goto is left exactly as it was.
func CoalescePhis(f *ir.Func) {
	nt := len(f.Temps)
	if nt == 0 {
		return
	}
	// Mark the temps on a phi argument that arrives over a computed-goto edge, with
	// that phi's result: only these are coalesced, and only their mutual
	// interference matters.
	involved := make([]bool, nt)
	anyIndirect := false
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			if p.To.Kind != ir.RefTemp {
				continue
			}
			for k, a := range p.Args {
				if a.Kind == ir.RefTemp && p.Blocks[k].Jmp.Kind == ir.JmpBr {
					involved[p.To.ID] = true
					involved[a.ID] = true
					anyIndirect = true
				}
			}
		}
	}
	if !anyIndirect {
		return
	}

	cfg := analysis.BuildCFG(f)
	live := cfg.Liveness()
	interf := buildPhiInterference(f, live, nt, involved)

	// Union-find over temps, each root carrying its members and the union of its
	// members' interference sets. Two classes may coalesce only when no member of
	// one interferes with any member of the other.
	parent := make([]int, nt)
	size := make([]int, nt)
	members := make([][]int32, nt)
	for i := 0; i < nt; i++ {
		parent[i] = i
		if involved[i] {
			size[i] = 1
			members[i] = []int32{int32(i)}
		}
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if size[ra] < size[rb] { // keep the larger class as root
			ra, rb = rb, ra
		}
		ia := interf[ra]
		for _, m := range members[rb] {
			if ia.Has(int(m)) {
				return // a member interferes: leave them separate, a copy will bridge
			}
		}
		parent[rb] = ra
		size[ra] += size[rb]
		members[ra] = append(members[ra], members[rb]...)
		members[rb] = nil
		ia.Union(interf[rb])
	}

	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			if p.To.Kind != ir.RefTemp {
				continue
			}
			d := int(p.To.ID)
			for k, a := range p.Args {
				if a.Kind == ir.RefTemp && p.Blocks[k].Jmp.Kind == ir.JmpBr {
					union(d, int(a.ID))
				}
			}
		}
	}

	// Each class renames to its lowest-numbered member: deterministic, and it keeps
	// the earliest-defined temp (usually the most readable name).
	rep := make([]uint32, nt)
	for i := 0; i < nt; i++ {
		rep[i] = uint32(i)
	}
	for i := 0; i < nt; i++ {
		if members[i] == nil {
			continue // not a class root
		}
		min := int32(i)
		for _, m := range members[i] {
			if m < min {
				min = m
			}
		}
		for _, m := range members[i] {
			rep[m] = uint32(min)
		}
	}

	rename := func(r *ir.Ref) {
		if r.Kind == ir.RefTemp {
			r.ID = rep[r.ID]
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			rename(&p.To)
			for k := range p.Args {
				rename(&p.Args[k])
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			rename(&in.To)
			for k := range in.Args {
				rename(&in.Args[k])
			}
			for k := range in.Defs {
				rename(&in.Defs[k])
			}
		}
		rename(&b.Jmp.Arg)
		for k := range b.Jmp.Args {
			rename(&b.Jmp.Args[k])
		}
	}
}

// buildPhiInterference returns, for each phi-involved temp, the set of other
// phi-involved temps whose live range overlaps it -- the ones it may not share
// storage with. Interference with temps that take no part in any phi is not
// recorded: those are never coalesced, so it could not change a decision.
func buildPhiInterference(f *ir.Func, live *analysis.Liveness, nt int, involved []bool) []*analysis.TempSet {
	interf := make([]*analysis.TempSet, nt)
	for i := 0; i < nt; i++ {
		if involved[i] {
			interf[i] = analysis.NewTempSet(nt)
		}
	}
	addEdge := func(a, b int) {
		if a != b && involved[a] && involved[b] {
			interf[a].Add(b)
			interf[b].Add(a)
		}
	}
	// def d, defined at a point whose live-out is liveNow, interferes with every
	// other temp live there.
	def := func(d int, liveNow *analysis.TempSet) {
		if involved[d] {
			for _, t := range liveNow.Members() {
				addEdge(d, t)
			}
		}
	}
	for _, b := range f.Blocks {
		liveNow := live.Out[b].Copy()
		liveNow.AddRef(b.Jmp.Arg)
		for _, a := range b.Jmp.Args {
			liveNow.AddRef(a)
		}
		for k := len(b.Instrs) - 1; k >= 0; k-- {
			in := &b.Instrs[k]
			if in.To.Kind == ir.RefTemp {
				def(int(in.To.ID), liveNow)
				liveNow.Remove(int(in.To.ID))
			}
			for _, dr := range in.Defs {
				if dr.Kind == ir.RefTemp {
					def(int(dr.ID), liveNow)
					liveNow.Remove(int(dr.ID))
				}
			}
			for _, a := range in.Args {
				liveNow.AddRef(a)
			}
		}
		// A block's phi results are all defined together at its top: each interferes
		// with whatever else is live there, and with the other phi results.
		for i, p := range b.Phis {
			if p.To.Kind != ir.RefTemp {
				continue
			}
			def(int(p.To.ID), liveNow)
			for j := i + 1; j < len(b.Phis); j++ {
				if b.Phis[j].To.Kind == ir.RefTemp {
					addEdge(int(p.To.ID), int(b.Phis[j].To.ID))
				}
			}
		}
	}
	return interf
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
