package analysis

import "github.com/evanphx/cg12/ir"

// SCCs returns the strongly-connected components of the reachable CFG, each as a
// slice of blocks. A component with more than one block, or a single block that
// branches to itself, is a cycle.
//
// Tarjan's algorithm, iterated rather than recursed: an interpreter's dispatch mesh
// is one enormous component over hundreds of blocks, which would overflow the Go
// stack. The frequency estimator uses this to find the irreducible cyclic regions
// whose visit distribution its spanning-DAG passes cannot compute.
func (c *CFG) SCCs() [][]*ir.Block {
	n := len(c.RPO)
	index := make(map[*ir.Block]int, n)
	low := make(map[*ir.Block]int, n)
	onStack := make(map[*ir.Block]bool, n)
	var stack []*ir.Block
	var comps [][]*ir.Block
	next := 0

	type frame struct {
		b  *ir.Block
		si int
	}
	for _, root := range c.RPO {
		if _, seen := index[root]; seen {
			continue
		}
		dfs := []frame{{root, 0}}
		index[root], low[root] = next, next
		next++
		stack = append(stack, root)
		onStack[root] = true

		for len(dfs) > 0 {
			f := &dfs[len(dfs)-1]
			succs := f.b.Succs()
			advanced := false
			for f.si < len(succs) {
				s := succs[f.si]
				f.si++
				if s == nil {
					continue
				}
				if _, seen := index[s]; !seen {
					index[s], low[s] = next, next
					next++
					stack = append(stack, s)
					onStack[s] = true
					dfs = append(dfs, frame{s, 0})
					advanced = true
					break
				} else if onStack[s] && index[s] < low[f.b] {
					low[f.b] = index[s]
				}
			}
			if advanced {
				continue
			}
			if low[f.b] == index[f.b] {
				var comp []*ir.Block
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp = append(comp, w)
					if w == f.b {
						break
					}
				}
				comps = append(comps, comp)
			}
			child := f.b
			dfs = dfs[:len(dfs)-1]
			if len(dfs) > 0 {
				if p := dfs[len(dfs)-1].b; low[child] < low[p] {
					low[p] = low[child]
				}
			}
		}
	}
	return comps
}
