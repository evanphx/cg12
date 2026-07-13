package opt

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// SimplifyCFG folds constant and degenerate conditional branches into
// unconditional ones, deletes blocks that become unreachable, repairs phi nodes
// whose predecessors disappeared, and collapses trivial single-edge phis.
func SimplifyCFG(f *ir.Func) bool {
	changed := false

	// 1. Fold conditional branches with a constant or coincident target.
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpJnz {
			continue
		}
		if b.Jmp.To == b.Jmp.To2 {
			b.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: b.Jmp.To}
			changed = true
			continue
		}
		if c, ok := constInt(f, b.Jmp.Arg); ok {
			target := b.Jmp.To
			if c == 0 {
				target = b.Jmp.To2
			}
			b.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: target}
			changed = true
		}
	}

	// 2. Compute reachability from the entry.
	reach := map[*ir.Block]bool{}
	if f.Start != nil {
		stack := []*ir.Block{f.Start}
		reach[f.Start] = true
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, s := range b.Succs() {
				if s != nil && !reach[s] {
					reach[s] = true
					stack = append(stack, s)
				}
			}
		}
	}

	// 3. Drop unreachable blocks.
	if len(reach) != len(f.Blocks) {
		kept := f.Blocks[:0]
		for _, b := range f.Blocks {
			if reach[b] {
				kept = append(kept, b)
			}
		}
		f.Blocks = kept
		changed = true
	}

	// 4. Repair phis: keep only operands from blocks that still flow here.
	for _, v := range f.Blocks {
		if len(v.Phis) == 0 {
			continue
		}
		valid := map[*ir.Block]bool{}
		for _, u := range f.Blocks {
			for _, s := range u.Succs() {
				if s == v {
					valid[u] = true
				}
			}
		}
		for _, p := range v.Phis {
			na, nb := p.Args[:0], p.Blocks[:0]
			for k, pb := range p.Blocks {
				if valid[pb] {
					na = append(na, p.Args[k])
					nb = append(nb, pb)
				}
			}
			if len(na) != len(p.Args) {
				changed = true
			}
			p.Args, p.Blocks = na, nb
		}
	}

	// 5. Collapse phis that now have a single incoming edge into copies.
	s := subst{}
	for _, v := range f.Blocks {
		kept := v.Phis[:0]
		for _, p := range v.Phis {
			if len(p.Args) == 1 && p.To.Kind == ir.RefTemp {
				s[p.To.ID] = p.Args[0]
				changed = true
			} else {
				kept = append(kept, p)
			}
		}
		v.Phis = kept
	}
	if len(s) > 0 {
		applySubst(f, s)
	}

	// 6. Coalesce a block into its sole predecessor when that predecessor jumps to
	// it unconditionally: the two are straight-line, so fuse them (appending the
	// successor's body and taking its terminator) and redirect any phis that named
	// the absorbed block to name the survivor.
	analysis.BuildCFG(f)
	for {
		fused := false
		for _, a := range f.Blocks {
			if a.Jmp.Kind != ir.JmpJmp {
				continue
			}
			b := a.Jmp.To
			if b == nil || b == a || len(b.Preds) != 1 || len(b.Phis) != 0 {
				continue
			}
			a.Instrs = append(a.Instrs, b.Instrs...)
			a.Jmp = b.Jmp
			for _, succ := range a.Succs() {
				for _, p := range succ.Phis {
					for k := range p.Blocks {
						if p.Blocks[k] == b {
							p.Blocks[k] = a
						}
					}
				}
			}
			removeBlock(f, b)
			changed, fused = true, true
			break
		}
		if !fused {
			break
		}
		analysis.BuildCFG(f)
	}

	analysis.BuildCFG(f) // refresh predecessor lists
	return changed
}

// removeBlock drops b from the function's block list.
func removeBlock(f *ir.Func, b *ir.Block) {
	kept := f.Blocks[:0]
	for _, x := range f.Blocks {
		if x != b {
			kept = append(kept, x)
		}
	}
	f.Blocks = kept
}
