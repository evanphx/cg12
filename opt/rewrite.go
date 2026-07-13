// Package opt provides composable SSA optimization passes over the ir form:
// constant folding with algebraic simplification, copy elimination, dead-code
// elimination, and constant-branch / unreachable-block CFG simplification.
//
// Each pass takes an *ir.Func, mutates it in place, and reports whether it
// changed anything, so [Optimize] can run them to a fixpoint. Passes operate on
// SSA and are meant to run before target lowering.
package opt

import "github.com/evanphx/cg12/ir"

// subst maps a temporary id to its replacement reference.
type subst map[uint32]ir.Ref

// resolve follows substitution chains to a fixed point.
func (s subst) resolve(r ir.Ref) ir.Ref {
	for r.Kind == ir.RefTemp {
		n, ok := s[r.ID]
		if !ok {
			break
		}
		r = n
	}
	return r
}

// applySubst rewrites every operand reference in f through s.
func applySubst(f *ir.Func, s subst) {
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for i := range p.Args {
				p.Args[i] = s.resolve(p.Args[i])
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			for j := range in.Args {
				in.Args[j] = s.resolve(in.Args[j])
			}
		}
		b.Jmp.Arg = s.resolve(b.Jmp.Arg)
	}
}

// useCounts returns how many times each temporary is used as an operand.
func useCounts(f *ir.Func) map[uint32]int {
	u := map[uint32]int{}
	add := func(r ir.Ref) {
		if r.Kind == ir.RefTemp {
			u[r.ID]++
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				add(a)
			}
		}
		for i := range b.Instrs {
			for _, a := range b.Instrs[i].Args {
				add(a)
			}
		}
		add(b.Jmp.Arg)
	}
	return u
}

// removeNops drops ONop placeholder instructions from every block.
func removeNops(f *ir.Func) {
	for _, b := range f.Blocks {
		out := b.Instrs[:0]
		for _, in := range b.Instrs {
			if in.Op != ir.ONop {
				out = append(out, in)
			}
		}
		b.Instrs = out
	}
}

// constInt returns the integer value of a constant reference.
func constInt(f *ir.Func, r ir.Ref) (int64, bool) {
	if r.Kind == ir.RefConst {
		if c := f.Consts[r.ID]; c.Kind == ir.ConstInt {
			return c.Int, true
		}
	}
	return 0, false
}
