package opt

import "github.com/evanphx/cg12/ir"

// DeadFuncElim removes functions that are neither exported nor referenced by any
// symbol operand anywhere in the module — chiefly helpers that inlining has
// dissolved into all of their callers. A function whose name still appears as a
// symbol (a direct call or a function pointer taken as a value) is kept, so this
// is conservative and never drops a reachable function.
func DeadFuncElim(m *ir.Module) bool {
	referenced := make(map[string]bool)
	mark := func(f *ir.Func, r ir.Ref) {
		if r.Kind == ir.RefConst && f.Consts[r.ID].Kind == ir.ConstSym {
			referenced[f.Consts[r.ID].Sym] = true
		}
	}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				for _, a := range in.Args {
					mark(f, a)
				}
			}
			// Phi operands can carry a function's address too (mem2reg turns a
			// stored function pointer into a phi), so they must be scanned.
			for _, p := range b.Phis {
				for _, a := range p.Args {
					mark(f, a)
				}
			}
			mark(f, b.Jmp.Arg)
			for _, a := range b.Jmp.Args {
				mark(f, a)
			}
		}
	}
	// Data definitions may also point at functions.
	for _, d := range m.Data {
		for _, it := range d.Items {
			if it.Sym != "" {
				referenced[it.Sym] = true
			}
		}
	}

	kept := m.Funcs[:0]
	changed := false
	for _, f := range m.Funcs {
		if f.Linkage.Export || referenced[f.Name] {
			kept = append(kept, f)
			continue
		}
		changed = true
	}
	m.Funcs = kept
	return changed
}
