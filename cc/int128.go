package cc

import (
	"math/big"

	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// A __int128 / unsigned __int128 value is modelled as a two-element {lo, hi}
// aggregate of 64-bit halves, so it travels by value through the same
// pointer-to-storage path as a struct. A 16-byte integer aggregate is passed and
// returned in a pair of general-purpose registers under both the System V and
// AAPCS64 rules, matching how those ABIs carry a 128-bit integer -- so the
// libgcc helpers (__multi3, __divti3, ...) interoperate. Add, subtract, the
// bitwise operators, negation, comparison, and narrowing/widening conversions
// are lowered here to 64-bit half operations; multiply, divide, remainder,
// shifts, and float conversions defer to the libgcc __*ti3 helpers.
//
// (A caveat: a 16-byte {long,long} aggregate has 8-byte alignment where a real
// __int128 has 16, so a mixed argument list can disagree on register assignment
// with a gcc-compiled callee. All-128-bit signatures -- including every libgcc
// helper used here -- agree, and self-consistent cg12 code is unaffected.)

// isInt128 reports whether t is a signed or unsigned 128-bit integer.
func isInt128(t moderncc.Type) bool {
	switch t.Kind() {
	case moderncc.Int128, moderncc.UInt128:
		return true
	}
	return false
}

// int128Agg returns (and memoizes) the {lo,hi} aggregate type modelling a 128-bit
// integer, so it rides the two-register integer path of the aggregate ABI.
func (g *gen) int128Agg() *ir.AggType {
	if g.ti128 == nil {
		g.ti128 = &ir.AggType{Name: "int128", Fields: []ir.Field{{Sub: ir.SubL}, {Sub: ir.SubL}}}
		g.mod.AddType(g.ti128)
	}
	return g.ti128
}

// int128Result stores halves lo, hi into a fresh 16-byte slot and returns its
// address (the value of a 128-bit expression).
func (g *gen) int128Result(lo, hi ir.Ref) ir.Ref {
	addr := g.cur.Alloc(16, 16)
	g.cur.Store(lo, addr)
	g.cur.Store(hi, g.offset(addr, 8))
	return addr
}

// int128Parts loads the lo and hi halves of operand e. A narrower integer is
// widened: its value fills lo and its sign (0 when unsigned) fills hi.
func (g *gen) int128Parts(e moderncc.ExpressionNode) (lo, hi ir.Ref) {
	if isInt128(e.Type()) {
		addr := g.genExpr(e)
		return g.loadParts(addr, e.Type())
	}
	return g.widenTo128(g.genExpr(e), e.Type())
}

// loadParts reads the {lo,hi} halves from a 128-bit value's storage.
func (g *gen) loadParts(addr ir.Ref, _ moderncc.Type) (lo, hi ir.Ref) {
	lo = g.cur.Load(ir.ClsL, addr)
	hi = g.cur.Load(ir.ClsL, g.offset(addr, 8))
	return
}

// widenTo128 extends an integer value v of type from into 128-bit halves.
func (g *gen) widenTo128(v ir.Ref, from moderncc.Type) (lo, hi ir.Ref) {
	lo = g.to64(v, from)
	if signed(from) {
		hi = g.cur.Sar(ir.ClsL, lo, g.fn.Long(63)) // replicate the sign bit
	} else {
		hi = g.fn.Long(0)
	}
	return
}

// to64 widens an integer value of type from to a 64-bit half (a no-op for a
// value already 64-bit wide).
func (g *gen) to64(v ir.Ref, from moderncc.Type) ir.Ref {
	if wide(clsOf(from)) {
		return v
	}
	if signed(from) {
		return g.cur.Extsw(ir.ClsL, v)
	}
	return g.cur.Extuw(ir.ClsL, v)
}

// int128Operand yields the address of operand e as a 128-bit value, widening a
// narrower integer into a fresh slot.
func (g *gen) int128Operand(e moderncc.ExpressionNode) ir.Ref {
	if isInt128(e.Type()) {
		return g.genExpr(e)
	}
	lo, hi := g.widenTo128(g.genExpr(e), e.Type())
	return g.int128Result(lo, hi)
}

// int128Arith lowers a 128-bit +, -, *, /, %, <<, >>, &, |, or ^, returning the
// address of the result.
func (g *gen) int128Arith(op string, ln, rn moderncc.ExpressionNode, resT moderncc.Type) ir.Ref {
	uns := !signed(resT)
	switch op {
	case "*":
		return g.ti3("__multi3", g.int128Operand(ln), g.int128Operand(rn))
	case "/":
		if uns {
			return g.ti3("__udivti3", g.int128Operand(ln), g.int128Operand(rn))
		}
		return g.ti3("__divti3", g.int128Operand(ln), g.int128Operand(rn))
	case "%":
		if uns {
			return g.ti3("__umodti3", g.int128Operand(ln), g.int128Operand(rn))
		}
		return g.ti3("__modti3", g.int128Operand(ln), g.int128Operand(rn))
	case "<<":
		return g.ti3Shift("__ashlti3", ln, rn)
	case ">>":
		if uns {
			return g.ti3Shift("__lshrti3", ln, rn)
		}
		return g.ti3Shift("__ashrti3", ln, rn)
	}

	alo, ahi := g.int128Parts(ln)
	blo, bhi := g.int128Parts(rn)
	b := g.cur
	switch op {
	case "+":
		lo := b.Add(ir.ClsL, alo, blo)
		carry := g.ext1(b.Cmp(ir.CmpUlt, ir.ClsW, lo, alo)) // lo wrapped => carry out
		hi := b.Add(ir.ClsL, b.Add(ir.ClsL, ahi, bhi), carry)
		return g.int128Result(lo, hi)
	case "-":
		lo := b.Sub(ir.ClsL, alo, blo)
		borrow := g.ext1(b.Cmp(ir.CmpUlt, ir.ClsW, alo, blo)) // alo < blo => borrow
		hi := b.Sub(ir.ClsL, b.Sub(ir.ClsL, ahi, bhi), borrow)
		return g.int128Result(lo, hi)
	case "&":
		return g.int128Result(b.And(ir.ClsL, alo, blo), b.And(ir.ClsL, ahi, bhi))
	case "|":
		return g.int128Result(b.Or(ir.ClsL, alo, blo), b.Or(ir.ClsL, ahi, bhi))
	case "^":
		return g.int128Result(b.Xor(ir.ClsL, alo, blo), b.Xor(ir.ClsL, ahi, bhi))
	}
	return g.fail("cc: operator %q is not defined for __int128", op)
}

// ext1 zero-extends a 0/1 comparison word to a 64-bit half.
func (g *gen) ext1(v ir.Ref) ir.Ref { return g.cur.Extuw(ir.ClsL, v) }

// int128Neg lowers unary minus: 0 - a.
func (g *gen) int128Neg(e moderncc.ExpressionNode) ir.Ref {
	alo, ahi := g.int128Parts(e)
	b := g.cur
	lo := b.Sub(ir.ClsL, g.fn.Long(0), alo)
	borrow := g.ext1(b.Cmp(ir.CmpNe, ir.ClsW, alo, g.fn.Long(0))) // any low bits => borrow
	hi := b.Sub(ir.ClsL, b.Sub(ir.ClsL, g.fn.Long(0), ahi), borrow)
	return g.int128Result(lo, hi)
}

// int128Cpl lowers ~a: complement both halves.
func (g *gen) int128Cpl(e moderncc.ExpressionNode) ir.Ref {
	alo, ahi := g.int128Parts(e)
	b := g.cur
	return g.int128Result(b.Xor(ir.ClsL, alo, g.fn.Long(-1)), b.Xor(ir.ClsL, ahi, g.fn.Long(-1)))
}

// int128Compare lowers a 128-bit relational or equality comparison to a 0/1 int.
// Equality tests both halves; ordering compares the high halves (signed for a
// signed type) and, when they are equal, the low halves (always unsigned).
func (g *gen) int128Compare(op string, ln, rn moderncc.ExpressionNode, signedCmp bool) ir.Ref {
	alo, ahi := g.int128Parts(ln)
	blo, bhi := g.int128Parts(rn)
	b := g.cur
	switch op {
	case "==":
		return b.And(ir.ClsW, b.Cmp(ir.CmpEq, ir.ClsW, alo, blo), b.Cmp(ir.CmpEq, ir.ClsW, ahi, bhi))
	case "!=":
		return b.Or(ir.ClsW, b.Cmp(ir.CmpNe, ir.ClsW, alo, blo), b.Cmp(ir.CmpNe, ir.ClsW, ahi, bhi))
	}
	var hiPred, loPred ir.Cmp
	switch op {
	case "<":
		hiPred, loPred = pick(signedCmp, ir.CmpSlt, ir.CmpUlt), ir.CmpUlt
	case "<=":
		hiPred, loPred = pick(signedCmp, ir.CmpSlt, ir.CmpUlt), ir.CmpUle
	case ">":
		hiPred, loPred = pick(signedCmp, ir.CmpSgt, ir.CmpUgt), ir.CmpUgt
	case ">=":
		hiPred, loPred = pick(signedCmp, ir.CmpSgt, ir.CmpUgt), ir.CmpUge
	default:
		return g.fail("cc: bad __int128 comparison %q", op)
	}
	hiOrd := b.Cmp(hiPred, ir.ClsW, ahi, bhi)
	hiEq := b.Cmp(ir.CmpEq, ir.ClsW, ahi, bhi)
	loOrd := b.Cmp(loPred, ir.ClsW, alo, blo)
	return b.Or(ir.ClsW, hiOrd, b.And(ir.ClsW, hiEq, loOrd))
}

func pick(cond bool, a, b ir.Cmp) ir.Cmp {
	if cond {
		return a
	}
	return b
}

// int128Convert coerces to/from a 128-bit integer: a narrower integer widens, a
// float converts through a libgcc helper, and a 128-bit value narrows to its low
// half (or, for a float or _Bool target, converts / tests both halves).
func (g *gen) int128Convert(v ir.Ref, from, to moderncc.Type) ir.Ref {
	switch {
	case isInt128(from) && isInt128(to):
		return v // signed<->unsigned 128 share a representation
	case isInt128(to): // narrower value -> 128
		if isFloat(from) {
			return g.floatToInt128(v, from, to)
		}
		lo, hi := g.widenTo128(v, from)
		return g.int128Result(lo, hi)
	default: // 128 -> narrower
		lo, hi := g.loadParts(v, from)
		if to.Kind() == moderncc.Bool {
			nz := g.cur.Or(ir.ClsL, lo, hi)
			return g.cur.Cmp(ir.CmpNe, ir.ClsW, nz, g.fn.Long(0))
		}
		if isFloat(to) {
			return g.int128ToFloat(v, from, to)
		}
		if isLongDouble(to) {
			return g.fail("cc: converting __int128 to long double is unsupported")
		}
		// Integer narrowing: the low half already holds the value; reclassify to the
		// target width (a word target truncates).
		if wide(clsOf(to)) {
			return lo
		}
		return g.cur.Copy(ir.ClsW, lo)
	}
}

// floatToInt128 converts a float/double to a 128-bit integer via libgcc.
func (g *gen) floatToInt128(v ir.Ref, from, to moderncc.Type) ir.Ref {
	dbl := from.Kind() == moderncc.Double
	var fn string
	switch {
	case signed(to) && dbl:
		fn = "__fixdfti"
	case signed(to):
		fn = "__fixsfti"
	case dbl:
		fn = "__fixunsdfti"
	default:
		fn = "__fixunssfti"
	}
	return g.ti3Scalar(fn, v)
}

// int128ToFloat converts a 128-bit integer (at address v) to a float/double via
// libgcc.
func (g *gen) int128ToFloat(v ir.Ref, from, to moderncc.Type) ir.Ref {
	dbl := to.Kind() == moderncc.Double
	var fn string
	switch {
	case signed(from) && dbl:
		fn = "__floattidf"
	case signed(from):
		fn = "__floattisf"
	case dbl:
		fn = "__floatuntidf"
	default:
		fn = "__floatuntisf"
	}
	r := g.cur.Call(clsOf(to), g.fn.Sym(fn, 0), v)
	g.lastInstr().SetAggArgs([]*ir.AggType{g.int128Agg()})
	return r
}

// int128IncDec computes (value at addr) +/- 1 with carry/borrow across the
// halves, returning the address of the new value.
func (g *gen) int128IncDec(addr ir.Ref, inc bool) ir.Ref {
	alo, ahi := g.loadParts(addr, nil)
	b := g.cur
	one := g.fn.Long(1)
	if inc {
		lo := b.Add(ir.ClsL, alo, one)
		hi := b.Add(ir.ClsL, ahi, g.ext1(b.Cmp(ir.CmpUlt, ir.ClsW, lo, alo)))
		return g.int128Result(lo, hi)
	}
	lo := b.Sub(ir.ClsL, alo, one)
	hi := b.Sub(ir.ClsL, ahi, g.ext1(b.Cmp(ir.CmpUlt, ir.ClsW, alo, one)))
	return g.int128Result(lo, hi)
}

// int128Copy snapshots a 128-bit value into a fresh slot (for post-inc/dec).
func (g *gen) int128Copy(addr ir.Ref) ir.Ref {
	lo, hi := g.loadParts(addr, nil)
	return g.int128Result(lo, hi)
}

// int128Compound performs a 128-bit compound assignment a op= rhs.
func (g *gen) int128Compound(op string, addr ir.Ref, t moderncc.Type, rhs moderncc.ExpressionNode) ir.Ref {
	res := g.int128ArithAddr(op, addr, t, rhs)
	g.copyAgg(addr, res, 16)
	return addr
}

// int128ArithAddr performs (value at addr) op rhs, reusing the binary lowering by
// treating the current storage as the left operand.
func (g *gen) int128ArithAddr(op string, addr ir.Ref, t moderncc.Type, rhs moderncc.ExpressionNode) ir.Ref {
	uns := !signed(t)
	switch op {
	case "*":
		return g.ti3("__multi3", addr, g.int128Operand(rhs))
	case "/":
		return g.ti3(pickStr(uns, "__udivti3", "__divti3"), addr, g.int128Operand(rhs))
	case "%":
		return g.ti3(pickStr(uns, "__umodti3", "__modti3"), addr, g.int128Operand(rhs))
	case "<<":
		return g.ti3ShiftAddr("__ashlti3", addr, rhs)
	case ">>":
		return g.ti3ShiftAddr(pickStr(uns, "__lshrti3", "__ashrti3"), addr, rhs)
	}
	alo, ahi := g.loadParts(addr, t)
	blo, bhi := g.int128Parts(rhs)
	b := g.cur
	switch op {
	case "+":
		lo := b.Add(ir.ClsL, alo, blo)
		hi := b.Add(ir.ClsL, b.Add(ir.ClsL, ahi, bhi), g.ext1(b.Cmp(ir.CmpUlt, ir.ClsW, lo, alo)))
		return g.int128Result(lo, hi)
	case "-":
		lo := b.Sub(ir.ClsL, alo, blo)
		hi := b.Sub(ir.ClsL, b.Sub(ir.ClsL, ahi, bhi), g.ext1(b.Cmp(ir.CmpUlt, ir.ClsW, alo, blo)))
		return g.int128Result(lo, hi)
	case "&":
		return g.int128Result(b.And(ir.ClsL, alo, blo), b.And(ir.ClsL, ahi, bhi))
	case "|":
		return g.int128Result(b.Or(ir.ClsL, alo, blo), b.Or(ir.ClsL, ahi, bhi))
	case "^":
		return g.int128Result(b.Xor(ir.ClsL, alo, blo), b.Xor(ir.ClsL, ahi, bhi))
	}
	return g.fail("cc: operator %q is not defined for __int128", op)
}

func pickStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// constInt128 evaluates a constant 128-bit expression to a big.Int for static
// initialization. modernc folds most 64-bit constant arithmetic but leaves
// wider results (a shift past 64, a product overflowing 64 bits) unfolded, so
// the operators are evaluated here at full precision over big.Int.
func constInt128(e moderncc.ExpressionNode) (*big.Int, bool) {
	if e == nil {
		return nil, false
	}
	switch n := e.(type) {
	case *moderncc.AdditiveExpression:
		if l, r, ok := constInt128Pair(n.AdditiveExpression, n.MultiplicativeExpression); ok {
			if n.Case == moderncc.AdditiveExpressionSub {
				return new(big.Int).Sub(l, r), true
			}
			return new(big.Int).Add(l, r), true
		}
	case *moderncc.MultiplicativeExpression:
		if l, r, ok := constInt128Pair(n.MultiplicativeExpression, n.CastExpression); ok {
			switch n.Case {
			case moderncc.MultiplicativeExpressionMul:
				return new(big.Int).Mul(l, r), true
			case moderncc.MultiplicativeExpressionDiv:
				if r.Sign() != 0 {
					return new(big.Int).Quo(l, r), true
				}
			case moderncc.MultiplicativeExpressionMod:
				if r.Sign() != 0 {
					return new(big.Int).Rem(l, r), true
				}
			}
		}
	case *moderncc.ShiftExpression:
		if l, r, ok := constInt128Pair(n.ShiftExpression, n.AdditiveExpression); ok && r.IsInt64() {
			s := uint(r.Int64())
			if n.Case == moderncc.ShiftExpressionRsh {
				return new(big.Int).Rsh(l, s), true
			}
			return new(big.Int).Lsh(l, s), true
		}
	case *moderncc.AndExpression:
		if l, r, ok := constInt128Pair(n.AndExpression, n.EqualityExpression); ok {
			return new(big.Int).And(l, r), true
		}
	case *moderncc.ExclusiveOrExpression:
		if l, r, ok := constInt128Pair(n.ExclusiveOrExpression, n.AndExpression); ok {
			return new(big.Int).Xor(l, r), true
		}
	case *moderncc.InclusiveOrExpression:
		if l, r, ok := constInt128Pair(n.InclusiveOrExpression, n.ExclusiveOrExpression); ok {
			return new(big.Int).Or(l, r), true
		}
	case *moderncc.UnaryExpression:
		if v, ok := constInt128(n.CastExpression); ok {
			switch n.Case {
			case moderncc.UnaryExpressionMinus:
				return new(big.Int).Neg(v), true
			case moderncc.UnaryExpressionPlus:
				return v, true
			case moderncc.UnaryExpressionCpl:
				return new(big.Int).Not(v), true
			}
		}
	case *moderncc.CastExpression:
		return constInt128(n.CastExpression)
	case *moderncc.PrimaryExpression:
		if n.Case == moderncc.PrimaryExpressionExpr {
			return constInt128(n.ExpressionList)
		}
	case *moderncc.ExpressionList:
		if n.ExpressionList == nil {
			return constInt128(n.AssignmentExpression)
		}
	}
	if v, ok := constInt(e); ok {
		return big.NewInt(v), true
	}
	return nil, false
}

func constInt128Pair(a, b moderncc.ExpressionNode) (*big.Int, *big.Int, bool) {
	l, lok := constInt128(a)
	r, rok := constInt128(b)
	if lok && rok {
		return l, r, true
	}
	return nil, nil, false
}

// split128 reduces v modulo 2^128 (two's complement) and returns its low and
// high 64-bit halves for emission as static data.
func split128(v *big.Int) (lo, hi int64) {
	mod := new(big.Int).Lsh(big.NewInt(1), 128)
	m := new(big.Int).Mod(v, mod) // Euclidean: 0 .. 2^128-1
	lo = int64(new(big.Int).And(m, new(big.Int).SetUint64(^uint64(0))).Uint64())
	hi = int64(new(big.Int).Rsh(m, 64).Uint64())
	return
}

// ti3 calls a libgcc helper taking two 128-bit arguments (each passed as a
// 16-byte integer aggregate) and returning a 128-bit result.
func (g *gen) ti3(fn string, a, b ir.Ref) ir.Ref {
	r := g.cur.Call(ir.ClsL, g.fn.Sym(fn, 0), a, b)
	call := g.lastInstr()
	call.RetAgg = g.int128Agg()
	call.SetAggArgs([]*ir.AggType{g.int128Agg(), g.int128Agg()})
	return r
}

// ti3Scalar calls a libgcc helper taking one scalar argument and returning a
// 128-bit result (the float-to-int128 conversions).
func (g *gen) ti3Scalar(fn string, v ir.Ref) ir.Ref {
	r := g.cur.Call(ir.ClsL, g.fn.Sym(fn, 0), v)
	g.lastInstr().RetAgg = g.int128Agg()
	return r
}

// ti3Shift calls a libgcc 128-bit shift helper: a 128-bit value and an int count.
func (g *gen) ti3Shift(fn string, ln, rn moderncc.ExpressionNode) ir.Ref {
	return g.ti3ShiftAddr(fn, g.int128Operand(ln), rn)
}

func (g *gen) ti3ShiftAddr(fn string, addr ir.Ref, rn moderncc.ExpressionNode) ir.Ref {
	n := g.cur.Copy(ir.ClsW, g.genExpr(rn)) // the shift count is an int
	r := g.cur.Call(ir.ClsL, g.fn.Sym(fn, 0), addr, n)
	call := g.lastInstr()
	call.RetAgg = g.int128Agg()
	call.SetAggArgs([]*ir.AggType{g.int128Agg(), nil})
	return r
}
