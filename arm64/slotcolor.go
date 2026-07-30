package arm64

import (
	"math/bits"
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
//
// The members are numbered densely and interference is a bit matrix over that
// numbering, and an edge is recorded only where a member *becomes* live rather than
// at every program point it is live at. Both matter at standard-library scale: with a
// map of maps re-marking every live pair at every instruction, this function alone was
// two thirds of the whole compile of goc/testdata/stdlib_http_tls_client_server.go.
// The interference relation and the greedy assignment below are unchanged, so the
// generated code is byte-identical either way.
func slotGroups(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, temps []int, sizeOf func(int) int) map[int]int {
	if len(temps) < 2 {
		return nil
	}

	// Members are numbered in ascending temp order, which is also the order the
	// greedy assignment below walks them in.
	sorted := append([]int(nil), temps...)
	sort.Ints(sorted)
	memberIndex := make([]int32, len(f.Temps))
	for temp := range memberIndex {
		memberIndex[temp] = -1
	}
	members := make([]int, 0, len(sorted))
	for _, temp := range sorted {
		if memberIndex[temp] >= 0 {
			continue
		}
		memberIndex[temp] = int32(len(members))
		members = append(members, temp)
	}

	interference := newInterferenceMatrix(len(members))
	liveMembers := make([]uint64, interference.words)
	// The members that became live at the point being marked. Every pair live at a
	// mark is recorded: the pairs that were already live together were recorded at
	// the previous mark (by induction, with a block's live-out set marked in full),
	// and any pair involving one of these is recorded here.
	var becameLive []int

	addLive := func(temp int) {
		index := memberIndex[temp]
		if index < 0 {
			return
		}
		word, bit := index/64, uint64(1)<<(uint(index)%64)
		if liveMembers[word]&bit != 0 {
			return
		}
		liveMembers[word] |= bit
		becameLive = append(becameLive, int(index))
	}
	removeLive := func(temp int) {
		index := memberIndex[temp]
		if index < 0 {
			return
		}
		liveMembers[index/64] &^= uint64(1) << (uint(index) % 64)
	}
	mark := func() {
		for _, index := range becameLive {
			interference.union(index, liveMembers)
		}
		becameLive = becameLive[:0]
	}

	for _, block := range cfg.RPO {
		for word := range liveMembers {
			liveMembers[word] = 0
		}
		becameLive = becameLive[:0]
		for _, temp := range live.LiveOut(block).Members() {
			addLive(temp)
		}
		if block.Jmp.Arg.Kind == ir.RefTemp {
			addLive(int(block.Jmp.Arg.ID))
		}
		for _, argument := range block.Jmp.Args {
			if argument.Kind == ir.RefTemp {
				addLive(int(argument.ID))
			}
		}
		mark()
		for index := len(block.Instrs) - 1; index >= 0; index-- {
			instruction := &block.Instrs[index]
			for _, def := range instrDefs(instruction) {
				removeLive(def)
			}
			for _, use := range instrUses(instruction) {
				addLive(use)
			}
			mark()
		}
	}
	interference.symmetrize()

	// Greedy colouring: each temp joins the first same-size group with no interfering
	// member. A group carries the union of its members' interference, so that test is
	// one bit rather than a scan of the group.
	type group struct {
		size        int
		first       int
		interfering []uint64
	}
	var groups []*group
	representative := make(map[int]int, len(members))
	for index, temp := range members {
		size := sizeOf(temp)
		placed := -1
		for candidate, g := range groups {
			if g.size != size {
				continue
			}
			if g.interfering[index/64]&(uint64(1)<<(uint(index)%64)) != 0 {
				continue
			}
			placed = candidate
			break
		}
		if placed < 0 {
			groups = append(groups, &group{size: size, first: temp, interfering: make([]uint64, interference.words)})
			placed = len(groups) - 1
		}
		g := groups[placed]
		for word, mask := range interference.row(index) {
			g.interfering[word] |= mask
		}
		representative[temp] = g.first
	}
	return representative
}

// interferenceMatrix is a square bit matrix over densely numbered members: row i
// holds every member simultaneously live with i somewhere in the function.
type interferenceMatrix struct {
	count int
	words int
	bits  []uint64
}

func newInterferenceMatrix(count int) *interferenceMatrix {
	words := (count + 63) / 64
	return &interferenceMatrix{count: count, words: words, bits: make([]uint64, count*words)}
}

func (m *interferenceMatrix) row(index int) []uint64 {
	return m.bits[index*m.words : (index+1)*m.words]
}

func (m *interferenceMatrix) union(index int, other []uint64) {
	row := m.row(index)
	for word := range row {
		row[word] |= other[word]
	}
}

// symmetrize completes the relation. Recording an edge only where a member becomes
// live sets it in one direction, whichever of the pair became live later; the greedy
// assignment reads a member's whole row, so the other direction has to be filled in.
// The diagonal is cleared first: a member does not interfere with itself, and with it
// gone no row is written while it is being read.
func (m *interferenceMatrix) symmetrize() {
	for index := 0; index < m.count; index++ {
		m.row(index)[index/64] &^= uint64(1) << (uint(index) % 64)
	}
	for index := 0; index < m.count; index++ {
		row := m.row(index)
		word, bit := index/64, uint64(1)<<(uint(index)%64)
		for offset := 0; offset < m.words; offset++ {
			remaining := row[offset]
			for remaining != 0 {
				other := offset*64 + bits.TrailingZeros64(remaining)
				remaining &= remaining - 1
				m.row(other)[word] |= bit
			}
		}
	}
}
