package analysis

import "github.com/evanphx/cg12/ir"

// CFG holds control-flow information derived from a function: predecessor lists
// (written back onto the blocks) and a reverse-postorder numbering. It is the
// substrate for dominance and liveness.
type CFG struct {
	Fn  *ir.Func
	RPO []*ir.Block // reverse postorder; RPO[0] is the entry block

	num map[*ir.Block]int // block -> index within RPO
}

// BuildCFG computes predecessors and reverse postorder for f. Blocks not
// reachable from the entry are omitted from RPO (and thus from later analyses).
func BuildCFG(f *ir.Func) *CFG {
	fillPreds(f)
	// Presized: a growing map rehashes on the way up, and rehashing was a
	// measurable share of a whole-module compile all by itself.
	c := &CFG{Fn: f, num: make(map[*ir.Block]int, len(f.Blocks))}
	c.computeRPO()
	return c
}

// Succs returns the executable and synthetic analysis successors of b.
func (c *CFG) Succs(b *ir.Block) []*ir.Block {
	successors := make([]*ir.Block, 0, b.SuccCount()+len(b.SyntheticSuccs))
	for i := 0; i < b.SuccCount(); i++ {
		successors = append(successors, b.SuccAt(i))
	}
	successors = append(successors, b.SyntheticSuccs...)
	return successors
}

// succCount and succAt walk the same sequence [CFG.Succs] returns -- a block's
// executable successors followed by its synthetic ones -- without building the
// slice. The CFG walk runs once per block per pass per fixpoint round, so the
// slice it used to build was pure garbage at compile-time scale.
func succCount(b *ir.Block) int {
	return b.SuccCount() + len(b.SyntheticSuccs)
}

func succAt(b *ir.Block, i int) *ir.Block {
	if executable := b.SuccCount(); i < executable {
		return b.SuccAt(i)
	} else {
		return b.SyntheticSuccs[i-executable]
	}
}

// fillPreds rebuilds each block's predecessor list from the executable and
// synthetic control-flow edges. A predecessor is listed once even if several
// edges target the same block.
func fillPreds(f *ir.Func) {
	for _, b := range f.Blocks {
		b.Preds = b.Preds[:0]
	}
	for _, b := range f.Blocks {
		for i, count := 0, succCount(b); i < count; i++ {
			s := succAt(b, i)
			if s == nil {
				continue
			}
			// The duplicate this drops is a second edge from b to s -- a `jnz c,
			// @x, @x`, or two switch cases landing on one block. Only b appends to
			// any predecessor list while b's own successors are being walked, so b
			// has already been recorded for s exactly when it is the last entry in
			// s.Preds. That is the whole of the set the loop used to carry.
			if len(s.Preds) > 0 && s.Preds[len(s.Preds)-1] == b {
				continue
			}
			s.Preds = append(s.Preds, b)
		}
	}
}

// rpoFrame is one entry of computeRPO's explicit DFS stack: the block being
// visited and how far through its successors the walk has got.
type rpoFrame struct {
	block *ir.Block
	next  int
}

func (c *CFG) computeRPO() {
	// The walk is the recursive postorder DFS it has always been, written with an
	// explicit stack: the closure the recursive form needed escaped to the heap on
	// every call, and so did the separate visited set. c.num doubles as that set,
	// holding visiting until the reverse pass overwrites it with the real index.
	const visiting = -1
	post := make([]*ir.Block, 0, len(c.Fn.Blocks))
	if c.Fn.Start != nil {
		stack := make([]rpoFrame, 0, len(c.Fn.Blocks))
		stack = append(stack, rpoFrame{block: c.Fn.Start})
		c.num[c.Fn.Start] = visiting
		for len(stack) > 0 {
			top := len(stack) - 1
			b := stack[top].block
			descended := false
			for count := succCount(b); stack[top].next < count; {
				s := succAt(b, stack[top].next)
				stack[top].next++
				if s == nil {
					continue
				}
				if _, seen := c.num[s]; seen {
					continue
				}
				c.num[s] = visiting
				stack = append(stack, rpoFrame{block: s})
				descended = true
				break
			}
			if descended {
				continue
			}
			post = append(post, b)
			stack = stack[:top]
		}
	}
	// Reverse the postorder into RPO and record each block's index.
	c.RPO = make([]*ir.Block, len(post))
	for i := range post {
		rb := post[len(post)-1-i]
		c.RPO[i] = rb
		c.num[rb] = i
	}
}

// Num returns the reverse-postorder index of b, or -1 if b is unreachable.
func (c *CFG) Num(b *ir.Block) int {
	if n, ok := c.num[b]; ok {
		return n
	}
	return -1
}

// Reachable reports whether b is reachable from the entry.
func (c *CFG) Reachable(b *ir.Block) bool {
	_, ok := c.num[b]
	return ok
}
