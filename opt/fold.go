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
	def := defMap(f)
	s := subst{}
	changed := false
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op == ir.OCmp && len(in.Args) > 0 && f.ClassOf(in.Args[0]).IsFloat() && !in.Cmp.IsFloat() {
				in.Cmp = floatingComparison(in.Cmp)
				changed = true
			}
			if in.To.IsNone() {
				continue
			}
			if r, ok := foldInstr(f, def, in); ok {
				if f.ClassOf(r) != in.Cls {
					continue
				}
				s[in.To.ID] = r
				b.Instrs[i] = ir.Instr{Op: ir.ONop}
				changed = true
				continue
			}
			if strengthReduce(f, in) {
				changed = true
			}
		}
	}
	for _, b := range f.Blocks {
		if foldBranch(f, def, b) {
			changed = true
		}
	}
	if changed {
		applySubst(f, s)
		removeNops(f)
	}
	return changed
}

// defMap indexes each temporary to its defining instruction, so a fold can look
// at how an operand was produced (e.g. whether it is already a 0/1 boolean).
func defMap(f *ir.Func) map[uint32]*ir.Instr {
	def := map[uint32]*ir.Instr{}
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if in := &b.Instrs[i]; in.To.Kind == ir.RefTemp {
				def[in.To.ID] = in
			}
		}
	}
	return def
}

// isBoolean reports whether r is known to hold 0 or 1. A comparison result is the
// common case; an AND against 1 also is. This lets a redundant compare-against-zero
// on an already-boolean value be dropped rather than re-materialized.
func isBoolean(f *ir.Func, def map[uint32]*ir.Instr, r ir.Ref) bool {
	if r.Kind != ir.RefTemp {
		return false
	}
	d := def[r.ID]
	if d == nil {
		return false
	}
	switch d.Op {
	case ir.OCmp:
		return true
	case ir.OAnd:
		if c, ok := constInt(f, d.Args[0]); ok && c == 1 {
			return true
		}
		if c, ok := constInt(f, d.Args[1]); ok && c == 1 {
			return true
		}
	}
	return false
}

// zeroCmpOperand returns the non-zero operand x of a compare against a zero
// constant (x != 0 or x == 0), along with the predicate. It is the hook for
// collapsing the boolean re-tests a front end emits for !, &&, and ||.
func zeroCmpOperand(f *ir.Func, in *ir.Instr) (ir.Ref, ir.Cmp, bool) {
	if in.Op != ir.OCmp || (in.Cmp != ir.CmpEq && in.Cmp != ir.CmpNe) {
		return ir.R, 0, false
	}
	if c, ok := constInt(f, in.Args[1]); ok && c == 0 {
		return in.Args[0], in.Cmp, true
	}
	if c, ok := constInt(f, in.Args[0]); ok && c == 0 {
		return in.Args[1], in.Cmp, true
	}
	return ir.R, 0, false
}

// foldBranch rewrites a conditional branch that tests a compare-against-zero so
// it tests the compared value directly: jnz (a != 0) -> jnz a, and jnz (a == 0)
// -> jnz a with the arms swapped. This is valid for any a (the branch only asks
// whether the condition is non-zero), and it lets the backend fuse the real
// comparison, or the value itself, into the branch instead of materializing an
// intermediate boolean. It collapses the cnew/ceqw chains produced by !, &&, ||.
func foldBranch(f *ir.Func, def map[uint32]*ir.Instr, b *ir.Block) bool {
	if b.Jmp.Kind != ir.JmpJnz || b.Jmp.Arg.Kind != ir.RefTemp {
		return false
	}
	d := def[b.Jmp.Arg.ID]
	if d == nil {
		return false
	}
	x, pred, ok := zeroCmpOperand(f, d)
	if !ok {
		return false
	}
	b.Jmp.Arg = x
	if pred == ir.CmpEq {
		b.Jmp.To, b.Jmp.To2 = b.Jmp.To2, b.Jmp.To
	}
	return true
}

// strengthReduce rewrites a multiply by a power of two into a left shift, in
// place (x * 2^k -> x << k). The two are identical in two's complement for every
// x, and a shift is cheaper than a multiply on every target. It returns whether
// it rewrote in.
func strengthReduce(f *ir.Func, in *ir.Instr) bool {
	if in.Op != ir.OMul || in.Cls.IsFloat() {
		return false
	}
	a, b := in.Args[0], in.Args[1]
	if cb, ok := constInt(f, b); ok && isPow2(cb) {
		in.Op = ir.OShl
		in.Args = []ir.Ref{a, f.ConstInt(in.Cls, log2(cb))}
		return true
	}
	if ca, ok := constInt(f, a); ok && isPow2(ca) {
		in.Op = ir.OShl
		in.Args = []ir.Ref{b, f.ConstInt(in.Cls, log2(ca))}
		return true
	}
	return false
}

// isPow2 reports whether v is a power of two greater than one.
func isPow2(v int64) bool { return v > 1 && v&(v-1) == 0 }

// log2 returns the exponent of a power of two.
func log2(v int64) int64 {
	n := int64(0)
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

// foldInstr returns the replacement reference for an instruction, if any.
func foldInstr(f *ir.Func, def map[uint32]*ir.Instr, in *ir.Instr) (ir.Ref, bool) {
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

	case ir.OSel:
		// A constant condition selects one arm; identical arms collapse.
		if c, ok := constInt(f, in.Args[0]); ok {
			if c != 0 {
				return in.Args[1], true
			}
			return in.Args[2], true
		}
		if in.Args[1] == in.Args[2] {
			return in.Args[1], true
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
		// (bool != 0) is just bool: when a value is already 0/1, comparing it
		// against zero re-materializes what it already is. Requires matching class
		// so the result stays well-typed.
		if in.Cmp == ir.CmpNe {
			if x, _, ok := zeroCmpOperand(f, in); ok && isBoolean(f, def, x) && f.ClassOf(x) == in.Cls {
				return x, true
			}
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
