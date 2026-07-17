package opt

import "github.com/evanphx/cg12/ir"

// DeadAlloc removes stores to non-escaping stack allocations that are never
// loaded. After LoadElim forwards a compound value's element loads to the stored
// values, the backing allocation is written but never read; escape analysis
// proves it cannot be observed, so the stores are dead. Removing them leaves the
// allocation and its address arithmetic unused, which DCE then deletes — so a
// non-escaping aggregate is fully decomposed into SSA values.
func DeadAlloc(f *ir.Func) bool {
	ai := newAliasInfo(f)

	// Collect the allocations and which of them are ever loaded.
	allocs := map[uint32]bool{}
	loaded := map[uint32]bool{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp {
				allocs[in.To.ID] = true
			}
			if in.Op.IsLoad() {
				if base, ok := ai.allocBase[in.Arg(0).ID]; ok {
					loaded[base] = true
				}
			}
			// Calls to the recognized memory helpers do not retain local pointers,
			// but they do observe the bytes stored behind those pointers. Keep the
			// allocation's stores even when later scalar load elimination has removed
			// every explicit load from this function.
			if benignMemoryCall(f, *in) {
				for _, argument := range in.Args[1:] {
					if argument.Kind != ir.RefTemp {
						continue
					}
					if base, ok := ai.allocBase[argument.ID]; ok {
						loaded[base] = true
					}
				}
			}
			// A volatile store is observable even when nothing reads the value
			// back, so the storage it writes to is not dead.
			if in.Volatile && in.Op.IsStore() {
				if base, ok := ai.allocBase[in.Arg(1).ID]; ok {
					loaded[base] = true
				}
			}
		}
	}

	// An allocation is dead if it neither escapes nor is ever loaded.
	dead := map[uint32]bool{}
	for id := range allocs {
		if !ai.escaped[id] && !loaded[id] {
			dead[id] = true
		}
	}
	if len(dead) == 0 {
		return false
	}

	// Drop every store into a dead allocation, and any lifetime marker bracketing
	// one -- a dead slot needs no live-range bounds, and leaving the markers would
	// keep the allocation (and its address arithmetic) artificially live so DCE
	// could not delete it.
	changed := false
	for _, b := range f.Blocks {
		out := b.Instrs[:0]
		for _, in := range b.Instrs {
			if in.Op.IsStore() {
				if base, ok := ai.allocBase[in.Arg(1).ID]; ok && dead[base] {
					changed = true
					continue
				}
			}
			if in.Op.IsLifetime() {
				if base, ok := ai.allocBase[in.Arg(0).ID]; ok && dead[base] {
					changed = true
					continue
				}
			}
			out = append(out, in)
		}
		b.Instrs = out
	}
	return changed
}
