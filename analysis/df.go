package analysis

import "github.com/evanphx/cg12/ir"

// BlockSet is a set of basic blocks.
type BlockSet map[*ir.Block]bool

// DominanceFrontier computes each block's dominance frontier: the set of blocks
// where the block's dominance "runs out". It is the classic input to SSA phi
// placement and is computed with the Cooper–Harvey–Kennedy edge-scan algorithm.
func (c *CFG) DominanceFrontier(dom *DomTree) map[*ir.Block]BlockSet {
	df := map[*ir.Block]BlockSet{}
	add := func(runner, b *ir.Block) {
		if df[runner] == nil {
			df[runner] = BlockSet{}
		}
		df[runner][b] = true
	}
	for _, b := range c.RPO {
		if len(b.Preds) < 2 {
			continue
		}
		idb := dom.Idom[b]
		for _, p := range b.Preds {
			for runner := p; runner != nil && runner != idb; runner = dom.Idom[runner] {
				add(runner, b)
			}
		}
	}
	return df
}

// IteratedFrontier returns the iterated dominance frontier of a set of blocks:
// the fixpoint of applying the dominance frontier, which is exactly the set of
// blocks that need a phi for a variable defined in those blocks.
func IteratedFrontier(df map[*ir.Block]BlockSet, defs BlockSet) BlockSet {
	result := BlockSet{}
	var work []*ir.Block
	for b := range defs {
		work = append(work, b)
	}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for d := range df[b] {
			if !result[d] {
				result[d] = true
				work = append(work, d)
			}
		}
	}
	return result
}
