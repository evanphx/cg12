package analysis

import (
	"sync"

	"github.com/evanphx/cg12/ir"
)

// DomTree is the dominator tree of a function: each block's immediate dominator.
// It is computed with the Cooper–Harvey–Kennedy iterative algorithm over the
// reverse-postorder numbering, which is simple and fast for the block counts a
// backend sees.
type DomTree struct {
	// idom is the immediate dominator of each block; the entry maps to nil.
	//
	// It is unexported so that [DomTree.Release] can hand it back to be filled in
	// again and a read afterwards can be caught. An exported map could not be:
	// a nil one reads as "no immediate dominator" for every block, which is a
	// plausible-looking answer rather than a failure. That mistake was made once
	// already with CFG.RPO, where it quietly turned a pass into a no-op.
	idom     map[*ir.Block]*ir.Block
	released bool
}

// Idom returns b's immediate dominator, or nil for the entry block and for a
// block the tree does not cover.
func (d *DomTree) Idom(b *ir.Block) *ir.Block {
	if d.released {
		panic("analysis: DomTree read after Release")
	}
	return d.idom[b]
}

// Len reports how many blocks the tree covers.
func (d *DomTree) Len() int {
	if d.released {
		panic("analysis: DomTree read after Release")
	}
	return len(d.idom)
}

// Release says the caller is done with d, so its map can be filled in again by
// the next dominator computation rather than collected. The rule is [CFG.Release]'s
// rule: d must be dead, and reading it afterwards panics.
//
// It is 52 MB of a -O compile of the SQLite amalgamation, most of it from jump
// threading, which builds a dominator tree per candidate it considers.
func (d *DomTree) Release() {
	if d == nil || d.released {
		return
	}
	d.released = true
	tree := d.idom
	d.idom = nil
	if len(tree) > pooledNumberingLimit {
		return
	}
	clear(tree)
	idomPool.Put(tree)
}

var idomPool sync.Pool

func takeIdom(size int) map[*ir.Block]*ir.Block {
	if size <= pooledNumberingLimit {
		if recycled, ok := idomPool.Get().(map[*ir.Block]*ir.Block); ok {
			return recycled
		}
	}
	return make(map[*ir.Block]*ir.Block, size)
}

// Dominators computes the dominator tree from the CFG.
func (c *CFG) Dominators() *DomTree {
	n := len(c.RPO)
	d := &DomTree{idom: takeIdom(n)}
	if n == 0 {
		return d
	}

	// int32 rather than int: these are reverse-postorder indices, so a function
	// would need two billion blocks to overflow one, and the array is built once
	// per function per pass per fixpoint round.
	const undef = int32(-1)
	idom := make([]int32, n)
	for i := range idom {
		idom[i] = undef
	}
	idom[0] = 0 // entry is its own immediate dominator during the fixpoint

	intersect := func(a, b int32) int32 {
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
				pi := int32(c.Num(p))
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
	d.idom[c.RPO[0]] = nil
	for i := 1; i < n; i++ {
		d.idom[c.RPO[i]] = c.RPO[idom[i]]
	}
	return d
}

// Dominates reports whether a dominates b (a block dominates itself).
func (d *DomTree) Dominates(a, b *ir.Block) bool {
	for x := b; x != nil; x = d.Idom(x) {
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
