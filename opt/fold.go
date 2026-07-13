package opt

import (
	"math"

	"github.com/evanphx/cg12/ir"
)

// Fold performs constant folding and local algebraic simplification: every
// instruction whose result is a known constant (or is equivalent to an existing
// value) is replaced and its uses rewritten. Running it inside [Optimize]'s
// fixpoint yields full constant propagation.
func Fold(f *ir.Func) bool {
	s := subst{}
	changed := false
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.To.IsNone() {
				continue
			}
			if r, ok := foldInstr(f, in); ok {
				s[in.To.ID] = r
				b.Instrs[i] = ir.Instr{Op: ir.ONop}
				changed = true
			}
		}
	}
	if changed {
		applySubst(f, s)
		removeNops(f)
	}
	return changed
}

// foldInstr returns the replacement reference for an instruction, if any.
func foldInstr(f *ir.Func, in *ir.Instr) (ir.Ref, bool) {
	switch in.Op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.ODiv, ir.OUDiv, ir.ORem, ir.OURem,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		if in.Cls.IsFloat() {
			return ir.R, false // never integer-fold a floating-point operation
		}
		a, b := in.Args[0], in.Args[1]
		ca, aok := constInt(f, a)
		cb, bok := constInt(f, b)
		if aok && bok {
			if v, ok := evalBinop(in.Op, in.Cls, ca, cb); ok {
				return f.ConstInt(in.Cls, v), true
			}
		}
		return binIdentity(f, in, a, b, ca, aok, cb, bok)

	case ir.ONeg:
		if in.Cls.IsFloat() {
			return ir.R, false
		}
		if c, ok := constInt(f, in.Args[0]); ok {
			return f.ConstInt(in.Cls, truncCls(in.Cls, -c)), true
		}

	case ir.OExtsw, ir.OExtuw, ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh:
		// Fold an integer width extension of a constant; this collapses the
		// constant-offset address arithmetic (extsw i; mul _,sz; add base,_) that a
		// front end emits for a[i], so alias analysis can resolve the elements.
		if c, ok := constInt(f, in.Args[0]); ok {
			var v int64
			switch in.Op {
			case ir.OExtsw:
				v = int64(int32(c))
			case ir.OExtuw:
				v = int64(uint32(c))
			case ir.OExtsb:
				v = int64(int8(c))
			case ir.OExtub:
				v = int64(uint8(c))
			case ir.OExtsh:
				v = int64(int16(c))
			case ir.OExtuh:
				v = int64(uint16(c))
			}
			return f.ConstInt(in.Cls, v), true
		}

	case ir.OCmp:
		a, b := in.Args[0], in.Args[1]
		if a == b && !in.Cmp.IsFloat() {
			return f.ConstInt(in.Cls, cmpReflexive(in.Cmp)), true
		}
		ca, aok := constInt(f, a)
		cb, bok := constInt(f, b)
		if aok && bok && !in.Cmp.IsFloat() {
			return f.ConstInt(in.Cls, evalCmp(in.Cmp, f.ClassOf(a), ca, cb)), true
		}
	}
	return ir.R, false
}

// binIdentity applies algebraic identities that need at most one constant.
func binIdentity(f *ir.Func, in *ir.Instr, a, b ir.Ref, ca int64, aok bool, cb int64, bok bool) (ir.Ref, bool) {
	zero := func() ir.Ref { return f.ConstInt(in.Cls, 0) }
	allOnes := int64(-1)
	switch in.Op {
	case ir.OAdd, ir.OOr, ir.OXor:
		if bok && cb == 0 {
			return a, true
		}
		if aok && ca == 0 {
			return b, true
		}
	case ir.OSub:
		if bok && cb == 0 {
			return a, true
		}
		if a == b {
			return zero(), true
		}
	case ir.OShl, ir.OShr, ir.OSar:
		if bok && cb == 0 {
			return a, true
		}
	case ir.OMul:
		if (bok && cb == 0) || (aok && ca == 0) {
			return zero(), true
		}
		if bok && cb == 1 {
			return a, true
		}
		if aok && ca == 1 {
			return b, true
		}
	case ir.ODiv, ir.OUDiv:
		if bok && cb == 1 {
			return a, true
		}
	case ir.OAnd:
		if (bok && cb == 0) || (aok && ca == 0) {
			return zero(), true
		}
		if bok && cb == allOnes {
			return a, true
		}
		if aok && ca == allOnes {
			return b, true
		}
		if a == b {
			return a, true
		}
	}
	if in.Op == ir.OOr && a == b {
		return a, true
	}
	return ir.R, false
}

// evalBinop evaluates an integer binary op on two constants at the given class.
// It returns ok=false when the operation is not statically defined (e.g. divide
// by zero), leaving the instruction in place.
func evalBinop(op ir.Op, cls ir.Cls, a, b int64) (int64, bool) {
	sa, sb := asS(cls, a), asS(cls, b)
	ua, ub := asU(cls, a), asU(cls, b)
	shift := uint(b) & shiftMask(cls)

	switch op {
	case ir.OAdd:
		return truncCls(cls, a+b), true
	case ir.OSub:
		return truncCls(cls, a-b), true
	case ir.OMul:
		return truncCls(cls, a*b), true
	case ir.OAnd:
		return truncCls(cls, a&b), true
	case ir.OOr:
		return truncCls(cls, a|b), true
	case ir.OXor:
		return truncCls(cls, a^b), true
	case ir.OShl:
		return truncCls(cls, a<<shift), true
	case ir.OShr:
		return truncCls(cls, int64(ua>>shift)), true
	case ir.OSar:
		return truncCls(cls, sa>>shift), true
	case ir.ODiv:
		if sb == 0 {
			return 0, false
		}
		if sa == math.MinInt64 && sb == -1 {
			return sa, true
		}
		return truncCls(cls, sa/sb), true
	case ir.ORem:
		if sb == 0 {
			return 0, false
		}
		if sa == math.MinInt64 && sb == -1 {
			return 0, true
		}
		return truncCls(cls, sa%sb), true
	case ir.OUDiv:
		if ub == 0 {
			return 0, false
		}
		return truncCls(cls, int64(ua/ub)), true
	case ir.OURem:
		if ub == 0 {
			return 0, false
		}
		return truncCls(cls, int64(ua%ub)), true
	}
	return 0, false
}

// evalCmp evaluates an integer comparison on two constants.
func evalCmp(pred ir.Cmp, argCls ir.Cls, a, b int64) int64 {
	sa, sb := asS(argCls, a), asS(argCls, b)
	ua, ub := asU(argCls, a), asU(argCls, b)
	var r bool
	switch pred {
	case ir.CmpEq:
		r = sa == sb
	case ir.CmpNe:
		r = sa != sb
	case ir.CmpSlt:
		r = sa < sb
	case ir.CmpSle:
		r = sa <= sb
	case ir.CmpSgt:
		r = sa > sb
	case ir.CmpSge:
		r = sa >= sb
	case ir.CmpUlt:
		r = ua < ub
	case ir.CmpUle:
		r = ua <= ub
	case ir.CmpUgt:
		r = ua > ub
	case ir.CmpUge:
		r = ua >= ub
	}
	return b2i(r)
}

// cmpReflexive folds a comparison of a value with itself.
func cmpReflexive(pred ir.Cmp) int64 {
	switch pred {
	case ir.CmpEq, ir.CmpSle, ir.CmpSge, ir.CmpUle, ir.CmpUge:
		return 1
	default:
		return 0
	}
}

func b2i(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func shiftMask(cls ir.Cls) uint {
	if cls == ir.ClsW {
		return 31
	}
	return 63
}

// asS interprets v as a signed value of the class width.
func asS(cls ir.Cls, v int64) int64 {
	if cls == ir.ClsW {
		return int64(int32(v))
	}
	return v
}

// asU interprets v as an unsigned value of the class width.
func asU(cls ir.Cls, v int64) uint64 {
	if cls == ir.ClsW {
		return uint64(uint32(v))
	}
	return uint64(v)
}

// truncCls narrows v to the class width, sign-extending words into int64.
func truncCls(cls ir.Cls, v int64) int64 {
	if cls == ir.ClsW {
		return int64(int32(v))
	}
	return v
}
