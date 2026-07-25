package arm64

import "github.com/evanphx/cg12/ir"

// layoutBlocks orders a function's blocks for emission so that as many control-flow
// edges as possible become fall-throughs rather than explicit branches. The blocks
// arrive in RPO, which places a block before its successors but does not try to put
// a block's fall-through successor immediately after it; the result is a branch at
// nearly every block boundary (measured on Ruby's vm.c: 13.6% of instructions were
// unconditional branches, against gcc's 4%).
//
// This is a greedy trace layout: starting from the entry, follow each block's
// fall-through-capable successor, extending a straight run until it reaches a block
// already placed (or a terminator with no fall-through). Then seed a new run from
// the next unplaced block, taking seeds in RPO order so the layout is deterministic
// and the entry stays first. Reordering is always safe -- every non-fall-through
// edge is still a branch the terminator emits explicitly -- so this only trades
// branches for fall-throughs, never changes behaviour.
//
// The fall-through-capable successor depends on how the terminator is emitted:
//   - JmpJmp falls through to its single target.
//   - JmpJnz emits `cbnz cond, To` then falls through to To2, so To2 is the edge
//     that can become a fall-through (To is always a taken conditional branch).
//   - JmpBr/JmpTable (computed goto, the interpreter dispatch) and JmpRet/JmpHlt
//     have no fall-through successor.
func layoutBlocks(f *ir.Func) []*ir.Block {
	order := make([]*ir.Block, 0, len(f.Blocks))
	placed := make(map[*ir.Block]bool, len(f.Blocks))

	for _, seed := range f.Blocks {
		for b := seed; b != nil && !placed[b]; b = fallThroughSucc(b) {
			placed[b] = true
			order = append(order, b)
		}
	}
	return order
}

// fallThroughSucc returns the successor that would fall through if laid out next,
// or nil when the terminator has none.
func fallThroughSucc(b *ir.Block) *ir.Block {
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		return b.Jmp.To
	case ir.JmpJnz:
		return b.Jmp.To2
	}
	return nil
}
