package analysis

import (
	"sort"

	"github.com/evanphx/cg12/ir"
)

// Loop is one natural loop: a header, the back-edge sources targeting it (latches),
// and its body. Loops nest via Parent (the innermost enclosing loop, or nil), and
// Depth is 1 for an outermost loop, +1 per enclosing loop.
type Loop struct {
	Header  *ir.Block
	Latches []*ir.Block
	Body    BlockSet
	Parent  *Loop
	Depth   int
}

// LoopForest is the loop-nesting forest of a function: every natural loop, indexed by
// header and by containing block. It persists the structure that LoopDepth computes
// and discards, which the frequency estimator needs to walk loops innermost-first and
// to know each block's innermost loop.
type LoopForest struct {
	Loops    []*Loop             // innermost-first (decreasing Depth)
	ByHeader map[*ir.Block]*Loop // header block -> its loop
	In       map[*ir.Block]*Loop // block -> innermost loop containing it, nil if none
}

// LoopForest builds the loop-nesting forest. A natural loop is identified by a back
// edge b->h where h dominates b (the same test LoopDepth uses); latches sharing a
// header form one loop.
func (c *CFG) LoopForest(dom *DomTree) *LoopForest {
	// Group latches (back-edge sources) by loop header.
	latches := map[*ir.Block][]*ir.Block{}
	for _, b := range c.RPO {
		for _, s := range b.Succs() {
			if s != nil && dom.Dominates(s, b) {
				latches[s] = append(latches[s], b)
			}
		}
	}

	// In reverse postorder, not the grouping map's iteration order: the ties below
	// -- the smallest containing body, the deepest containing loop -- are resolved
	// by position in this list, so a map order would make the forest, and every
	// frequency and spill decision derived from it, differ between runs.
	lf := &LoopForest{ByHeader: map[*ir.Block]*Loop{}, In: map[*ir.Block]*Loop{}}
	for _, header := range c.RPO {
		ls, ok := latches[header]
		if !ok {
			continue
		}
		l := &Loop{Header: header, Latches: ls, Body: naturalLoop(header, ls)}
		lf.Loops = append(lf.Loops, l)
		lf.ByHeader[header] = l
	}

	// Parent = the smallest other loop whose body contains this loop's header. For
	// reducible (properly nested) loops the enclosing loops form a chain, so the
	// smallest containing body is the immediate parent.
	for _, l := range lf.Loops {
		for _, a := range lf.Loops {
			if a != l && a.Body[l.Header] {
				if l.Parent == nil || len(a.Body) < len(l.Parent.Body) {
					l.Parent = a
				}
			}
		}
	}

	// Depth follows the parent chain.
	var depthOf func(l *Loop) int
	depthOf = func(l *Loop) int {
		if l.Depth != 0 {
			return l.Depth
		}
		l.Depth = 1
		if l.Parent != nil {
			l.Depth = depthOf(l.Parent) + 1
		}
		return l.Depth
	}
	for _, l := range lf.Loops {
		depthOf(l)
	}

	// In[b] = the innermost (deepest) loop whose body contains b.
	for _, l := range lf.Loops {
		for b := range l.Body {
			if cur := lf.In[b]; cur == nil || l.Depth > cur.Depth {
				lf.In[b] = l
			}
		}
	}

	// Innermost-first, so the frequency estimator collapses inner loops before the
	// loops that enclose them.
	sort.SliceStable(lf.Loops, func(i, j int) bool { return lf.Loops[i].Depth > lf.Loops[j].Depth })
	return lf
}
