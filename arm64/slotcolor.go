package arm64

import (
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// slotGroups partitions temps into slot-sharing groups: two temps whose live ranges
// never overlap can occupy the same stack slot. It returns temp -> the representative
// temp of its group (temps sharing a representative share one slot). Interference is
// exact per-instruction liveness -- a coarser block-level test would miss a temp that
// is born and killed inside one block and wrongly let it share a live slot. Only temps
// of the same size are grouped (sizeOf), so a group's slot size is any member's.
func slotGroups(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, temps []int, sizeOf func(int) int) map[int]int {
	if len(temps) < 2 {
		return nil
	}
	member := make(map[int]bool, len(temps))
	for _, t := range temps {
		member[t] = true
	}

	inter := map[int]map[int]bool{}
	addEdge := func(a, b int) {
		if a == b {
			return
		}
		if inter[a] == nil {
			inter[a] = map[int]bool{}
		}
		if inter[b] == nil {
			inter[b] = map[int]bool{}
		}
		inter[a][b], inter[b][a] = true, true
	}
	for _, b := range cfg.RPO {
		liveSet := live.LiveOut(b).Copy()
		if b.Jmp.Arg.Kind == ir.RefTemp {
			liveSet.Add(int(b.Jmp.Arg.ID))
		}
		for _, a := range b.Jmp.Args {
			if a.Kind == ir.RefTemp {
				liveSet.Add(int(a.ID))
			}
		}
		mark := func() {
			var here []int
			for _, t := range liveSet.Members() {
				if member[t] {
					here = append(here, t)
				}
			}
			for i := 0; i < len(here); i++ {
				for j := i + 1; j < len(here); j++ {
					addEdge(here[i], here[j])
				}
			}
		}
		mark()
		for k := len(b.Instrs) - 1; k >= 0; k-- {
			in := &b.Instrs[k]
			for _, d := range instrDefs(in) {
				liveSet.Remove(d)
			}
			for _, u := range instrUses(in) {
				liveSet.Add(u)
			}
			mark()
		}
	}

	// Greedy colouring: each temp joins the first same-size group with no interfering
	// member. Sorted for determinism.
	order := append([]int(nil), temps...)
	sort.Ints(order)
	type grp struct {
		size    int
		members []int
	}
	var groups []*grp
	rep := make(map[int]int, len(order))
	for _, t := range order {
		sz := sizeOf(t)
		placed := -1
		for gi, g := range groups {
			if g.size != sz {
				continue
			}
			conflict := false
			for _, m := range g.members {
				if inter[t][m] {
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
			groups = append(groups, &grp{size: sz})
			placed = len(groups) - 1
		}
		groups[placed].members = append(groups[placed].members, t)
		rep[t] = groups[placed].members[0]
	}
	return rep
}
