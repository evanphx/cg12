package cc

import (
	"math"

	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// builtinCall lowers the GCC/Clang compiler builtins that ordinary code and
// system libraries lean on, so the front end need not emit an out-of-line call
// to a nonexistent function. It reports whether it handled the call.
func (g *gen) builtinCall(n *cc.PostfixExpression) (ir.Ref, bool) {
	name := calleeIdent(n)
	args := builtinArgs(n)

	switch name {
	case "__builtin_expect", "__builtin_expect_with_probability":
		// A branch-prediction hint: evaluate to the first argument unchanged.
		if len(args) > 0 {
			return g.genExpr(args[0]), true
		}
		return ir.R, true

	case "__builtin_inff":
		return g.fn.Single(math.Inf(1)), true
	case "__builtin_inf", "__builtin_huge_val":
		return g.fn.Double(math.Inf(1)), true
	case "__builtin_huge_valf":
		return g.fn.Single(math.Inf(1)), true

	case "__builtin_bswap16":
		return g.bswap(g.genExpr(args[0]), 2), true
	case "__builtin_bswap32":
		return g.bswap(g.genExpr(args[0]), 4), true
	case "__builtin_bswap64":
		return g.bswap(g.genExpr(args[0]), 8), true

	case "__builtin_clz", "__builtin_clzl", "__builtin_clzll":
		cls := clsOf(args[0].Type())
		return g.cur.Clz(cls, g.genExpr(args[0])), true

	case "__atomic_load_n":
		elem := pointee(args[0].Type())
		return g.loadVal(g.genExpr(args[0]), elem), true
	case "__atomic_store_n":
		elem := pointee(args[0].Type())
		addr := g.genExpr(args[0])
		val := g.convert(g.genExpr(args[1]), args[1].Type(), elem)
		g.storeVal(addr, val, elem)
		return ir.R, true

	case "__builtin_add_overflow", "__builtin_sub_overflow", "__builtin_mul_overflow":
		return g.overflowBuiltin(name, args), true
	}
	return ir.R, false
}

// builtinArgs collects a call's argument expression nodes, head first.
func builtinArgs(n *cc.PostfixExpression) []cc.ExpressionNode {
	var out []cc.ExpressionNode
	for l := n.ArgumentExpressionList; l != nil; l = l.ArgumentExpressionList {
		out = append(out, l.AssignmentExpression)
	}
	return out
}

// pointee returns the type a pointer points at, or int as a harmless fallback.
func pointee(t cc.Type) cc.Type {
	if pt, ok := t.(*cc.PointerType); ok {
		return pt.Elem()
	}
	return t
}

// bswap reverses the byte order of v, an integer of the given width, using
// portable shift/mask arithmetic.
func (g *gen) bswap(v ir.Ref, width int) ir.Ref {
	cls := ir.ClsW
	if width == 8 {
		cls = ir.ClsL
	}
	mask := func(x ir.Ref, m int64) ir.Ref { return g.cur.And(cls, x, g.fn.ConstInt(cls, m)) }
	shl := func(x ir.Ref, s int64) ir.Ref { return g.cur.Shl(cls, x, g.fn.ConstInt(cls, s)) }
	shr := func(x ir.Ref, s int64) ir.Ref { return g.cur.Shr(cls, x, g.fn.ConstInt(cls, s)) }
	or := func(a, b ir.Ref) ir.Ref { return g.cur.Or(cls, a, b) }

	switch width {
	case 2:
		return or(shl(mask(v, 0xff), 8), mask(shr(v, 8), 0xff))
	case 4:
		return or(
			or(shl(mask(v, 0xff), 24), shl(mask(v, 0xff00), 8)),
			or(mask(shr(v, 8), 0xff00), mask(shr(v, 24), 0xff)),
		)
	default: // 8
		r := shl(mask(v, 0xff), 56)
		r = or(r, shl(mask(v, 0xff00), 40))
		r = or(r, shl(mask(v, 0xff0000), 24))
		r = or(r, shl(mask(v, 0xff000000), 8))
		r = or(r, mask(shr(v, 8), 0xff000000))
		r = or(r, mask(shr(v, 24), 0xff0000))
		r = or(r, mask(shr(v, 40), 0xff00))
		r = or(r, mask(shr(v, 56), 0xff))
		return r
	}
}

// overflowBuiltin lowers __builtin_{add,sub,mul}_overflow(a, b, res): it computes
// the wrapped result, stores it through res, and returns 1 when the true result
// would not fit the (signed) result type, 0 otherwise. The overflow test is
// branchless.
func (g *gen) overflowBuiltin(name string, args []cc.ExpressionNode) ir.Ref {
	elem := pointee(args[2].Type())
	cls := clsOf(elem)
	a := g.convert(g.genExpr(args[0]), args[0].Type(), elem)
	b := g.convert(g.genExpr(args[1]), args[1].Type(), elem)
	res := g.genExpr(args[2])

	zero := g.fn.ConstInt(cls, 0)
	neg := func(x ir.Ref) ir.Ref { return g.cur.Cmp(ir.CmpSlt, ir.ClsW, x, zero) } // x < 0 ? 1 : 0

	var result, ovf ir.Ref
	switch name {
	case "__builtin_add_overflow":
		result = g.cur.Add(cls, a, b)
		// Overflow iff a and b share a sign that differs from the sum's.
		ovf = neg(g.cur.And(cls, g.cur.Xor(cls, a, result), g.cur.Xor(cls, b, result)))
	case "__builtin_sub_overflow":
		result = g.cur.Sub(cls, a, b)
		// Overflow iff a and b differ in sign and the result's sign differs from a's.
		ovf = neg(g.cur.And(cls, g.cur.Xor(cls, a, b), g.cur.Xor(cls, a, result)))
	default: // __builtin_mul_overflow
		result = g.cur.Mul(cls, a, b)
		// Overflow iff b != 0 and result/b != a (division does not trap on this
		// target), plus the one case that check misses: MIN * -1.
		bNZ := g.cur.Cmp(ir.CmpNe, ir.ClsW, b, zero)
		q := g.cur.Div(cls, result, b)
		mism := g.cur.Cmp(ir.CmpNe, ir.ClsW, q, a)
		ovf = g.cur.And(ir.ClsW, bNZ, mism)
		minv := g.fn.ConstInt(cls, int64(-1)<<uint(elem.Size()*8-1)) // most-negative value
		isMin := g.cur.Cmp(ir.CmpEq, ir.ClsW, a, minv)
		isNeg1 := g.cur.Cmp(ir.CmpEq, ir.ClsW, b, g.fn.ConstInt(cls, -1))
		ovf = g.cur.Or(ir.ClsW, ovf, g.cur.And(ir.ClsW, isMin, isNeg1))
	}
	g.storeVal(res, result, elem)
	return ovf
}
