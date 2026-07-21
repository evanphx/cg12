package analysis

import "github.com/evanphx/cg12/ir"

// LoopDepth returns the loop-nesting depth of every block: 0 outside all loops,
// and incremented once for each natural loop containing the block. It is a thin
// wrapper over LoopForest: a block's depth is the depth of its innermost loop (the
// length of that loop's parent chain), which equals the number of loops containing it.
func (c *CFG) LoopDepth(dom *DomTree) map[*ir.Block]int {
	depth := map[*ir.Block]int{}
	for b, l := range c.LoopForest(dom).In {
		depth[b] = l.Depth
	}
	return depth
}

// naturalLoop returns the body of the natural loop with the given header and
// latches: the header plus every block that can reach a latch without passing
// through the header.
func naturalLoop(header *ir.Block, latches []*ir.Block) BlockSet {
	body := BlockSet{header: true}
	var stack []*ir.Block
	for _, l := range latches {
		if !body[l] {
			body[l] = true
			stack = append(stack, l)
		}
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range n.Preds {
			if !body[p] {
				body[p] = true
				stack = append(stack, p)
			}
		}
	}
	return body
}
