package arm64

import (
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// allocaGroups colors fixed-size stack allocations onto shared slots using their
// lifetime.start/end markers: two allocations whose live regions never overlap can
// occupy the same stack slot (LLVM's stack-coloring, and what lets an interpreter's
// per-handler locals share a handful of slots instead of one each). It returns, for
// every colorable OAlloc, the representative OAlloc of its slot group -- allocas
// mapping to the same representative share a slot, sized and aligned to the largest
// member. Allocas without a matched start+end pair are omitted (the frame allocator
// gives them a private, whole-function slot); an alloca whose address escapes to a
// call is also omitted, since a callee could retain the pointer past the marked end.
func allocaGroups(f *ir.Func, cfg *analysis.CFG) map[*ir.Instr]*ir.Instr {
	allocaInstr := map[uint32]*ir.Instr{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp {
				allocaInstr[in.To.ID] = in
			}
		}
	}

	// An allocation is colorable when it has both a start and an end, so its live
	// region is bounded. Escape to a call does not disqualify it: C ends the object's
	// lifetime at the marked lifetime.end, so the slot is free to reuse afterward
	// (accessing it past the end is undefined) -- LLVM's stack coloring trusts the
	// markers over escape the same way.
	hasStart := map[uint32]bool{}
	hasEnd := map[uint32]bool{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op == ir.OLifeStart && in.Args[0].Kind == ir.RefTemp {
				hasStart[in.Args[0].ID] = true
			} else if in.Op == ir.OLifeEnd && in.Args[0].Kind == ir.RefTemp {
				hasEnd[in.Args[0].ID] = true
			}
		}
	}

	// Index the colorable allocations in program order, not in map order. The
	// greedy coloring below sorts by size with sort.SliceStable, so equal-size
	// allocations -- the common case, and the whole point of sharing slots -- keep
	// whatever order they were indexed in, and that order decides which group each
	// one joins. Ranging allocaInstr made that Go's per-range map seed, so two runs
	// of the compiler on identical input could emit different frame offsets.
	var ids []uint32
	idx := map[uint32]int{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if !in.Op.IsAlloc() || in.To.Kind != ir.RefTemp {
				continue
			}
			id := in.To.ID
			if _, indexed := idx[id]; indexed {
				continue
			}
			if !hasStart[id] || !hasEnd[id] {
				continue
			}
			idx[id] = len(ids)
			ids = append(ids, id)
		}
	}
	if len(ids) < 2 {
		return nil // nothing can share
	}
	n := len(ids)

	// Forward "which allocas are live" dataflow: a start turns one on, an end off.
	// blockLive[b][i] records whether alloca i is live anywhere in block b -- the
	// substrate for a conservative, block-granular interference test.
	liveOut := map[*ir.Block][]bool{}
	blockLive := map[*ir.Block][]bool{}
	for _, b := range cfg.RPO {
		liveOut[b] = make([]bool, n)
		blockLive[b] = make([]bool, n)
	}
	for changed := true; changed; {
		changed = false
		for _, b := range cfg.RPO {
			cur := make([]bool, n)
			for _, p := range b.Preds {
				lo := liveOut[p]
				for i := 0; i < n; i++ {
					cur[i] = cur[i] || lo[i]
				}
			}
			bl := make([]bool, n)
			copy(bl, cur)
			for k := range b.Instrs {
				in := &b.Instrs[k]
				if in.Op == ir.OLifeStart {
					if i, ok := idx[in.Args[0].ID]; ok {
						cur[i] = true
					}
				} else if in.Op == ir.OLifeEnd {
					if i, ok := idx[in.Args[0].ID]; ok {
						cur[i] = false
					}
				}
				for i := 0; i < n; i++ {
					bl[i] = bl[i] || cur[i]
				}
			}
			blockLive[b] = bl
			old := liveOut[b]
			for i := 0; i < n; i++ {
				if cur[i] != old[i] {
					changed = true
					break
				}
			}
			liveOut[b] = cur
		}
	}

	// Interference: two allocas that are both live somewhere in the same block cannot
	// share a slot.
	interfere := make([]map[int]bool, n)
	for i := range interfere {
		interfere[i] = map[int]bool{}
	}
	for _, b := range cfg.RPO {
		bl := blockLive[b]
		var live []int
		for i := 0; i < n; i++ {
			if bl[i] {
				live = append(live, i)
			}
		}
		for x := 0; x < len(live); x++ {
			for y := x + 1; y < len(live); y++ {
				interfere[live[x]][live[y]] = true
				interfere[live[y]][live[x]] = true
			}
		}
	}

	// Greedy coloring: largest first, into the first group of the same size/align
	// class with no interfering member.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	shapeOf := func(i int) (int, int) { a, s := allocShape(f, allocaInstr[ids[i]]); return a, s }
	sort.SliceStable(order, func(a, b int) bool {
		_, sa := shapeOf(order[a])
		_, sb := shapeOf(order[b])
		return sa > sb
	})

	type group struct {
		align, size int
		members     []int
	}
	var groups []*group
	groupOf := make([]int, n)
	for i := range groupOf {
		groupOf[i] = -1
	}
	for _, i := range order {
		al, sz := shapeOf(i)
		placed := -1
		for gi, g := range groups {
			if g.align != al || g.size != sz {
				continue
			}
			conflict := false
			for _, m := range g.members {
				if interfere[i][m] {
					conflict = true
					break
				}
			}
			if !conflict {
				placed = gi
				break
			}
		}
		if placed < 0 {
			groups = append(groups, &group{align: al, size: sz})
			placed = len(groups) - 1
		}
		groups[placed].members = append(groups[placed].members, i)
		groupOf[i] = placed
	}

	// Map every colored alloca to its group's representative (the first member).
	rep := make([]*ir.Instr, len(groups))
	for _, i := range order {
		g := groupOf[i]
		if rep[g] == nil {
			rep[g] = allocaInstr[ids[i]]
		}
	}
	out := map[*ir.Instr]*ir.Instr{}
	for i, id := range ids {
		out[allocaInstr[id]] = rep[groupOf[i]]
	}
	return out
}
