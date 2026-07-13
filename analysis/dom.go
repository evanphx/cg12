package analysis

import "github.com/evanphx/cg12/ir"

// DomTree is the dominator tree of a function: each block's immediate dominator.
// It is computed with the Cooper–Harvey–Kennedy iterative algorithm over the
// reverse-postorder numbering, which is simple and fast for the block counts a
// backend sees.
type DomTree struct {
	Idom map[*ir.Block]*ir.Block // immediate dominator; entry maps to nil
}

// Dominators computes the dominator tree from the CFG.
func (c *CFG) Dominators() *DomTree {
	n := len(c.RPO)
	d := &DomTree{Idom: make(map[*ir.Block]*ir.Block, n)}
	if n == 0 {
		return d
	}

	const undef = -1
	idom := make([]int, n)
	for i := range idom {
		idom[i] = undef
	}
	idom[0] = 0 // entry is its own immediate dominator during the fixpoint

	intersect := func(a, b int) int {
		for a != b {
			for a > b {
				a = idom[a]
			}
			for b > a {
				b = idom[b]
			}
		}
		return a
	}

	for changed := true; changed; {
		changed = false
		for i := 1; i < n; i++ {
			newIdom := undef
			for _, p := range c.RPO[i].Preds {
				pi := c.Num(p)
				if pi < 0 || idom[pi] == undef {
					continue // unreachable or not yet processed
				}
				if newIdom == undef {
					newIdom = pi
				} else {
					newIdom = intersect(pi, newIdom)
				}
			}
			if newIdom != undef && idom[i] != newIdom {
				idom[i] = newIdom
				changed = true
			}
		}
	}

	// Every reachable non-entry block is reached in RPO through a DFS-tree
	// parent that precedes it and is itself a predecessor, so its idom is always
	// defined by the time the fixpoint settles.
	d.Idom[c.RPO[0]] = nil
	for i := 1; i < n; i++ {
		d.Idom[c.RPO[i]] = c.RPO[idom[i]]
	}
	return d
}

// Dominates reports whether a dominates b (a block dominates itself).
func (d *DomTree) Dominates(a, b *ir.Block) bool {
	for x := b; x != nil; x = d.Idom[x] {
		if x == a {
			return true
		}
	}
	return false
}

// StrictlyDominates reports whether a dominates b and a != b.
func (d *DomTree) StrictlyDominates(a, b *ir.Block) bool {
	return a != b && d.Dominates(a, b)
}
