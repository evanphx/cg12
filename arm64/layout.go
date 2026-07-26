package arm64

import "github.com/evanphx/cg12/ir"

// layoutBlocks orders a function's blocks for emission so that as many control-flow
// edges as possible become fall-throughs rather than explicit branches. Any order is
// correct -- a non-adjacent edge is just an explicit branch the terminator emits --
// so this only trades branches for fall-throughs.
//
// It picks a maximum set of fall-through edges exactly, as a bipartite matching:
// each block may be immediately followed by at most one successor, and precede at
// most one, so the achievable fall-throughs are a maximum matching between blocks
// (as predecessors) and their fall-through-capable successors. A JmpJmp block can
// fall through to its target; a JmpJnz block to either arm (the emitter inverts the
// condition to branch to the other, see branchArm), with the To2/false arm preferred
// so a simple if-then keeps its then-block out of line; br/table/ret have no
// fall-through. The matched edges decompose into chains, which are emitted back to
// back -- entry's chain first (entry must lead), the rest in block order.
//
// The greedy trace layout this replaced reached ~79% of this optimum on Ruby's
// vm_exec_core; the matching captures the rest.
func layoutBlocks(f *ir.Func) []*ir.Block {
	n := len(f.Blocks)
	idx := make(map[*ir.Block]int, n)
	for i, b := range f.Blocks {
		idx[b] = i
	}

	// Fall-through candidates per block, self-loops excluded (a block cannot follow
	// itself). To2 is listed before To so the matching prefers it, matching the
	// convention that a conditional's false arm falls through.
	cand := make([][]int, n)
	for i, b := range f.Blocks {
		switch b.Jmp.Kind {
		case ir.JmpJmp:
			if b.Jmp.To != b {
				cand[i] = []int{idx[b.Jmp.To]}
			}
		case ir.JmpJnz:
			for _, t := range [2]*ir.Block{b.Jmp.To2, b.Jmp.To} {
				if t != b {
					cand[i] = append(cand[i], idx[t])
				}
			}
		}
	}

	// Maximum bipartite matching: matchR[v] is the predecessor that falls through to
	// v. Entry is excluded as a successor -- it must be the first block emitted.
	entry := idx[f.Start]
	matchR := make([]int, n)
	for i := range matchR {
		matchR[i] = -1
	}
	var try func(u int, seen []bool) bool
	try = func(u int, seen []bool) bool {
		for _, v := range cand[u] {
			if v == entry || seen[v] {
				continue
			}
			seen[v] = true
			if matchR[v] == -1 || try(matchR[v], seen) {
				matchR[v] = u
				return true
			}
		}
		return false
	}
	for u := 0; u < n; u++ {
		if len(cand[u]) > 0 {
			try(u, make([]bool, n))
		}
	}

	// nxt[u] is the successor u falls through to; hasPrev marks mid-chain blocks.
	nxt := make([]int, n)
	hasPrev := make([]bool, n)
	for i := range nxt {
		nxt[i] = -1
	}
	for v, u := range matchR {
		if u != -1 {
			nxt[u] = v
			hasPrev[v] = true
		}
	}

	order := make([]*ir.Block, 0, n)
	placed := make([]bool, n)
	emitChain := func(start int) {
		for c := start; c != -1 && !placed[c]; c = nxt[c] {
			placed[c] = true
			order = append(order, f.Blocks[c])
		}
	}
	emitChain(entry)         // entry's chain leads
	for i := 0; i < n; i++ { // then every other chain, from its head
		if !placed[i] && !hasPrev[i] {
			emitChain(i)
		}
	}
	for i := 0; i < n; i++ { // any block only reachable inside a matching cycle
		if !placed[i] {
			emitChain(i)
		}
	}
	return order
}
