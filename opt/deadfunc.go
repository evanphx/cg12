package opt

import (
	"strings"

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

// linkerSymbol mirrors the target backends' symbol spelling. Frontend-created
// helper calls sometimes already use linker spelling (runtime_foo), while Go
// function definitions retain semantic spelling (runtime.foo). They denote the
// same ELF symbol and therefore must participate in the same reachability set.
func linkerSymbol(name string) string {
	var symbol strings.Builder
	for _, character := range name {
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			symbol.WriteRune(character)
		} else {
			symbol.WriteByte('_')
		}
	}
	if symbol.Len() == 0 {
		return "anon"
	}
	return symbol.String()
}
