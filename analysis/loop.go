package analysis

import "github.com/evanphx/cg12/ir"

// LoopDepth returns the loop-nesting depth of every block: 0 outside all loops,
// and incremented once for each natural loop containing the block. A natural
// loop is identified by a back edge b->h where the header h dominates b.
func (c *CFG) LoopDepth(dom *DomTree) map[*ir.Block]int {
	depth := map[*ir.Block]int{}

	// Group latches (back-edge sources) by loop header.
	latches := map[*ir.Block][]*ir.Block{}
	for _, b := range c.RPO {
		for _, s := range b.Succs() {
			if s != nil && dom.Dominates(s, b) {
				latches[s] = append(latches[s], b)
			}
		}
	}
	for header, ls := range latches {
		for n := range naturalLoop(header, ls) {
			depth[n]++
		}
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
