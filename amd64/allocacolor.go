package amd64

import (
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// This file is stack colouring for fixed-size allocas: two allocations whose
// live regions never overlap can share one frame slot instead of taking one
// each. It is the amd64 counterpart of arm64/allocacolor.go and is deliberately
// the same algorithm, because the analysis is pure IR -- lifetime markers and a
// CFG -- with nothing architectural in it. What *is* architectural is how the
// result is consumed: arm64 lays allocas out at increasing positive offsets from
// x29, amd64 at increasing distances *below* RBP, so computeFrame's use of these
// groups is written against the amd64 direction and is not a copy.
//
// Nothing here reads a register, a convention, or a frame size, so it is safe
// under both System V and Go ABIInternal.

// allocaGroups colours fixed-size stack allocations onto shared slots using their
// lifetime.start/end markers: two allocations whose live regions never overlap can
// occupy the same stack slot (LLVM's stack colouring, and what lets an interpreter's
// per-handler locals share a handful of slots instead of one each). It returns, for
// every colourable OAlloc, the representative OAlloc of its slot group -- allocas
// mapping to the same representative share a slot, and every member of a group has
// the same size and alignment, so the slot needs no widening.
//
// Allocas without a matched start+end pair are omitted, and the frame allocator
// gives them a private, whole-function slot. Escape to a call does *not*
// disqualify an alloca: C ends the object's lifetime at the marked lifetime.end,
// so the slot is free to reuse afterward (touching it past the end is undefined)
// -- LLVM's stack colouring trusts the markers over escape the same way.
//
// A nil result means "nothing shares"; every caller must handle it, since that is
// also the answer for a function with fewer than two colourable allocas.
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

	// An allocation is colourable when it has both a start and an end, so its live
	// region is bounded.
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

	// ids is built by walking the blocks rather than ranging the allocaInstr map,
	// so the index assignment -- and therefore the greedy colouring's tie-breaking
	// and the choice of group representative -- is program order and not map
	// iteration order. Frame offsets are observable in the emitted code, so an
	// unstable order here would make the same input compile to different bytes on
	// different runs.
	var ids []uint32
	idx := map[uint32]int{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if !in.Op.IsAlloc() || in.To.Kind != ir.RefTemp {
				continue
			}
			id := in.To.ID
			if _, dup := idx[id]; dup {
				continue
			}
			if hasStart[id] && hasEnd[id] {
				idx[id] = len(ids)
				ids = append(ids, id)
			}
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

	// Greedy colouring: largest first, into the first group of the same size/align
	// class with no interfering member. Requiring an exact size/align match (rather
	// than widening a group to its largest member) is what lets computeFrame place a
	// group once, when its first member is reached, without revisiting the offset.
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

	// Map every coloured alloca to its group's representative (the first member).
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
