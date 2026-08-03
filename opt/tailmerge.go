package opt

import "github.com/evanphx/cg12/ir"

// TailMerge shares a function's return epilogue. Every `return expr` becomes its
// own block ending in a ret, and the backend expands each ret into the full
// callee-saved restore and stack teardown -- so a function with several returns
// carries several identical epilogues. TailMerge folds the empty return blocks
// into one `ret phi` block (a plain `ret` when the function is void): each
// predecessor branches there instead, and a phi collects the value each returned.
// The backend then emits a single epilogue.
//
// It merges only empty return blocks (a bare ret, no other instructions), so no
// computation is duplicated or moved. A return block reached together with
// another from one predecessor's conditional branch is left alone -- a phi cannot
// carry two different values from one predecessor.
func TailMerge(f *ir.Func) bool {
	preds := predMap(f)

	// Candidate empty return blocks: a bare scalar or void ret, with predecessors,
	// and not the multi-register aggregate-return form.
	var cand []*ir.Block
	for _, b := range f.Blocks {
		if len(b.Phis) == 0 && len(b.Instrs) == 0 &&
			b.Jmp.Kind == ir.JmpRet && len(b.Jmp.Args) == 0 && len(preds[b]) > 0 {
			cand = append(cand, b)
		}
	}
	if len(cand) < 2 {
		return false
	}

	// Exclude any return block a predecessor reaches alongside another (its
	// conditional branch would need two phi values on one edge set).
	inGroup := map[*ir.Block]bool{}
	for _, b := range cand {
		inGroup[b] = true
	}
	for _, b := range f.Blocks {
		hits := 0
		var reached []*ir.Block
		for _, s := range b.Succs() {
			if inGroup[s] {
				hits++
				reached = append(reached, s)
			}
		}
		if hits > 1 {
			for _, s := range reached {
				delete(inGroup, s)
			}
		}
	}
	var group []*ir.Block
	void := !f.HasRet
	for _, b := range cand {
		if inGroup[b] {
			group = append(group, b)
		}
	}
	if len(group) < 2 {
		return false
	}

	// Build the shared return block.
	r := f.NewBlock("")
	var phi *ir.Phi
	if void {
		r.Jmp = ir.Jmp{Kind: ir.JmpRet}
	} else {
		phi = &ir.Phi{Cls: f.Retty, To: f.NewTemp("", f.Retty)}
		r.Phis = []*ir.Phi{phi}
		r.Jmp = ir.Jmp{Kind: ir.JmpRet, Arg: phi.To}
	}
	for _, b := range group {
		val := b.Jmp.Arg // the value this block returned (available at each pred)
		for _, p := range preds[b] {
			retargetSucc(p, b, r)
			if !void {
				phi.Args = append(phi.Args, val)
				phi.Blocks = append(phi.Blocks, p)
			}
		}
	}
	if !void {
		f.InheritGCRef(phi.To, phi.Args...) // the merged result stands for each returned value
	}
	// The old return blocks are now unreachable; the following SimplifyCFG drops them.
	return true
}

// retargetSucc replaces every edge from b to old with an edge to n.
func retargetSucc(b *ir.Block, old, n *ir.Block) {
	j := &b.Jmp
	if j.To == old {
		j.To = n
	}
	if j.To2 == old {
		j.To2 = n
	}
	for i := range j.Targets {
		if j.Targets[i] == old {
			j.Targets[i] = n
		}
	}
	for i := range j.Cases {
		if j.Cases[i].Blk == old {
			j.Cases[i].Blk = n
		}
	}
}
