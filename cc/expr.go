package cc

import (
	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// genExpr evaluates an expression to its rvalue.
func (g *gen) genExpr(e cc.ExpressionNode) ir.Ref {
	switch n := e.(type) {
	case *cc.PrimaryExpression:
		return g.genPrimary(n)
	case *cc.PostfixExpression:
		return g.genPostfix(n)
	case *cc.UnaryExpression:
		return g.genUnary(n)
	case *cc.CastExpression:
		return g.convert(g.genExpr(n.CastExpression), n.CastExpression.Type(), n.Type())
	case *cc.MultiplicativeExpression:
		return g.arith(mulOp(n.Case), n.MultiplicativeExpression, n.CastExpression, n.Type())
	case *cc.AdditiveExpression:
		return g.arith(addOp(n.Case), n.AdditiveExpression, n.MultiplicativeExpression, n.Type())
	case *cc.ShiftExpression:
		return g.arith(shiftOp(n.Case), n.ShiftExpression, n.AdditiveExpression, n.Type())
	case *cc.AndExpression:
		return g.arith("&", n.AndExpression, n.EqualityExpression, n.Type())
	case *cc.ExclusiveOrExpression:
		return g.arith("^", n.ExclusiveOrExpression, n.AndExpression, n.Type())
	case *cc.InclusiveOrExpression:
		return g.arith("|", n.InclusiveOrExpression, n.ExclusiveOrExpression, n.Type())
	case *cc.RelationalExpression:
		return g.compare(relOp(n.Case), n.RelationalExpression, n.ShiftExpression)
	case *cc.EqualityExpression:
		return g.compare(eqOp(n.Case), n.EqualityExpression, n.RelationalExpression)
	case *cc.LogicalAndExpression:
		return g.logical(true, n.LogicalAndExpression, n.InclusiveOrExpression)
	case *cc.LogicalOrExpression:
		return g.logical(false, n.LogicalOrExpression, n.LogicalAndExpression)
	case *cc.ConditionalExpression:
		return g.genCond3(n)
	case *cc.AssignmentExpression:
		return g.genAssign(n)
	case *cc.ExpressionList:
		g.genExpr(n.ExpressionList) // comma: evaluate the left for effect
		return g.genExpr(n.AssignmentExpression)
	}
	return g.fail("cc: unsupported expression %T", e)
}

func mulOp(c cc.MultiplicativeExpressionCase) string {
	switch c {
	case cc.MultiplicativeExpressionDiv:
		return "/"
	case cc.MultiplicativeExpressionMod:
		return "%"
	default:
		return "*"
	}
}
func addOp(c cc.AdditiveExpressionCase) string {
	if c == cc.AdditiveExpressionSub {
		return "-"
	}
	return "+"
}
func shiftOp(c cc.ShiftExpressionCase) string {
	if c == cc.ShiftExpressionRsh {
		return ">>"
	}
	return "<<"
}
func relOp(c cc.RelationalExpressionCase) string {
	switch c {
	case cc.RelationalExpressionGt:
		return ">"
	case cc.RelationalExpressionLeq:
		return "<="
	case cc.RelationalExpressionGeq:
		return ">="
	default:
		return "<"
	}
}
func eqOp(c cc.EqualityExpressionCase) string {
	if c == cc.EqualityExpressionNeq {
		return "!="
	}
	return "=="
}

// --- primary expressions ---------------------------------------------------

func (g *gen) genPrimary(n *cc.PrimaryExpression) ir.Ref {
	switch n.Case {
	case cc.PrimaryExpressionInt, cc.PrimaryExpressionChar, cc.PrimaryExpressionLChar:
		v, _ := constInt(n)
		return g.constOf(v, n.Type())
	case cc.PrimaryExpressionFloat:
		if v, ok := n.Value().(cc.Float64Value); ok {
			return g.fn.Double(float64(v))
		}
		return g.fn.Double(0)
	case cc.PrimaryExpressionString:
		s, _ := n.Value().(cc.StringValue)
		return g.strSym(string(s))
	case cc.PrimaryExpressionIdent:
		return g.loadLval(n)
	case cc.PrimaryExpressionExpr:
		return g.genExpr(n.ExpressionList)
	}
	return g.fail("cc: unsupported primary expression case %v", n.Case)
}

// constOf builds an integer/pointer constant of the given type's class.
func (g *gen) constOf(v int64, t cc.Type) ir.Ref {
	if wide(clsOf(t)) {
		return g.fn.Long(v)
	}
	return g.fn.Word(v)
}

// loadLval loads the value of a named identifier (variable or the address of a
// function/array).
func (g *gen) loadLval(n *cc.PrimaryExpression) ir.Ref {
	name := n.Token.SrcStr()
	if v, ok := g.lookup(name); ok {
		if _, isArr := v.typ.(*cc.ArrayType); isArr {
			return v.addr // arrays decay to their address
		}
		val := g.loadVal(v.addr, v.typ)
		g.setName(val, name) // this value is a read of the C variable
		return val
	}
	// An unknown identifier is a global function or object referenced by symbol.
	return g.fn.Sym(name, 0)
}

// --- lvalue addresses ------------------------------------------------------

// genAddr returns the address of an lvalue expression.
func (g *gen) genAddr(e cc.ExpressionNode) (ir.Ref, cc.Type) {
	switch n := e.(type) {
	case *cc.PrimaryExpression:
		if n.Case == cc.PrimaryExpressionIdent {
			if v, ok := g.lookup(n.Token.SrcStr()); ok {
				return v.addr, v.typ
			}
			return g.fn.Sym(n.Token.SrcStr(), 0), n.Type()
		}
		if n.Case == cc.PrimaryExpressionExpr {
			return g.genAddr(n.ExpressionList)
		}
	case *cc.UnaryExpression:
		if n.Case == cc.UnaryExpressionDeref { // *p
			return g.genExpr(n.CastExpression), n.Type()
		}
	case *cc.PostfixExpression:
		switch n.Case {
		case cc.PostfixExpressionIndex: // a[i]
			base, elemT := g.arrayBase(n.PostfixExpression)
			idx := g.toLong(g.genExpr(n.ExpressionList), n.ExpressionList.Type())
			off := g.cur.Mul(ir.ClsL, idx, g.fn.Long(int64(elemT.Size())))
			return g.cur.Add(ir.ClsP, base, off), elemT
		case cc.PostfixExpressionSelect: // s.field
			base, _ := g.genAddr(n.PostfixExpression)
			fld := n.Field()
			return g.offset(base, int(fld.Offset())), fld.Type()
		case cc.PostfixExpressionPSelect: // p->field
			ptr := g.genExpr(n.PostfixExpression)
			fld := n.Field()
			return g.offset(ptr, int(fld.Offset())), fld.Type()
		}
	}
	g.fail("cc: expression is not an lvalue: %T", e)
	return ir.R, e.Type()
}

// arrayBase returns the base pointer and element type for indexing a[i] where a
// is an array (decays to a pointer) or a pointer.
func (g *gen) arrayBase(e cc.ExpressionNode) (ir.Ref, cc.Type) {
	if pt, ok := e.Type().(*cc.PointerType); ok {
		return g.genExpr(e), pt.Elem()
	}
	if at, ok := e.Type().(*cc.ArrayType); ok {
		base, _ := g.genAddr(e)
		return base, at.Elem()
	}
	return g.genExpr(e), e.Type()
}

func (g *gen) offset(addr ir.Ref, off int) ir.Ref {
	if off == 0 {
		return addr
	}
	return g.cur.Add(ir.ClsP, addr, g.fn.Long(int64(off)))
}

// --- postfix ---------------------------------------------------------------

func (g *gen) genPostfix(n *cc.PostfixExpression) ir.Ref {
	switch n.Case {
	case cc.PostfixExpressionCall:
		return g.genCall(n)
	case cc.PostfixExpressionIndex:
		addr, t := g.genAddr(n)
		return g.loadVal(addr, t)
	case cc.PostfixExpressionSelect, cc.PostfixExpressionPSelect:
		addr, t := g.genAddr(n)
		return g.loadVal(addr, t)
	case cc.PostfixExpressionInc, cc.PostfixExpressionDec:
		addr, t := g.genAddr(n.PostfixExpression)
		old := g.loadVal(addr, t)
		g.storeVal(addr, g.incDec(old, t, n.Case == cc.PostfixExpressionInc), t)
		return old // post-inc/dec yields the old value
	}
	return g.fail("cc: unsupported postfix case %v", n.Case)
}

// incDec adds or subtracts one (scaled by element size for pointers).
func (g *gen) incDec(v ir.Ref, t cc.Type, inc bool) ir.Ref {
	step := int64(1)
	if pt, ok := t.(*cc.PointerType); ok {
		step = int64(pt.Elem().Size())
	}
	cls := clsOf(t)
	d := g.fn.Word(step)
	if wide(cls) {
		d = g.fn.Long(step)
	}
	if inc {
		return g.cur.Add(cls, v, d)
	}
	return g.cur.Sub(cls, v, d)
}

// --- unary -----------------------------------------------------------------

func (g *gen) genUnary(n *cc.UnaryExpression) ir.Ref {
	switch n.Case {
	case cc.UnaryExpressionMinus:
		v := g.genExpr(n.CastExpression)
		return g.cur.Neg(clsOf(n.Type()), v)
	case cc.UnaryExpressionPlus:
		return g.genExpr(n.CastExpression)
	case cc.UnaryExpressionNot: // !x  ==  x == 0
		v := g.genExpr(n.CastExpression)
		t := n.CastExpression.Type()
		if isFloat(t) {
			return g.cur.Cmp(ir.CmpFeq, clsOf(t), v, g.fn.Double(0))
		}
		return g.cur.Cmp(ir.CmpEq, clsOf(t), v, g.constOf(0, t))
	case cc.UnaryExpressionCpl: // ~x
		v := g.genExpr(n.CastExpression)
		cls := clsOf(n.Type())
		if wide(cls) {
			return g.cur.Xor(cls, v, g.fn.Long(-1))
		}
		return g.cur.Xor(cls, v, g.fn.Word(-1))
	case cc.UnaryExpressionDeref: // *p
		p := g.genExpr(n.CastExpression)
		return g.loadVal(p, n.Type())
	case cc.UnaryExpressionAddrof: // &x
		addr, _ := g.genAddr(n.CastExpression)
		return addr
	case cc.UnaryExpressionInc, cc.UnaryExpressionDec:
		addr, t := g.genAddr(n.UnaryExpression)
		v := g.incDec(g.loadVal(addr, t), t, n.Case == cc.UnaryExpressionInc)
		g.storeVal(addr, v, t)
		return v
	case cc.UnaryExpressionSizeofExpr, cc.UnaryExpressionSizeofType:
		v, _ := constInt(n)
		return g.fn.Long(v)
	}
	return g.fail("cc: unsupported unary case %v", n.Case)
}

// --- binary arithmetic -----------------------------------------------------

func (g *gen) arith(op string, ln, rn cc.ExpressionNode, resT cc.Type) ir.Ref {
	// Pointer arithmetic: p +/- n scales n by the element size.
	if pt, ok := resT.(*cc.PointerType); ok && (op == "+" || op == "-") {
		return g.ptrArith(op, ln, rn, pt)
	}
	cls := clsOf(resT)
	l := g.convert(g.genExpr(ln), ln.Type(), resT)
	r := g.convert(g.genExpr(rn), rn.Type(), resT)
	b := g.cur
	switch op {
	case "+":
		return b.Add(cls, l, r)
	case "-":
		return b.Sub(cls, l, r)
	case "*":
		return b.Mul(cls, l, r)
	case "/":
		if isFloat(resT) {
			return b.Div(cls, l, r)
		}
		if signed(resT) {
			return b.Div(cls, l, r)
		}
		return b.UDiv(cls, l, r)
	case "%":
		if signed(resT) {
			return b.Rem(cls, l, r)
		}
		return b.URem(cls, l, r)
	case "&":
		return b.And(cls, l, r)
	case "|":
		return b.Or(cls, l, r)
	case "^":
		return b.Xor(cls, l, r)
	case "<<":
		return b.Shl(cls, l, r)
	case ">>":
		if signed(resT) {
			return b.Sar(cls, l, r)
		}
		return b.Shr(cls, l, r)
	}
	return g.fail("cc: bad arith op %q", op)
}

func (g *gen) ptrArith(op string, ln, rn cc.ExpressionNode, pt *cc.PointerType) ir.Ref {
	base := g.genExpr(ln)
	idx := g.toLong(g.genExpr(rn), rn.Type())
	off := g.cur.Mul(ir.ClsL, idx, g.fn.Long(int64(pt.Elem().Size())))
	if op == "-" {
		return g.cur.Sub(ir.ClsP, base, off)
	}
	return g.cur.Add(ir.ClsP, base, off)
}

// compare emits a relational/equality comparison producing a 0/1 int.
func (g *gen) compare(op string, ln, rn cc.ExpressionNode) ir.Ref {
	// Compare in the common type of the operands.
	ct := ln.Type()
	if wide(clsOf(rn.Type())) || isFloat(rn.Type()) {
		ct = rn.Type()
	}
	if wide(clsOf(ln.Type())) {
		ct = ln.Type()
	}
	l := g.convert(g.genExpr(ln), ln.Type(), ct)
	r := g.convert(g.genExpr(rn), rn.Type(), ct)
	pred := cmpPred(op, signed(ct), isFloat(ct))
	return g.cur.Cmp(pred, clsOf(ct), l, r)
}

func cmpPred(op string, signed, flt bool) ir.Cmp {
	if flt {
		switch op {
		case "==":
			return ir.CmpFeq
		case "!=":
			return ir.CmpFne
		case "<":
			return ir.CmpFlt
		case "<=":
			return ir.CmpFle
		case ">":
			return ir.CmpFgt
		default:
			return ir.CmpFge
		}
	}
	switch op {
	case "==":
		return ir.CmpEq
	case "!=":
		return ir.CmpNe
	case "<":
		if signed {
			return ir.CmpSlt
		}
		return ir.CmpUlt
	case "<=":
		if signed {
			return ir.CmpSle
		}
		return ir.CmpUle
	case ">":
		if signed {
			return ir.CmpSgt
		}
		return ir.CmpUgt
	default:
		if signed {
			return ir.CmpSge
		}
		return ir.CmpUge
	}
}

// logical emits short-circuiting && / || producing a 0/1 int.
func (g *gen) logical(and bool, ln, rn cc.ExpressionNode) ir.Ref {
	res := g.cur.Alloc(4, 4)
	rhsB, endB := g.block("logrhs"), g.block("logend")
	l := g.genCond(ln)
	g.cur.StoreSub(ir.SubW, g.boolOf(l), res)
	if and {
		g.cur.Jnz(l, rhsB, endB) // if false, short-circuit to end
	} else {
		g.cur.Jnz(l, endB, rhsB) // if true, short-circuit to end
	}
	g.cur = rhsB
	r := g.genCond(rn)
	g.cur.StoreSub(ir.SubW, g.boolOf(r), res)
	g.cur.Goto(endB)
	g.cur = endB
	return g.cur.LoadSub(ir.ClsW, ir.SubUB, res)
}

// boolOf normalizes a value to 0/1.
func (g *gen) boolOf(v ir.Ref) ir.Ref {
	return g.cur.Cmp(ir.CmpNe, ir.ClsW, v, g.fn.Word(0))
}

// genCond3 emits the ?: conditional operator.
func (g *gen) genCond3(n *cc.ConditionalExpression) ir.Ref {
	res := g.cur.Alloc(align(n.Type()), int(n.Type().Size()))
	thenB, elseB, endB := g.block("qt"), g.block("qf"), g.block("qend")
	g.cur.Jnz(g.genCond(n.LogicalOrExpression), thenB, elseB)
	g.cur = thenB
	g.storeVal(res, g.convert(g.genExpr(n.ExpressionList), n.ExpressionList.Type(), n.Type()), n.Type())
	g.cur.Goto(endB)
	g.cur = elseB
	g.storeVal(res, g.convert(g.genExpr(n.ConditionalExpression), n.ConditionalExpression.Type(), n.Type()), n.Type())
	g.cur.Goto(endB)
	g.cur = endB
	return g.loadVal(res, n.Type())
}

// --- assignment ------------------------------------------------------------

func (g *gen) genAssign(n *cc.AssignmentExpression) ir.Ref {
	addr, t := g.genAddr(n.UnaryExpression)
	if n.Case == cc.AssignmentExpressionAssign {
		v := g.convert(g.genExpr(n.AssignmentExpression), n.AssignmentExpression.Type(), t)
		g.storeVal(addr, v, t)
		return v
	}
	// Compound assignment: load, combine, store.
	old := g.loadVal(addr, t)
	rhs := g.convert(g.genExpr(n.AssignmentExpression), n.AssignmentExpression.Type(), t)
	v := g.combineOp(n.Case, old, rhs, t)
	g.storeVal(addr, v, t)
	return v
}

func (g *gen) combineOp(c cc.AssignmentExpressionCase, l, r ir.Ref, t cc.Type) ir.Ref {
	cls := clsOf(t)
	b := g.cur
	switch c {
	case cc.AssignmentExpressionAdd:
		return b.Add(cls, l, r)
	case cc.AssignmentExpressionSub:
		return b.Sub(cls, l, r)
	case cc.AssignmentExpressionMul:
		return b.Mul(cls, l, r)
	case cc.AssignmentExpressionDiv:
		if signed(t) || isFloat(t) {
			return b.Div(cls, l, r)
		}
		return b.UDiv(cls, l, r)
	case cc.AssignmentExpressionMod:
		if signed(t) {
			return b.Rem(cls, l, r)
		}
		return b.URem(cls, l, r)
	case cc.AssignmentExpressionAnd:
		return b.And(cls, l, r)
	case cc.AssignmentExpressionOr:
		return b.Or(cls, l, r)
	case cc.AssignmentExpressionXor:
		return b.Xor(cls, l, r)
	case cc.AssignmentExpressionLsh:
		return b.Shl(cls, l, r)
	case cc.AssignmentExpressionRsh:
		if signed(t) {
			return b.Sar(cls, l, r)
		}
		return b.Shr(cls, l, r)
	}
	return g.fail("cc: bad compound assignment")
}
