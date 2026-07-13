package opt

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// GVN performs dominator-tree-based global value numbering: a redundant pure
// computation is replaced by an equivalent one that dominates it. The scoped
// hash table walks the dominator tree, so every entry it holds was defined by a
// block that dominates the current one — hence the reused value is always
// available. Commutative operands are canonicalised so add(x,y) and add(y,x)
// number identically.
func GVN(f *ir.Func) bool {
	cfg := analysis.BuildCFG(f)
	dom := cfg.Dominators()
	children := domChildren(cfg, dom)

	sub := subst{}
	table := map[valueKey]ir.Ref{}
	changed := false

	var visit func(b *ir.Block)
	visit = func(b *ir.Block) {
		var added []valueKey
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if !gvnEligible(in.Op) || in.To.Kind != ir.RefTemp {
				continue
			}
			k := makeKey(in, sub)
			if prev, ok := table[k]; ok {
				sub[in.To.ID] = prev
				b.Instrs[i] = ir.Instr{Op: ir.ONop}
				changed = true
			} else {
				table[k] = in.To
				added = append(added, k)
			}
		}
		for _, c := range children[b] {
			visit(c)
		}
		for _, k := range added {
			delete(table, k)
		}
	}
	if len(cfg.RPO) > 0 {
		visit(cfg.RPO[0])
	}

	if changed {
		applySubst(f, sub)
		removeNops(f)
	}
	return changed
}

// valueKey identifies a pure computation by its op, class, predicate, and
// canonicalised operands (resolved through prior substitutions).
type valueKey struct {
	op   ir.Op
	cls  ir.Cls
	cmp  ir.Cmp
	a, b ir.Ref
}

func makeKey(in *ir.Instr, sub subst) valueKey {
	a := sub.resolve(in.Arg(0))
	b := sub.resolve(in.Arg(1))
	if in.Op.IsCommutative() && lessRef(b, a) {
		a, b = b, a
	}
	return valueKey{op: in.Op, cls: in.Cls, cmp: in.Cmp, a: a, b: b}
}

func lessRef(x, y ir.Ref) bool {
	if x.Kind != y.Kind {
		return x.Kind < y.Kind
	}
	return x.ID < y.ID
}

// gvnEligible reports whether an op is a side-effect-free computation whose
// result is a pure function of its operands (so it can be value-numbered).
// Loads are excluded: without alias analysis two loads are not known equal.
func gvnEligible(op ir.Op) bool {
	switch op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.ODiv, ir.OUDiv, ir.ORem, ir.OURem,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar,
		ir.ONeg, ir.OCmp,
		ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw,
		ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof,
		ir.OCast, ir.OCopy:
		return true
	}
	return false
}
