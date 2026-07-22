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

	// 0. Thread branches through empty forwarding blocks: if a terminator targets a
	// block that only jumps unconditionally onward, retarget it past that block.
	// This is what turns a conditional whose two empty arms both reach one block
	// into Jnz(c, X, X), which step 1 then collapses -- and with the condition now
	// dead, DCE removes the flag an interpreter stored, loaded, and branched on when
	// both arms went to the same place (124 such branches in Ruby's dispatch loop).
	forward := func(t *ir.Block) *ir.Block {
		// Follow a chain of empty, phi-free forwarding blocks to its end, stopping
		// short of any block that carries phis (their operands are edge-sensitive) or
		// of a cycle.
		seen := map[*ir.Block]bool{}
		for t != nil && len(t.Instrs) == 0 && len(t.Phis) == 0 && t.Jmp.Kind == ir.JmpJmp && !seen[t] {
			u := t.Jmp.To
			if u == nil || u == t || len(u.Phis) != 0 {
				break
			}
			seen[t] = true
			t = u
		}
		return t
	}
	for _, b := range f.Blocks {
		switch b.Jmp.Kind {
		case ir.JmpJnz:
			if nt := forward(b.Jmp.To); nt != b.Jmp.To {
				b.Jmp.To, changed = nt, true
			}
			if nt := forward(b.Jmp.To2); nt != b.Jmp.To2 {
				b.Jmp.To2, changed = nt, true
			}
		case ir.JmpJmp:
			if nt := forward(b.Jmp.To); nt != b.Jmp.To {
				b.Jmp.To, changed = nt, true
			}
		}
	}

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

	// 1b. Fold a switch on a constant to a direct jump to the matching case (or the
	// default). This is what specializes an inlined helper whose call sites pass a
	// constant selector -- e.g. Ruby's vm_sendish(..., method_explorer), inlined into
	// vm_exec_core with a constant explorer, collapses to the one live arm; the dead
	// arms then become unreachable and are dropped below. Compare at the arg's class
	// width so a sub-word case value matches soundly.
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpSwitch {
			continue
		}
		c, ok := constInt(f, b.Jmp.Arg)
		if !ok {
			continue
		}
		cls := f.ClassOf(b.Jmp.Arg)
		cv := truncCls(cls, c)
		target := b.Jmp.To // default
		for _, cs := range b.Jmp.Cases {
			if truncCls(cls, cs.Val) == cv {
				target = cs.Blk
				break
			}
		}
		b.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: target}
		changed = true
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
	for _, entry := range f.Blocks {
		if !entry.SecondaryEntry || reach[entry] {
			continue
		}
		stack := []*ir.Block{entry}
		reach[entry] = true
		for len(stack) > 0 {
			block := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, successor := range block.Succs() {
				if successor != nil && !reach[successor] {
					reach[successor] = true
					stack = append(stack, successor)
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
			if b == nil || b == a || b.SecondaryEntry || len(b.Preds) != 1 || len(b.Phis) != 0 {
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
