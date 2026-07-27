package opt

import (
	"github.com/evanphx/cg12/ir"
)

// DeadFuncElim removes functions that are neither exported nor referenced by any
// symbol operand anywhere in the module — chiefly helpers that inlining has
// dissolved into all of their callers. A function whose name still appears as a
// symbol (a direct call or a function pointer taken as a value) is kept, so this
// is conservative and never drops a reachable function.
func DeadFuncElim(m *ir.Module) bool {
	referenced := make(map[string]bool)
	mark := func(f *ir.Func, r ir.Ref) {
		if r.Kind == ir.RefConst && f.Consts[r.ID].Kind == ir.ConstSym {
			referenced[linkerSymbol(f.Consts[r.ID].Sym)] = true
		}
	}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				for _, a := range in.Uses() {
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
				referenced[linkerSymbol(it.Sym)] = true
			}
		}
	}

	kept := m.Funcs[:0]
	changed := false
	for _, f := range m.Funcs {
		if f.Linkage.Export || referenced[linkerSymbol(f.Name)] {
			kept = append(kept, f)
			continue
		}
		changed = true
	}
	m.Funcs = kept
	return changed
}

// linkerSymbol canonicalizes a name to its linker spelling so definitions and
// references denote the same reachability-set entry: a "runtime.foo" definition
// and an already-mangled "runtime_foo" helper call must match. It uses the
// single canonical mangler (ir.LinkerSymbol) shared with the backend rather than
// re-implementing the spelling here.
func linkerSymbol(name string) string { return ir.LinkerSymbol(name) }
