package cc

import (
	"math"
	"strings"

	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// builtinCall lowers the GCC/Clang compiler builtins that ordinary code and
// system libraries lean on, so the front end need not emit an out-of-line call
// to a nonexistent function. It reports whether it handled the call.
func (g *gen) builtinCall(n *moderncc.PostfixExpression) (ir.Ref, bool) {
	name := calleeIdent(n)
	args := builtinArgs(n)

	switch name {
	case "__builtin_expect", "__builtin_expect_with_probability":
		// A branch-prediction hint: evaluate to the first argument unchanged.
		if len(args) > 0 {
			return g.genExpr(args[0]), true
		}
		return ir.R, true

	case "__builtin_choose_expr":
		// A compile-time ?: -- the condition is a constant, and only the chosen arm
		// is evaluated (the other need not even be valid). Emit the chosen arm.
		if len(args) == 3 {
			if c, ok := constInt(args[0]); ok {
				if c != 0 {
					return g.genExpr(args[1]), true
				}
				return g.genExpr(args[2]), true
			}
		}
		return ir.R, false

	case "__builtin_rotateleft8", "__builtin_rotateleft16", "__builtin_rotateleft32", "__builtin_rotateleft64",
		"__builtin_rotateright8", "__builtin_rotateright16", "__builtin_rotateright32", "__builtin_rotateright64":
		if len(args) == 2 {
			return g.rotate(name, args[0], args[1]), true
		}
		return ir.R, false

	case "__builtin___clear_cache":
		// Flush the instruction cache for a range -- only meaningful after writing
		// code to execute (a JIT). cg12 does not emit the flush yet; a no-op is
		// correct for everything that is not running freshly generated machine code.
		return ir.R, true

	case "__builtin_unreachable", "__builtin_trap":
		// The program asserts control never reaches here. Terminate the block as
		// unreachable (a trap) rather than emitting a call to a nonexistent
		// function; it yields no value.
		g.cur.Hlt()
		return ir.R, true

	case "__builtin_inff":
		return g.fn.Single(math.Inf(1)), true
	case "__builtin_inf", "__builtin_huge_val":
		return g.fn.Double(math.Inf(1)), true
	case "__builtin_huge_valf":
		return g.fn.Single(math.Inf(1)), true

	case "__builtin_nanf":
		return g.fn.Single(math.NaN()), true
	case "__builtin_nan", "__builtin_nanl":
		// nanl would be a long double; the payload string argument is ignored (a
		// quiet NaN), and a double NaN widens to it without loss.
		return g.fn.Double(math.NaN()), true

	case "__builtin_bswap16":
		return g.bswap(g.genExpr(args[0]), 2), true
	case "__builtin_bswap32":
		return g.bswap(g.genExpr(args[0]), 4), true
	case "__builtin_bswap64":
		return g.bswap(g.genExpr(args[0]), 8), true

	case "__builtin_clz", "__builtin_clzl", "__builtin_clzll":
		cls := clsOf(args[0].Type())
		return g.cur.Clz(cls, g.genExpr(args[0])), true

	case "__builtin_frame_address", "__builtin_return_address":
		// Only level 0 is meaningful here; callers use it (js_get_stack_pointer)
		// to sample the current stack depth, which the live stack pointer answers.
		return g.cur.StackSave(), true

	case "__builtin_alloca", "__builtin_alloca_with_align":
		// A dynamic stack allocation, reclaimed when the function returns. AllocN
		// needs a 16-byte multiple, so round the size up.
		cls := clsOf(args[0].Type())
		sz := g.genExpr(args[0])
		sz = g.cur.And(cls, g.cur.Add(cls, sz, g.fn.ConstInt(cls, 15)), g.fn.ConstInt(cls, -16))
		return g.cur.AllocN(sz), true

	case "__builtin_isnan":
		// NaN is the only value unordered with itself.
		return g.cur.Cmp(ir.CmpFuo, ir.ClsW, g.genExpr(args[0]), g.genExpr(args[0])), true

	case "__builtin_isinf", "__builtin_isinf_sign":
		// isinf is nonzero for either infinity; isinf_sign is +1 for +inf and -1
		// for -inf. Both fall out of comparing against the two infinities.
		x := g.genExpr(args[0])
		pinf, ninf := g.inf(args[0].Type(), 1), g.inf(args[0].Type(), -1)
		p := g.cur.Cmp(ir.CmpFeq, ir.ClsW, x, pinf)
		n := g.cur.Cmp(ir.CmpFeq, ir.ClsW, x, ninf)
		if name == "__builtin_isinf" {
			return g.cur.Or(ir.ClsW, p, n), true
		}
		return g.cur.Sub(ir.ClsW, p, n), true

	case "__builtin_constant_p":
		// Whether the argument is a compile-time constant. Reporting "no" is always
		// sound: it only steers the caller to the runtime path.
		return g.fn.Word(0), true

	case "__builtin_popcount", "__builtin_popcountl", "__builtin_popcountll":
		return g.popcount(g.genExpr(args[0]), clsOf(args[0].Type())), true

	case "__builtin_ctz", "__builtin_ctzl", "__builtin_ctzll":
		// Count trailing zeros: isolate the lowest set bit (x & -x) and count the
		// leading zeros of that. Like clz, undefined for zero.
		cls := clsOf(args[0].Type())
		x := g.genExpr(args[0])
		low := g.cur.And(cls, x, g.cur.Neg(cls, x))
		width := int64(cls.Size()) * 8
		return g.cur.Sub(cls, g.fn.ConstInt(cls, width-1), g.cur.Clz(cls, low)), true

	case "__builtin_isfinite":
		// x - x is 0 for a finite x, and NaN for an infinity or NaN, so the ordered
		// compare against 0 is true only when x is finite.
		cls := clsOf(args[0].Type())
		x := g.genExpr(args[0])
		diff := g.cur.Sub(cls, x, x)
		zero := g.fn.Double(0)
		if cls == ir.ClsS {
			zero = g.fn.Single(0)
		}
		return g.cur.Cmp(ir.CmpFeq, ir.ClsW, diff, zero), true

	case "__builtin_signbit", "__builtin_signbitf":
		// The sign bit is the top bit of the value's bit pattern, so reinterpret it
		// as an integer and test whether that is negative.
		cls := clsOf(args[0].Type())
		intCls := ir.ClsL
		if cls == ir.ClsS {
			intCls = ir.ClsW
		}
		bits := g.cur.Cast(intCls, g.genExpr(args[0]))
		return g.cur.Cmp(ir.CmpSlt, ir.ClsW, bits, g.fn.ConstInt(intCls, 0)), true

	case "__atomic_fetch_add", "__atomic_fetch_sub", "__atomic_fetch_and",
		"__atomic_fetch_or", "__atomic_fetch_xor":
		return g.atomicFetchOp(strings.TrimPrefix(name, "__atomic_fetch_"), args, false), true
	case "__atomic_add_fetch", "__atomic_sub_fetch", "__atomic_and_fetch",
		"__atomic_or_fetch", "__atomic_xor_fetch":
		op := strings.TrimSuffix(strings.TrimPrefix(name, "__atomic_"), "_fetch")
		return g.atomicFetchOp(op, args, true), true

	case "__atomic_load_n":
		return g.atomicLoad(args[0]), true
	case "__atomic_store_n":
		g.atomicStore(args[0], args[1])
		return ir.R, true
	case "__atomic_exchange_n":
		return g.atomicXchg(args[0], args[1]), true
	case "__atomic_compare_exchange_n":
		return g.atomicCAS(args[0], args[1], args[2]), true

	// The generic forms pass the value and result by pointer rather than by value.
	case "__atomic_load": // (ptr, ret_ptr, order): *ret_ptr = atomic load *ptr
		suf, cls, width, ok := atomicWidth(args[0])
		if !ok {
			g.atomicUnsupported(name)
			return ir.R, true
		}
		v := g.cur.Intrinsic("atomic.load."+suf, cls, g.genExpr(args[0]))
		g.storeWidth(width, v, g.genExpr(args[1]))
		return ir.R, true
	case "__atomic_store": // (ptr, val_ptr, order): *ptr = *val_ptr
		suf, _, width, ok := atomicWidth(args[0])
		if !ok {
			g.atomicUnsupported(name)
			return ir.R, true
		}
		ptr := g.genExpr(args[0])
		g.cur.IntrinsicVoid("atomic.store."+suf, ptr, g.loadWidth(width, g.genExpr(args[1])))
		return ir.R, true
	case "__atomic_exchange": // (ptr, val_ptr, ret_ptr, order)
		suf, cls, width, ok := atomicWidth(args[0])
		if !ok {
			g.atomicUnsupported(name)
			return ir.R, true
		}
		ptr := g.genExpr(args[0])
		v := g.loadWidth(width, g.genExpr(args[1]))
		old := g.cur.Intrinsic("atomic.xchg."+suf, cls, ptr, v)
		g.storeWidth(width, old, g.genExpr(args[2]))
		return ir.R, true
	case "__atomic_compare_exchange": // (ptr, expected_ptr, desired_ptr, weak, s, f)
		suf, cls, width, ok := atomicWidth(args[0])
		if !ok {
			g.atomicUnsupported(name)
			return ir.R, true
		}
		ptr := g.genExpr(args[0])
		expPtr := g.genExpr(args[1])
		exp := g.loadWidth(width, expPtr)
		old := g.cur.Intrinsic("atomic.cas."+suf, cls, ptr, exp, g.loadWidth(width, g.genExpr(args[2])))
		g.storeWidth(width, old, expPtr) // *expected = old (a no-op on success)
		return g.cur.Cmp(ir.CmpEq, ir.ClsW, old, exp), true

	case "__atomic_thread_fence", "__atomic_signal_fence":
		g.cur.IntrinsicVoid("atomic.fence")
		return ir.R, true

	case "__builtin_add_overflow", "__builtin_sub_overflow", "__builtin_mul_overflow":
		return g.overflowBuiltin(name, args, false), true
	case "__builtin_add_overflow_p", "__builtin_sub_overflow_p", "__builtin_mul_overflow_p":
		// The _p forms only ask whether the operation overflows the third argument's
		// type; they compute no result and store nothing.
		return g.overflowBuiltin(name, args, true), true

	case "__builtin_prefetch":
		// A cache hint with no value and no observable effect here.
		return ir.R, true

	case "__builtin_assume_aligned":
		// Asserts the pointer's alignment and evaluates to it unchanged.
		if len(args) > 0 {
			return g.genExpr(args[0]), true
		}
		return ir.R, true
	}
	return ir.R, false
}

// builtinArgs collects a call's argument expression nodes, head first.
func builtinArgs(n *moderncc.PostfixExpression) []moderncc.ExpressionNode {
	var out []moderncc.ExpressionNode
	for l := n.ArgumentExpressionList; l != nil; l = l.ArgumentExpressionList {
		out = append(out, l.AssignmentExpression)
	}
	return out
}

// pointee returns the type a pointer points at, or int as a harmless fallback.
func pointee(t moderncc.Type) moderncc.Type {
	if pt, ok := t.(*moderncc.PointerType); ok {
		return pt.Elem()
	}
	return t
}

// bswap reverses the byte order of v, an integer of the given width, using
// portable shift/mask arithmetic.
// rotate lowers __builtin_rotateleft/right<N>(x, n) with shifts, since cg12's
// rotate primitive takes only a constant amount. The count is reduced modulo the
// width, so a zero rotate shifts by zero rather than by the width (which is
// undefined). Sub-word widths mask the operand and result to their bits.
func (g *gen) rotate(name string, xe, ne moderncc.ExpressionNode) ir.Ref {
	var width int64 = 64
	switch {
	case strings.HasSuffix(name, "8"):
		width = 8
	case strings.HasSuffix(name, "16"):
		width = 16
	case strings.HasSuffix(name, "32"):
		width = 32
	}
	left := strings.Contains(name, "left")
	cls := ir.ClsW
	if width == 64 {
		cls = ir.ClsL
	}
	x := g.toCls(g.genExpr(xe), cls)
	if width < 32 {
		x = g.cur.And(cls, x, g.fn.ConstInt(cls, (1<<width)-1))
	}
	mask := g.fn.ConstInt(cls, width-1)
	n := g.cur.And(cls, g.toCls(g.genExpr(ne), cls), mask)
	negn := g.cur.And(cls, g.cur.Sub(cls, g.fn.ConstInt(cls, 0), n), mask)
	shl, shr := n, negn // rotate left: (x << n) | (x >> (width-n))
	if !left {
		shl, shr = negn, n // rotate right: (x >> n) | (x << (width-n))
	}
	r := g.cur.Or(cls, g.cur.Shl(cls, x, shl), g.cur.Shr(cls, x, shr))
	if width < 32 {
		r = g.cur.And(cls, r, g.fn.ConstInt(cls, (1<<width)-1))
	}
	return r
}

// popcount counts set bits with the classic SWAR sequence (cg12 has no popcount
// primitive): pairwise sums that fold up to a single byte, extracted by a
// multiply. Works for a 32- or 64-bit operand.
func (g *gen) popcount(x ir.Ref, cls ir.Cls) ir.Ref {
	width := int64(cls.Size()) * 8
	c := func(v int64) ir.Ref { return g.fn.ConstInt(cls, v) }
	m1, m2, m4, h01 := int64(0x55555555), int64(0x33333333), int64(0x0f0f0f0f), int64(0x01010101)
	if width == 64 {
		m1, m2, m4, h01 = 0x5555555555555555, 0x3333333333333333, 0x0f0f0f0f0f0f0f0f, 0x0101010101010101
	}
	x = g.cur.Sub(cls, x, g.cur.And(cls, g.cur.Shr(cls, x, c(1)), c(m1)))
	x = g.cur.Add(cls, g.cur.And(cls, x, c(m2)), g.cur.And(cls, g.cur.Shr(cls, x, c(2)), c(m2)))
	x = g.cur.And(cls, g.cur.Add(cls, x, g.cur.Shr(cls, x, c(4))), c(m4))
	return g.cur.Shr(cls, g.cur.Mul(cls, x, c(h01)), c(width-8))
}

// inf returns a floating infinity constant of t's class (single or double), with
// the given sign (+1 or -1).
func (g *gen) inf(t moderncc.Type, sign int) ir.Ref {
	if clsOf(t) == ir.ClsS {
		return g.fn.Single(math.Inf(sign))
	}
	return g.fn.Double(math.Inf(sign))
}

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
// would not fit the result type, 0 otherwise. The overflow test is branchless.
//
// The result type's signedness picks the test: the signed and unsigned forms are
// entirely different questions, and asking the signed one about unsigned operands
// answers no for every value that only overflows unsigned.
func (g *gen) overflowBuiltin(name string, args []moderncc.ExpressionNode, predicate bool) ir.Ref {
	// The _p forms carry the result type directly in the third argument; the
	// storing forms carry a pointer to it. Only the storing forms evaluate it.
	var elem moderncc.Type
	if predicate {
		name = strings.TrimSuffix(name, "_p")
		elem = args[2].Type()
	} else {
		elem = pointee(args[2].Type())
	}
	cls := clsOf(elem)
	a := g.convert(g.genExpr(args[0]), args[0].Type(), elem)
	b := g.convert(g.genExpr(args[1]), args[1].Type(), elem)

	zero := g.fn.ConstInt(cls, 0)
	neg := func(x ir.Ref) ir.Ref { return g.cur.Cmp(ir.CmpSlt, ir.ClsW, x, zero) } // x < 0 ? 1 : 0

	var result, ovf ir.Ref
	switch name {
	case "__builtin_add_overflow":
		result = g.cur.Add(cls, a, b)
		if signed(elem) {
			// Overflow iff a and b share a sign that differs from the sum's.
			ovf = neg(g.cur.And(cls, g.cur.Xor(cls, a, result), g.cur.Xor(cls, b, result)))
		} else {
			// Unsigned addition wraps iff the sum came out below either operand.
			ovf = g.cur.Cmp(ir.CmpUlt, ir.ClsW, result, a)
		}
	case "__builtin_sub_overflow":
		result = g.cur.Sub(cls, a, b)
		if signed(elem) {
			// Overflow iff a and b differ in sign and the result's sign differs from a's.
			ovf = neg(g.cur.And(cls, g.cur.Xor(cls, a, b), g.cur.Xor(cls, a, result)))
		} else {
			// Unsigned subtraction wraps iff it borrowed, i.e. a was the smaller.
			ovf = g.cur.Cmp(ir.CmpUlt, ir.ClsW, a, b)
		}
	default: // __builtin_mul_overflow
		result = g.cur.Mul(cls, a, b)
		// Overflow iff b != 0 and result/b != a (division does not trap on this
		// target). The division has to match the operands' signedness.
		bNZ := g.cur.Cmp(ir.CmpNe, ir.ClsW, b, zero)
		var q ir.Ref
		if signed(elem) {
			q = g.cur.Div(cls, result, b)
		} else {
			q = g.cur.UDiv(cls, result, b)
		}
		mism := g.cur.Cmp(ir.CmpNe, ir.ClsW, q, a)
		ovf = g.cur.And(ir.ClsW, bNZ, mism)
		if signed(elem) {
			// The one case the division check misses: MIN * -1 divides back cleanly.
			minv := g.fn.ConstInt(cls, int64(-1)<<uint(elem.Size()*8-1)) // most-negative value
			isMin := g.cur.Cmp(ir.CmpEq, ir.ClsW, a, minv)
			isNeg1 := g.cur.Cmp(ir.CmpEq, ir.ClsW, b, g.fn.ConstInt(cls, -1))
			ovf = g.cur.Or(ir.ClsW, ovf, g.cur.And(ir.ClsW, isMin, isNeg1))
		}
	}
	if !predicate {
		g.storeVal(g.genExpr(args[2]), result, elem)
	}
	return ovf
}
