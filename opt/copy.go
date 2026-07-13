package opt

import "github.com/evanphx/cg12/ir"

// Copy eliminates copy instructions: every use of a copy's result is rewritten
// to the copied value and the copy itself removed.
func Copy(f *ir.Func) bool {
	raw := map[uint32]ir.Ref{}
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op == ir.OCopy && in.To.Kind == ir.RefTemp {
				raw[in.To.ID] = in.Args[0]
			}
		}
	}
	if len(raw) == 0 {
		return false
	}

	// Resolve chains of copies to their ultimate source.
	resolved := subst{}
	var chase func(id uint32) ir.Ref
	chase = func(id uint32) ir.Ref {
		src := raw[id]
		for src.Kind == ir.RefTemp {
			next, ok := raw[src.ID]
			if !ok {
				break
			}
			src = next
		}
		return src
	}
	for id := range raw {
		resolved[id] = chase(id)
	}

	applySubst(f, resolved)
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Op == ir.OCopy {
				b.Instrs[i] = ir.Instr{Op: ir.ONop}
			}
		}
	}
	removeNops(f)
	return true
}
