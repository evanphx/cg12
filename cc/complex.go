package cc

import (
	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// A C _Complex value is modelled as a two-element aggregate {real, imag} of its
// component type, so it travels through the same by-value-in-memory path as a
// struct. That representation is ABI-compatible with how both targets pass a
// complex: a {double,double} aggregate is a homogeneous float aggregate carried
// in a pair of SIMD registers, exactly as the System V and AAPCS64 rules
// classify `double _Complex`. Arithmetic is lowered here to component
// operations. Only float and double complex are supported; the long-double and
// integer complex kinds fall through to a loud failure rather than miscompiling.

// isComplex reports whether t is a supported (_Complex float/double) type.
func isComplex(t moderncc.Type) bool {
	switch t.Kind() {
	case moderncc.ComplexFloat, moderncc.ComplexDouble:
		return true
	}
	return false
}

// complexCls returns the IR class of the real/imaginary component of complex t.
func complexCls(t moderncc.Type) ir.Cls {
	if t.Kind() == moderncc.ComplexFloat {
		return ir.ClsS
	}
	return ir.ClsD
}

// complexSub returns the aggregate sub-class of complex t's component.
func complexSub(t moderncc.Type) ir.SubCls {
	if t.Kind() == moderncc.ComplexFloat {
		return ir.SubS
	}
	return ir.SubD
}

// complexCompSize returns the byte size of one component of complex t.
func complexCompSize(t moderncc.Type) int {
	if t.Kind() == moderncc.ComplexFloat {
		return 4
	}
	return 8
}

// complexAgg returns (and memoizes) the {re,im} aggregate type modelling complex
// t, so a complex value travels through the SIMD-register path of the aggregate
// ABI just like a homogeneous {double,double} struct.
func (g *gen) complexAgg(t moderncc.Type) *ir.AggType {
	sub := complexSub(t)
	if a, ok := g.cplx[sub]; ok {
		return a
	}
	name := "complexdouble"
	if sub == ir.SubS {
		name = "complexfloat"
	}
	agg := &ir.AggType{Name: name, Fields: []ir.Field{{Sub: sub}, {Sub: sub}}}
	g.cplx[sub] = agg
	g.mod.AddType(agg)
	return agg
}

// floatCast widens or narrows a floating value between the single and double
// classes; a no-op when the classes already match.
func (g *gen) floatCast(v ir.Ref, from, to ir.Cls) ir.Ref {
	if from == to {
		return v
	}
	if to == ir.ClsD {
		return g.cur.Exts(v) // single -> double
	}
	return g.cur.Truncd(v) // double -> single
}

// floatZero returns a +0.0 constant of the given floating class.
func (g *gen) floatZero(cls ir.Cls) ir.Ref {
	if cls == ir.ClsS {
		return g.fn.Single(0)
	}
	return g.fn.Double(0)
}

// toComponent converts a real scalar value of C type from into a floating
// component of class comp (the component type of a complex result).
func (g *gen) toComponent(v ir.Ref, from moderncc.Type, comp ir.Cls) ir.Ref {
	if isLongDouble(from) {
		g.fail("cc: mixing long double with _Complex float/double is unsupported")
		return v
	}
	if isFloat(from) {
		return g.floatCast(v, clsOf(from), comp)
	}
	if signed(from) {
		return g.cur.Sltof(comp, v)
	}
	return g.cur.Ultof(comp, v)
}

// compSize returns the byte size of a component of the given floating class.
func compSize(comp ir.Cls) int {
	if comp == ir.ClsS {
		return 4
	}
	return 8
}

// complexParts loads the real and imaginary components of operand e as the
// component class comp. A real operand promotes to (value, 0). Complexness is
// detected structurally (effComplex), since modernc drops the complex kind from
// a `real op _Complex float` expression even though the value is complex.
func (g *gen) complexParts(e moderncc.ExpressionNode, comp ir.Cls) (re, im ir.Ref) {
	if src, ok := g.effComplex(e); ok {
		addr := g.genExpr(e) // pointer to the {re,im} storage
		re = g.floatCast(g.cur.Load(src, addr), src, comp)
		im = g.floatCast(g.cur.Load(src, g.offset(addr, compSize(src))), src, comp)
		return re, im
	}
	return g.toComponent(g.genExpr(e), e.Type(), comp), g.floatZero(comp)
}

// complexResult stores components re, im into a fresh stack slot for a complex
// value with component class comp, returning that slot's address.
func (g *gen) complexResult(re, im ir.Ref, comp ir.Cls) ir.Ref {
	sz := compSize(comp)
	addr := g.cur.Alloc(sz, 2*sz)
	g.cur.Store(re, addr)
	g.cur.Store(im, g.offset(addr, sz))
	return addr
}

// complexArithComp reports whether a +, -, *, / expression is complex and, if
// so, the component class its result is computed in.
func (g *gen) complexArithComp(ln, rn moderncc.ExpressionNode, resT moderncc.Type) (ir.Cls, bool) {
	if isComplex(resT) {
		return complexCls(resT), true
	}
	return g.effComplexBin(ln, rn)
}

// complexArith lowers a complex +, -, *, or / to component operations at class
// comp, returning the address of the result. Division and multiplication use the
// textbook formulas (finite, non-overflowing operands), matching gcc/clang on
// ordinary values; they do not reproduce the inf/nan rescue that libgcc's
// __muldc3 and __divdc3 perform at the extremes.
func (g *gen) complexArith(op string, ln, rn moderncc.ExpressionNode, comp ir.Cls) ir.Ref {
	a, b := g.complexParts(ln, comp)
	c, d := g.complexParts(rn, comp)
	return g.complexArithParts(op, a, b, c, d, comp)
}

// complexArithParts computes (a+bi) op (c+di) at component class comp and returns
// the address of the result slot.
func (g *gen) complexArithParts(op string, a, b, c, d ir.Ref, comp ir.Cls) ir.Ref {
	cur := g.cur
	var re, im ir.Ref
	switch op {
	case "+":
		re, im = cur.Add(comp, a, c), cur.Add(comp, b, d)
	case "-":
		re, im = cur.Sub(comp, a, c), cur.Sub(comp, b, d)
	case "*":
		// (a+bi)(c+di) = (ac - bd) + (ad + bc)i
		re = cur.Sub(comp, cur.Mul(comp, a, c), cur.Mul(comp, b, d))
		im = cur.Add(comp, cur.Mul(comp, a, d), cur.Mul(comp, b, c))
	case "/":
		// (a+bi)/(c+di) = ((ac + bd) + (bc - ad)i) / (c^2 + d^2)
		den := cur.Add(comp, cur.Mul(comp, c, c), cur.Mul(comp, d, d))
		re = cur.Div(comp, cur.Add(comp, cur.Mul(comp, a, c), cur.Mul(comp, b, d)), den)
		im = cur.Div(comp, cur.Sub(comp, cur.Mul(comp, b, c), cur.Mul(comp, a, d)), den)
	default:
		return g.fail("cc: operator %q is not defined for _Complex", op)
	}
	return g.complexResult(re, im, comp)
}

// complexCompound performs a complex compound assignment a op= rhs: it reloads
// the current value at addr, combines it with rhs, and copies the result back.
func (g *gen) complexCompound(op string, addr ir.Ref, t moderncc.Type, rhs moderncc.ExpressionNode) ir.Ref {
	comp := complexCls(t)
	a := g.cur.Load(comp, addr)
	b := g.cur.Load(comp, g.offset(addr, compSize(comp)))
	c, d := g.complexParts(rhs, comp)
	res := g.complexArithParts(op, a, b, c, d, comp)
	g.copyAgg(addr, res, int(t.Size()))
	return addr
}

// complexNeg lowers unary minus on a complex value: negate both components.
func (g *gen) complexNeg(e moderncc.ExpressionNode, comp ir.Cls) ir.Ref {
	re, im := g.complexParts(e, comp)
	return g.complexResult(g.cur.Neg(comp, re), g.cur.Neg(comp, im), comp)
}

// complexCompare lowers == / != on complex operands: equality holds when both
// components are equal.
func (g *gen) complexCompare(op string, ln, rn moderncc.ExpressionNode) ir.Ref {
	comp := ir.ClsS
	if lc, _ := g.effComplex(ln); lc == ir.ClsD {
		comp = ir.ClsD
	}
	if rc, _ := g.effComplex(rn); rc == ir.ClsD {
		comp = ir.ClsD
	}
	a, b := g.complexParts(ln, comp)
	c, d := g.complexParts(rn, comp)
	if op == "!=" {
		reNe := g.cur.Cmp(ir.CmpFne, ir.ClsW, a, c)
		imNe := g.cur.Cmp(ir.CmpFne, ir.ClsW, b, d)
		return g.cur.Or(ir.ClsW, reNe, imNe)
	}
	reEq := g.cur.Cmp(ir.CmpFeq, ir.ClsW, a, c)
	imEq := g.cur.Cmp(ir.CmpFeq, ir.ClsW, b, d)
	return g.cur.And(ir.ClsW, reEq, imEq)
}

// complexConst materializes a constant complex value with component class comp.
func (g *gen) complexConst(v complex128, comp ir.Cls) ir.Ref {
	return g.complexResult(g.floatConstCls(real(v), comp), g.floatConstCls(imag(v), comp), comp)
}

// effComplex reports whether expression e yields a complex value and, if so, its
// component class. It first trusts modernc's type; failing that (the `real op
// _Complex float` quirk, which mis-reports a real result), it recomputes the
// answer structurally from the operands, whose own complex kinds are intact.
func (g *gen) effComplex(e moderncc.ExpressionNode) (ir.Cls, bool) {
	if e == nil {
		return 0, false
	}
	if isComplex(e.Type()) {
		return complexCls(e.Type()), true
	}
	switch n := e.(type) {
	case *moderncc.MultiplicativeExpression:
		if n.Case == moderncc.MultiplicativeExpressionMul || n.Case == moderncc.MultiplicativeExpressionDiv {
			return g.effComplexBin(n.MultiplicativeExpression, n.CastExpression)
		}
	case *moderncc.AdditiveExpression:
		return g.effComplexBin(n.AdditiveExpression, n.MultiplicativeExpression)
	case *moderncc.UnaryExpression:
		if n.Case == moderncc.UnaryExpressionMinus || n.Case == moderncc.UnaryExpressionPlus {
			return g.effComplex(n.CastExpression)
		}
	case *moderncc.PrimaryExpression:
		if n.Case == moderncc.PrimaryExpressionExpr {
			return g.effComplex(n.ExpressionList)
		}
	case *moderncc.ExpressionList:
		if n.ExpressionList == nil {
			return g.effComplex(n.AssignmentExpression)
		}
		return g.effComplex(n.ExpressionList)
	}
	return 0, false
}

// effComplexBin computes the complexness of a binary operator from its operands:
// complex if either is, with a double component if any operand contributes one.
func (g *gen) effComplexBin(l, r moderncc.ExpressionNode) (ir.Cls, bool) {
	lc, lok := g.effComplex(l)
	rc, rok := g.effComplex(r)
	if !lok && !rok {
		return 0, false
	}
	comp := ir.ClsS
	if lc == ir.ClsD || rc == ir.ClsD || compRankDouble(l) || compRankDouble(r) {
		comp = ir.ClsD
	}
	return comp, true
}

// compRankDouble reports whether a real operand's rank widens the complex
// component to double (a double or long double operand does; float and integers
// do not).
func compRankDouble(e moderncc.ExpressionNode) bool {
	switch e.Type().Kind() {
	case moderncc.Double, moderncc.LongDouble, moderncc.ComplexDouble, moderncc.ComplexLongDouble:
		return true
	}
	return false
}

// floatConstCls builds a floating constant of the given class.
func (g *gen) floatConstCls(v float64, cls ir.Cls) ir.Ref {
	if cls == ir.ClsS {
		return g.fn.Single(v)
	}
	return g.fn.Double(v)
}

// complexConstVal returns the constant complex value of e, if it folds to one.
func complexConstVal(e moderncc.ExpressionNode) (complex128, bool) {
	switch v := e.Value().(type) {
	case moderncc.Complex128Value:
		return complex128(v), true
	case moderncc.Complex64Value:
		return complex128(complex64(v)), true
	}
	return 0, false
}

// constComplex evaluates a constant complex expression to a complex128 for
// static initialization. modernc folds only bare imaginary literals, so an
// initializer like 3.0 + 4.0*I (which it also mis-types as real) must be
// evaluated here structurally over +, -, *, /, unary +/-, and real/imaginary
// constants.
func constComplex(e moderncc.ExpressionNode) (complex128, bool) {
	if e == nil {
		return 0, false
	}
	if cv, ok := complexConstVal(e); ok {
		return cv, true
	}
	if fv, ok := e.Value().(moderncc.Float64Value); ok {
		return complex(float64(fv), 0), true
	}
	switch n := e.(type) {
	case *moderncc.AdditiveExpression:
		l, lok := constComplex(n.AdditiveExpression)
		r, rok := constComplex(n.MultiplicativeExpression)
		if lok && rok {
			if n.Case == moderncc.AdditiveExpressionSub {
				return l - r, true
			}
			return l + r, true
		}
	case *moderncc.MultiplicativeExpression:
		l, lok := constComplex(n.MultiplicativeExpression)
		r, rok := constComplex(n.CastExpression)
		if lok && rok {
			switch n.Case {
			case moderncc.MultiplicativeExpressionMul:
				return l * r, true
			case moderncc.MultiplicativeExpressionDiv:
				return l / r, true
			}
		}
	case *moderncc.UnaryExpression:
		if v, ok := constComplex(n.CastExpression); ok {
			switch n.Case {
			case moderncc.UnaryExpressionMinus:
				return -v, true
			case moderncc.UnaryExpressionPlus:
				return v, true
			}
		}
	case *moderncc.CastExpression:
		return constComplex(n.CastExpression)
	case *moderncc.PrimaryExpression:
		if n.Case == moderncc.PrimaryExpressionExpr {
			return constComplex(n.ExpressionList)
		}
	case *moderncc.ExpressionList:
		if n.ExpressionList == nil {
			return constComplex(n.AssignmentExpression)
		}
	}
	if iv, ok := constInt(e); ok {
		return complex(float64(iv), 0), true
	}
	return 0, false
}

// complexConvert coerces to/from a complex type (both correctly typed): a real
// value becomes (x, 0), a complex narrows/widens its components, and a complex
// converted to a real type keeps its real part.
func (g *gen) complexConvert(v ir.Ref, from, to moderncc.Type) ir.Ref {
	switch {
	case isComplex(from) && isComplex(to):
		return g.complexToComplex(v, complexCls(from), to)
	case isComplex(from):
		return g.complexToReal(v, complexCls(from), to)
	default: // real -> complex: (x, 0)
		comp := complexCls(to)
		return g.complexResult(g.toComponent(v, from, comp), g.floatZero(comp), comp)
	}
}

// complexToComplex widens or narrows the components of a complex value (at
// address v, component class src) to the component class of complex type to.
func (g *gen) complexToComplex(v ir.Ref, src ir.Cls, to moderncc.Type) ir.Ref {
	comp := complexCls(to)
	if src == comp {
		return v
	}
	re := g.floatCast(g.cur.Load(src, v), src, comp)
	im := g.floatCast(g.cur.Load(src, g.offset(v, compSize(src))), src, comp)
	return g.complexResult(re, im, comp)
}

// complexToReal converts a complex value (at address v, component class src) to a
// real, integer, or _Bool type, keeping the real part (_Bool tests both parts).
func (g *gen) complexToReal(v ir.Ref, src ir.Cls, to moderncc.Type) ir.Ref {
	re := g.cur.Load(src, v)
	if to.Kind() == moderncc.Bool {
		im := g.cur.Load(src, g.offset(v, compSize(src)))
		reNz := g.cur.Cmp(ir.CmpFne, ir.ClsW, re, g.floatZero(src))
		imNz := g.cur.Cmp(ir.CmpFne, ir.ClsW, im, g.floatZero(src))
		return g.cur.Or(ir.ClsW, reNz, imNz)
	}
	if isLongDouble(to) {
		g.fail("cc: converting _Complex to long double is unsupported")
		return re
	}
	if isFloat(to) {
		return g.floatCast(re, src, clsOf(to))
	}
	if signed(to) {
		return g.cur.Stosi(clsOf(to), re)
	}
	return g.cur.Stoui(clsOf(to), re)
}

// rval evaluates e and coerces it to type to. It is the complex-aware form of
// `convert(genExpr(e), e.Type(), to)`, used at every site that consumes an
// expression's value with a target type. When modernc has dropped the complex
// kind from e (the `real op _Complex float` quirk) the value is really a complex
// slot, so the conversion is driven off the structurally-recovered component
// class rather than the mis-reported real type.
func (g *gen) rval(e moderncc.ExpressionNode, to moderncc.Type) ir.Ref {
	if src, ok := g.effComplex(e); ok && !isComplex(e.Type()) {
		v := g.genExpr(e)
		if isComplex(to) {
			return g.complexToComplex(v, src, to)
		}
		if moderncc.IsComplexType(to) {
			return g.fail("cc: unsupported complex conversion to %v", to)
		}
		return g.complexToReal(v, src, to)
	}
	return g.convert(g.genExpr(e), e.Type(), to)
}

// complexOperand returns the operand expression of a __real__ / __imag__ unary
// expression (grammar: `"__real__" UnaryExpression`).
func complexOperand(n *moderncc.UnaryExpression) moderncc.ExpressionNode {
	if n.UnaryExpression != nil {
		return n.UnaryExpression
	}
	return n.CastExpression
}

// complexReal / complexImag extract a component of a complex operand as a scalar
// of the component type (the result type of __real__ / __imag__). Applied to a
// real operand, __real__ is the value itself and __imag__ is zero.
func (g *gen) complexReal(e moderncc.ExpressionNode) ir.Ref {
	if isComplex(e.Type()) {
		return g.cur.Load(complexCls(e.Type()), g.genExpr(e))
	}
	return g.genExpr(e)
}

func (g *gen) complexImag(e moderncc.ExpressionNode) ir.Ref {
	if isComplex(e.Type()) {
		addr := g.genExpr(e)
		return g.cur.Load(complexCls(e.Type()), g.offset(addr, complexCompSize(e.Type())))
	}
	g.genExpr(e) // evaluate for side effects
	if isFloat(e.Type()) {
		return g.floatOf(0, e.Type())
	}
	return g.constOf(0, e.Type())
}
