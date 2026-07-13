package opt

import "github.com/evanphx/cg12/ir"

// DCE removes instructions and phis whose results are never used and that have
// no side effects, iterating to a fixpoint so that chains of dead code collapse.
func DCE(f *ir.Func) bool {
	changed := false
	for {
		uses := useCounts(f)
		removed := false
		for _, b := range f.Blocks {
			// Prune dead phis.
			if kept := filterPhis(b.Phis, uses); len(kept) != len(b.Phis) {
				b.Phis = kept
				removed = true
			}
			// Prune dead instructions.
			out := b.Instrs[:0]
			for _, in := range b.Instrs {
				if isDead(&in, uses) {
					removed = true
					continue
				}
				out = append(out, in)
			}
			b.Instrs = out
		}
		if !removed {
			break
		}
		changed = true
	}
	return changed
}

func filterPhis(phis []*ir.Phi, uses map[uint32]int) []*ir.Phi {
	out := phis[:0]
	for _, p := range phis {
		if p.To.Kind == ir.RefTemp && uses[p.To.ID] == 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// isDead reports whether an instruction can be removed.
func isDead(in *ir.Instr, uses map[uint32]int) bool {
	if hasSideEffect(in.Op) {
		return false
	}
	if in.To.Kind != ir.RefTemp {
		return true // no side effect and no result: pure dead weight
	}
	return uses[in.To.ID] == 0
}

// hasSideEffect reports whether an op affects state beyond its result.
func hasSideEffect(op ir.Op) bool {
	switch {
	case op.IsStore():
		return true
	case op == ir.OCall, op == ir.OBlit, op == ir.OVaStart:
		return true
	case op == ir.OSafepoint:
		return true // a safepoint pins the stack map; never remove it
	}
	return false
}
