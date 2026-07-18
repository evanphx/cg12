package lower

import "github.com/evanphx/cg12/ir"

// Selects rewrites every OSel (conditional select) into a control-flow diamond
// with a phi, for backends that have no branchless select (bpf) or would rather
// not special-case one (amd64). A `%r = select %c, %a, %b` in the middle of a
// block becomes: the head branches on %c to two empty arms, which rejoin at a
// continuation block holding `%r = phi arm_true %a, arm_false %b` followed by the
// rest of the original block. It reports whether it changed anything.
//
// It runs before SplitCriticalEdges/DestructSSA, so the phi it introduces is
// destructed by the ordinary machinery. Backends with a native select (arm64
// csel, wasm select) skip this and lower OSel directly.
func Selects(f *ir.Func) bool {
	changed := false
	// f.Blocks grows as blocks split; a plain index walk reaches the new
	// continuation blocks (and any selects they inherited) in turn.
	for i := 0; i < len(f.Blocks); i++ {
		b := f.Blocks[i]
		for j := range b.Instrs {
			if b.Instrs[j].Op == ir.OSel {
				expandSelect(f, b, j)
				changed = true
				break // b's tail moved to a fresh block, visited later
			}
		}
	}
	return changed
}

func expandSelect(f *ir.Func, b *ir.Block, j int) {
	sel := b.Instrs[j]
	cond, a, bArg := sel.Args[0], sel.Args[1], sel.Args[2]

	// The continuation inherits everything after the select and the block's own
	// terminator, so b's successors now flow from cont instead of b.
	cont := f.NewBlock("")
	cont.Instrs = append(cont.Instrs, b.Instrs[j+1:]...)
	cont.Jmp = b.Jmp
	for _, s := range cont.Succs() {
		for _, p := range s.Phis {
			for k := range p.Blocks {
				if p.Blocks[k] == b {
					p.Blocks[k] = cont
				}
			}
		}
	}

	// Two single-edge arms so the join has two distinct predecessors for the phi.
	armT := f.NewBlock("")
	armF := f.NewBlock("")
	armT.Goto(cont)
	armF.Goto(cont)

	// The head keeps the instructions before the select and branches on cond.
	b.Instrs = b.Instrs[:j:j]
	b.Jmp = ir.Jmp{}
	b.Jnz(cond, armT, armF)

	cont.Phis = append(cont.Phis, &ir.Phi{
		Cls:    sel.Cls,
		To:     sel.To,
		Args:   []ir.Ref{a, bArg},
		Blocks: []*ir.Block{armT, armF},
	})
}
