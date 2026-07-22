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
	c := &CFG{Fn: f, num: map[*ir.Block]int{}}
	c.computeRPO()
	return c
}

// Succs returns the executable and synthetic analysis successors of b.
func (c *CFG) Succs(b *ir.Block) []*ir.Block {
	successors := make([]*ir.Block, 0, len(b.Succs())+len(b.SyntheticSuccs))
	successors = append(successors, b.Succs()...)
	successors = append(successors, b.SyntheticSuccs...)
	return successors
}

// fillPreds rebuilds each block's predecessor list from the executable and
// synthetic control-flow edges. A predecessor is listed once even if several
// edges target the same block.
func fillPreds(f *ir.Func) {
	for _, b := range f.Blocks {
		b.Preds = b.Preds[:0]
	}
	for _, b := range f.Blocks {
		seen := map[*ir.Block]bool{}
		successors := make([]*ir.Block, 0, len(b.Succs())+len(b.SyntheticSuccs))
		successors = append(successors, b.Succs()...)
		successors = append(successors, b.SyntheticSuccs...)
		for _, s := range successors {
			if s == nil || seen[s] {
				continue
			}
			seen[s] = true
			s.Preds = append(s.Preds, b)
		}
	}
}

func (c *CFG) computeRPO() {
	visited := map[*ir.Block]bool{}
	var post []*ir.Block
	var dfs func(b *ir.Block)
	dfs = func(b *ir.Block) {
		visited[b] = true
		for _, s := range c.Succs(b) {
			if s != nil && !visited[s] {
				dfs(s)
			}
		}
		post = append(post, b)
	}
	if c.Fn.Start != nil {
		dfs(c.Fn.Start)
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
