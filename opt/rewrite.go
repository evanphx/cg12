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
			in.ClosureContext = s.resolve(in.ClosureContext)
		}
		b.Jmp.Arg = s.resolve(b.Jmp.Arg)
		for index := range b.Jmp.Args {
			b.Jmp.Args[index] = s.resolve(b.Jmp.Args[index])
		}
	}
}

// useCounts returns how many times each temporary is used as an operand,
// indexed by temp id.
//
// A slice rather than a map because temp ids are dense: ir.Func only ever
// appends temporaries, so they run 0..len(Temps)-1 with no gaps, and a map was
// paying hashing and 69 MB of growth on a -O compile of the SQLite amalgamation
// to hold what an array holds exactly. [IfConvert] already counted uses this
// way; this brings DCE into line with it.
func useCounts(f *ir.Func) []int32 {
	u := make([]int32, len(f.Temps))
	add := func(r ir.Ref) {
		if r.Kind == ir.RefTemp && int(r.ID) < len(u) {
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
			for _, a := range b.Instrs[i].Uses() {
				add(a)
			}
		}
		add(b.Jmp.Arg)
		for _, argument := range b.Jmp.Args {
			add(argument)
		}
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

// useCountOf reads a temporary's use count from a [useCounts] result, answering
// zero for a ref naming a temporary the count was not sized for. A caller that
// counted before adding temporaries would otherwise index past the end.
func useCountOf(uses []int32, r ir.Ref) int32 {
	if r.Kind != ir.RefTemp || int(r.ID) >= len(uses) {
		return 0
	}
	return uses[r.ID]
}
