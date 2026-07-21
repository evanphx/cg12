package lower

import "github.com/evanphx/cg12/ir"

// ThreadJumps collapses the empty forwarding blocks that edge splitting and SSA
// destruction leave behind: a block whose only action is an unconditional jump,
// with no instructions of its own, is bypassed so branches land directly on its
// target, and blocks left unreachable are dropped. Trampoline chains -- a handler
// jumping to a split block that jumps to the loop's merge block that jumps to the
// dispatch head -- collapse to a single branch.
//
// It runs after SSA destruction, so a forwarded-through block holds no phi (there
// is no per-predecessor value that block was needed to carry). A block whose
// address is taken (a computed-goto landing pad, via OBlockAddr) is never bypassed
// or removed: an indirect branch can reach it without any explicit edge.
func ThreadJumps(f *ir.Func) {
	addrTaken := map[*ir.Block]bool{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			if in := &b.Instrs[k]; in.Op == ir.OBlockAddr && in.Blk != nil {
				addrTaken[in.Blk] = true
			}
		}
	}

	// forward follows a chain of empty jump-only blocks to the first block that
	// does real work (or whose address is taken), guarding against a cycle of empty
	// blocks such as `for(;;);`.
	forward := func(t *ir.Block) *ir.Block {
		seen := map[*ir.Block]bool{}
		for t != nil && !addrTaken[t] && len(t.Instrs) == 0 &&
			len(t.Phis) == 0 && t.Jmp.Kind == ir.JmpJmp {
			if seen[t] {
				break
			}
			seen[t] = true
			t = t.Jmp.To
		}
		return t
	}

	for _, b := range f.Blocks {
		switch b.Jmp.Kind {
		case ir.JmpJmp:
			b.Jmp.To = forward(b.Jmp.To)
		case ir.JmpJnz:
			b.Jmp.To = forward(b.Jmp.To)
			b.Jmp.To2 = forward(b.Jmp.To2)
		case ir.JmpSwitch:
			b.Jmp.To = forward(b.Jmp.To)
			for i := range b.Jmp.Cases {
				b.Jmp.Cases[i].Blk = forward(b.Jmp.Cases[i].Blk)
			}
		case ir.JmpTable:
			for i := range b.Jmp.Targets {
				b.Jmp.Targets[i] = forward(b.Jmp.Targets[i])
			}
		}
	}

	// Drop the blocks nothing reaches any more. An address-taken block is a root:
	// an indirect branch can land on it with no explicit edge into it.
	reachable := map[*ir.Block]bool{}
	var walk func(b *ir.Block)
	walk = func(b *ir.Block) {
		if b == nil || reachable[b] {
			return
		}
		reachable[b] = true
		for _, s := range b.Succs() {
			walk(s)
		}
	}
	walk(f.Start)
	for _, b := range f.Blocks {
		if addrTaken[b] {
			walk(b)
		}
	}
	kept := f.Blocks[:0]
	for _, b := range f.Blocks {
		if reachable[b] {
			kept = append(kept, b)
		}
	}
	f.Blocks = kept
}
