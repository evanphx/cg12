package cc

import (
	"math/big"

	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// genExpr evaluates an expression to its rvalue.
func (g *gen) genExpr(e moderncc.ExpressionNode) ir.Ref {
	switch n := e.(type) {
	case *moderncc.PrimaryExpression:
		return g.genPrimary(n)
	case *moderncc.PostfixExpression:
		return g.genPostfix(n)
	case *moderncc.UnaryExpression:
		return g.genUnary(n)
	case *moderncc.CastExpression:
		return g.rval(n.CastExpression, n.Type())
	case *moderncc.MultiplicativeExpression:
		return g.arith(mulOp(n.Case), n.MultiplicativeExpression, n.CastExpression, n.Type())
	case *moderncc.AdditiveExpression:
		return g.arith(addOp(n.Case), n.AdditiveExpression, n.MultiplicativeExpression, n.Type())
	case *moderncc.ShiftExpression:
		return g.arith(shiftOp(n.Case), n.ShiftExpression, n.AdditiveExpression, n.Type())
	case *moderncc.AndExpression:
		return g.arith("&", n.AndExpression, n.EqualityExpression, n.Type())
	case *moderncc.ExclusiveOrExpression:
		return g.arith("^", n.ExclusiveOrExpression, n.AndExpression, n.Type())
	case *moderncc.InclusiveOrExpression:
		return g.arith("|", n.InclusiveOrExpression, n.ExclusiveOrExpression, n.Type())
	case *moderncc.RelationalExpression:
		return g.compare(relOp(n.Case), n.RelationalExpression, n.ShiftExpression)
	case *moderncc.EqualityExpression:
		return g.compare(eqOp(n.Case), n.EqualityExpression, n.RelationalExpression)
	case *moderncc.LogicalAndExpression:
		return g.logical(true, n.LogicalAndExpression, n.InclusiveOrExpression)
	case *moderncc.LogicalOrExpression:
		return g.logical(false, n.LogicalOrExpression, n.LogicalAndExpression)
	case *moderncc.ConditionalExpression:
		return g.genCond3(n)
	case *moderncc.AssignmentExpression:
		return g.genAssign(n)
	case *moderncc.ExpressionList:
		// A comma expression, stored head-first: AssignmentExpression is the
		// earlier operand (evaluated for effect), ExpressionList the rest, whose
		// value is the value of the whole expression.
		if n.ExpressionList == nil {
			return g.genExpr(n.AssignmentExpression)
		}
		g.genExpr(n.AssignmentExpression)
		return g.genExpr(n.ExpressionList)
	}
	return g.fail("cc: unsupported expression %T", e)
}

func mulOp(c moderncc.MultiplicativeExpressionCase) string {
	switch c {
	case moderncc.MultiplicativeExpressionDiv:
		return "/"
	case moderncc.MultiplicativeExpressionMod:
		return "%"
	default:
		return "*"
	}
}
func addOp(c moderncc.AdditiveExpressionCase) string {
	if c == moderncc.AdditiveExpressionSub {
		return "-"
	}
	return "+"
}
func shiftOp(c moderncc.ShiftExpressionCase) string {
	if c == moderncc.ShiftExpressionRsh {
		return ">>"
	}
	return "<<"
}
func relOp(c moderncc.RelationalExpressionCase) string {
	switch c {
	case moderncc.RelationalExpressionGt:
		return ">"
	case moderncc.RelationalExpressionLeq:
		return "<="
	case moderncc.RelationalExpressionGeq:
		return ">="
	default:
		return "<"
	}
}
func eqOp(c moderncc.EqualityExpressionCase) string {
	if c == moderncc.EqualityExpressionNeq {
		return "!="
	}
	return "=="
}

// --- primary expressions ---------------------------------------------------

func (g *gen) genPrimary(n *moderncc.PrimaryExpression) ir.Ref {
	// A folded complex constant (an imaginary literal like 2.0i, or _Complex_I)
	// materializes into a {re,im} slot.
	if isComplex(n.Type()) {
		if cv, ok := complexConstVal(n); ok {
			return g.complexConst(cv, complexCls(n.Type()))
		}
	}
	switch n.Case {
	case moderncc.PrimaryExpressionInt, moderncc.PrimaryExpressionChar, moderncc.PrimaryExpressionLChar:
		v, _ := constInt(n)
		return g.constOf(v, n.Type())
	case moderncc.PrimaryExpressionFloat:
		if ldv, ok := n.Value().(*moderncc.LongDoubleValue); ok {
			return g.quadConst((*big.Float)(ldv))
		}
		if v, ok := n.Value().(moderncc.Float64Value); ok {
			return g.floatOf(float64(v), n.Type())
		}
		return g.floatOf(0, n.Type())
	case moderncc.PrimaryExpressionString:
		s, _ := n.Value().(moderncc.StringValue)
		return g.strSym(string(s))
	case moderncc.PrimaryExpressionIdent:
		return g.loadLval(n)
	case moderncc.PrimaryExpressionExpr:
		return g.genExpr(n.ExpressionList)
	case moderncc.PrimaryExpressionGeneric:
		// _Generic: the type checker already picked the matching association.
		if a := n.GenericSelection.Associated(); a != nil {
			return g.genExpr(a.AssignmentExpression)
		}
		return g.fail("cc: _Generic with no matching association")
	case moderncc.PrimaryExpressionStmt:
		// A GNU statement expression ({ ... }): its value is the last statement.
		return g.genStmtExpr(n.CompoundStatement)
	}
	return g.fail("cc: unsupported primary expression case %v", n.Case)
}

// constOf builds an integer/pointer constant of the given type's class.
func (g *gen) constOf(v int64, t moderncc.Type) ir.Ref {
	if wide(clsOf(t)) {
		return g.fn.Long(v)
	}
	return g.fn.Word(v)
}

// loadLval loads the value of a named identifier (variable or the address of a
// function/array).
func (g *gen) loadLval(n *moderncc.PrimaryExpression) ir.Ref {
	name := n.Token.SrcStr()
	if v, ok := g.lookup(name); ok {
		addr := g.addrOf(v)
		if isVaList(v.typ) {
			return g.vaStorage(v) // forward the __va_list state address
		}
		if isArray(v.typ) || isMemValue(v.typ) {
			return addr // an array, aggregate, or long double value is its address
		}
		val := g.loadVal(addr, v.typ)
		g.setName(val, name) // this value is a read of the C variable
		return val
	}
	// An enumeration constant (or any identifier the type checker folded to a
	// constant) is used by value.
	if val, ok := constInt(n); ok {
		return g.constOf(val, n.Type())
	}
	// An unknown identifier is a global function or object referenced by symbol.
	// A thread-local object (declared extern in a header, so never defined here)
	// must still be reached through the TLS ABI, or the link fails with a mismatch
	// against its real thread-local definition.
	if isThreadLocalIdent(n) {
		return g.fn.ThreadSym(name)
	}
	return g.fn.Sym(name, 0)
}

// isThreadLocalIdent reports whether an identifier primary expression refers to a
// thread-local variable, so a symbol reference to it uses the TLS ABI even when
// the variable was declared extern in a header and thus never defined here.
func isThreadLocalIdent(n *moderncc.PrimaryExpression) bool {
	d, ok := n.ResolvedTo().(*moderncc.Declarator)
	return ok && d.IsThreadLocal()
}

// --- lvalue addresses ------------------------------------------------------

// genAddr returns the address of an lvalue expression.
func (g *gen) genAddr(e moderncc.ExpressionNode) (ir.Ref, moderncc.Type) {
	switch n := e.(type) {
	case *moderncc.PrimaryExpression:
		if n.Case == moderncc.PrimaryExpressionIdent {
			if v, ok := g.lookup(n.Token.SrcStr()); ok {
				return g.addrOf(v), v.typ
			}
			if isThreadLocalIdent(n) {
				return g.fn.ThreadSym(n.Token.SrcStr()), n.Type()
			}
			return g.fn.Sym(n.Token.SrcStr(), 0), n.Type()
		}
		if n.Case == moderncc.PrimaryExpressionExpr {
			return g.genAddr(n.ExpressionList)
		}
	case *moderncc.UnaryExpression:
		if n.Case == moderncc.UnaryExpressionDeref { // *p
			return g.genExpr(n.CastExpression), n.Type()
		}
	case *moderncc.PostfixExpression:
		switch n.Case {
		case moderncc.PostfixExpressionIndex: // a[i], equivalently i[a]
			var arrN, idxN moderncc.ExpressionNode = n.PostfixExpression, n.ExpressionList
			if !isPtrOrArray(arrN.Type()) {
				arrN, idxN = idxN, arrN
			}
			base, elemT := g.arrayBase(arrN)
			if at, ok := elemT.(*moderncc.ArrayType); ok && at.IsVLA() {
				// Indexing a multi-dimensional VLA: the row stride is a runtime value.
				idx := g.toPtr(g.genExpr(idxN), idxN.Type())
				off := g.cur.Mul(ir.ClsP, idx, g.vlaBytes(at))
				return g.cur.Add(ir.ClsP, base, off), elemT
			}
			return g.ptrIndex(base, false, g.genExpr(idxN), idxN.Type(), int64(elemT.Size())), elemT
		case moderncc.PostfixExpressionSelect: // s.field
			base, bt := g.genAddr(n.PostfixExpression)
			g.checkPackedBitfield(bt)
			fld := n.Field()
			g.checkAtomicMember(fld)
			return g.offset(base, int(fld.Offset())), fld.Type()
		case moderncc.PostfixExpressionPSelect: // p->field
			ptr := g.genExpr(n.PostfixExpression)
			g.checkPackedBitfield(pointee(n.PostfixExpression.Type()))
			fld := n.Field()
			g.checkAtomicMember(fld)
			return g.offset(ptr, int(fld.Offset())), fld.Type()
		case moderncc.PostfixExpressionComplit: // (T){ ... }
			return g.complit(n)
		}
	}
	// An aggregate rvalue -- a call result, a statement expression, a ?: of
	// structs -- is materialized in memory, and genExpr yields a pointer to that
	// storage. That pointer is its address, so f().field and the like work.
	if isMemValue(e.Type()) {
		return g.genExpr(e), e.Type()
	}
	g.fail("cc: expression is not an lvalue: %T", e)
	return ir.R, e.Type()
}

// arrayBase returns the base pointer and element type for indexing a[i] where a
// is an array (decays to a pointer) or a pointer.
func (g *gen) arrayBase(e moderncc.ExpressionNode) (ir.Ref, moderncc.Type) {
	if pt, ok := e.Type().(*moderncc.PointerType); ok {
		return g.genExpr(e), pt.Elem()
	}
	if at, ok := e.Type().(*moderncc.ArrayType); ok {
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

func (g *gen) genPostfix(n *moderncc.PostfixExpression) ir.Ref {
	switch n.Case {
	case moderncc.PostfixExpressionCall:
		return g.genCall(n)
	case moderncc.PostfixExpressionIndex:
		addr, t := g.genAddr(n)
		return g.rvalue(addr, t)
	case moderncc.PostfixExpressionSelect, moderncc.PostfixExpressionPSelect:
		if f, unit, ok := g.asBitfield(n); ok {
			return g.readBitfield(unit, f)
		}
		addr, t := g.genAddr(n)
		return g.rvalue(addr, t)
	case moderncc.PostfixExpressionComplit:
		addr, t := g.complit(n)
		return g.rvalue(addr, t)
	case moderncc.PostfixExpressionInc, moderncc.PostfixExpressionDec:
		if f, unit, ok := g.asBitfield(n.PostfixExpression); ok {
			old := g.readBitfield(unit, f)
			g.writeBitfield(unit, g.incDec(old, f.Type(), n.Case == moderncc.PostfixExpressionInc), f)
			return old
		}
		addr, t := g.genAddr(n.PostfixExpression)
		if isInt128(t) {
			old := g.int128Copy(addr)
			g.copyAgg(addr, g.int128IncDec(addr, n.Case == moderncc.PostfixExpressionInc), 16)
			return old
		}
		old := g.loadVal(addr, t)
		g.storeVal(addr, g.incDec(old, t, n.Case == moderncc.PostfixExpressionInc), t)
		return old // post-inc/dec yields the old value
	}
	return g.fail("cc: unsupported postfix case %v", n.Case)
}

// incDec adds or subtracts one (scaled by element size for pointers).
func (g *gen) incDec(v ir.Ref, t moderncc.Type, inc bool) ir.Ref {
	step := int64(1)
	if pt, ok := t.(*moderncc.PointerType); ok {
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

func (g *gen) genUnary(n *moderncc.UnaryExpression) ir.Ref {
	switch n.Case {
	case moderncc.UnaryExpressionMinus:
		if isInt128(n.Type()) {
			return g.int128Neg(n.CastExpression)
		}
		if comp, ok := g.effComplex(n.CastExpression); ok {
			return g.complexNeg(n.CastExpression, comp)
		}
		if isLongDouble(n.Type()) {
			return g.softcall("__negtf2", true, 0, qa(g.genExpr(n.CastExpression)))
		}
		v := g.genExpr(n.CastExpression)
		return g.cur.Neg(clsOf(n.Type()), v)
	case moderncc.UnaryExpressionReal: // __real__ z
		return g.complexReal(complexOperand(n))
	case moderncc.UnaryExpressionImag: // __imag__ z
		return g.complexImag(complexOperand(n))
	case moderncc.UnaryExpressionPlus:
		return g.genExpr(n.CastExpression)
	case moderncc.UnaryExpressionNot: // !x  ==  x == 0
		t := n.CastExpression.Type()
		if isLongDouble(t) { // !x  ==  (x == 0.0L)
			c := g.softcall("__eqtf2", false, ir.ClsW, qa(g.genExpr(n.CastExpression)), qa(g.quadZero()))
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, c, g.fn.Word(0))
		}
		v := g.genExpr(n.CastExpression)
		if isFloat(t) {
			return g.cur.Cmp(ir.CmpFeq, ir.ClsW, v, g.floatOf(0, t))
		}
		return g.cur.Cmp(ir.CmpEq, ir.ClsW, v, g.constOf(0, t))
	case moderncc.UnaryExpressionCpl: // ~x
		if isInt128(n.Type()) {
			return g.int128Cpl(n.CastExpression)
		}
		v := g.genExpr(n.CastExpression)
		cls := clsOf(n.Type())
		if wide(cls) {
			return g.cur.Xor(cls, v, g.fn.Long(-1))
		}
		return g.cur.Xor(cls, v, g.fn.Word(-1))
	case moderncc.UnaryExpressionDeref: // *p
		if r, ok := g.vaArgExpr(n); ok { // *(T*)__builtin_va_arg_impl(ap)
			return r
		}
		p := g.genExpr(n.CastExpression)
		// Dereferencing a function pointer yields a function designator, which
		// decays right back to the same address -- there is no load. This makes
		// the explicit (*fp)(args) call form equivalent to fp(args). cc decays
		// the deref's own result type back to a pointer, so test the operand.
		if funcTypeOf(n.CastExpression.Type()) != nil {
			return p
		}
		return g.rvalue(p, n.Type())
	case moderncc.UnaryExpressionAddrof: // &x
		addr, _ := g.genAddr(n.CastExpression)
		return addr
	case moderncc.UnaryExpressionInc, moderncc.UnaryExpressionDec:
		if f, unit, ok := g.asBitfield(n.UnaryExpression); ok {
			v := g.incDec(g.readBitfield(unit, f), f.Type(), n.Case == moderncc.UnaryExpressionInc)
			g.writeBitfield(unit, v, f)
			return v
		}
		addr, t := g.genAddr(n.UnaryExpression)
		if isInt128(t) {
			g.copyAgg(addr, g.int128IncDec(addr, n.Case == moderncc.UnaryExpressionInc), 16)
			return addr // prefix yields the new value
		}
		v := g.incDec(g.loadVal(addr, t), t, n.Case == moderncc.UnaryExpressionInc)
		g.storeVal(addr, v, t)
		return v
	case moderncc.UnaryExpressionSizeofExpr, moderncc.UnaryExpressionSizeofType:
		var t moderncc.Type
		if n.Case == moderncc.UnaryExpressionSizeofType {
			t = n.TypeName.Type()
		} else {
			// sizeof does not decay its array operand; recover the array type.
			t = n.UnaryExpression.Type()
			if pt, ok := t.(*moderncc.PointerType); ok && pt.Undecay() != nil {
				t = pt.Undecay()
			}
		}
		if at, ok := t.(*moderncc.ArrayType); ok && at.IsVLA() {
			return g.vlaBytes(at) // sizeof a VLA is its runtime byte size
		}
		v, ok := constInt(n)
		if !ok {
			return g.fail("cc: sizeof is not a constant here")
		}
		return g.fn.Long(v)
	case moderncc.UnaryExpressionAlignofType: // _Alignof(type)
		return g.fn.Long(int64(n.TypeName.Type().Align()))
	case moderncc.UnaryExpressionAlignofExpr: // _Alignof expr (GNU)
		return g.fn.Long(int64(n.UnaryExpression.Type().Align()))
	case moderncc.UnaryExpressionLabelAddr: // &&label
		if b, ok := g.labels[n.Token2.SrcStr()]; ok {
			return g.cur.BlockAddr(b)
		}
		return g.fail("cc: &&%s: unknown label", n.Token2.SrcStr())
	}
	return g.fail("cc: unsupported unary case %v", n.Case)
}

// --- binary arithmetic -----------------------------------------------------

func (g *gen) arith(op string, ln, rn moderncc.ExpressionNode, resT moderncc.Type) ir.Ref {
	// Pointer arithmetic: p +/- n scales n by the element size.
	if pt, ok := resT.(*moderncc.PointerType); ok && (op == "+" || op == "-") {
		return g.ptrArith(op, ln, rn, pt)
	}
	// Pointer difference: p - q is the byte distance divided by the element
	// size, yielding a signed ptrdiff_t. The result type is an integer (not a
	// pointer), so this is not caught by the pointer-arithmetic case above.
	if op == "-" && isPtrOrArray(ln.Type()) && isPtrOrArray(rn.Type()) {
		diff := g.ptrDiff(g.genExpr(ln), g.genExpr(rn), elemSize(ln.Type()))
		return g.convert(diff, ln.Type(), resT)
	}
	// Complex arithmetic lowers to component operations on the {re,im} pair.
	// effComplex also recovers the cases modernc mis-types as real.
	switch op {
	case "+", "-", "*", "/":
		if comp, ok := g.complexArithComp(ln, rn, resT); ok {
			return g.complexArith(op, ln, rn, comp)
		}
		if moderncc.IsComplexType(resT) {
			return g.fail("cc: unsupported complex type %v", resT)
		}
	}
	// 128-bit integer arithmetic lowers to 64-bit half operations (and libgcc
	// helpers for multiply, divide, remainder, and shifts).
	if isInt128(resT) {
		return g.int128Arith(op, ln, rn, resT)
	}
	// long double arithmetic is a soft-float call on the operands' addresses.
	if isLongDouble(resT) {
		l := g.convert(g.genExpr(ln), ln.Type(), resT)
		r := g.convert(g.genExpr(rn), rn.Type(), resT)
		return g.quadArith(op, l, r)
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

// ptrOffset scales an integer index by the element size, producing a byte
// offset in the pointer class. A unit element size needs no multiply.
func (g *gen) ptrOffset(idx ir.Ref, idxType moderncc.Type, elem int64) ir.Ref {
	off := g.toPtr(idx, idxType)
	if elem != 1 {
		off = g.cur.Mul(ir.ClsP, off, g.fn.ConstInt(ir.ClsP, elem))
	}
	return off
}

// ptrIndex computes the address of the idx-th element from base:
// base +/- idx*elem in the pointer class (subtracting when sub is true).
func (g *gen) ptrIndex(base ir.Ref, sub bool, idx ir.Ref, idxType moderncc.Type, elem int64) ir.Ref {
	off := g.ptrOffset(idx, idxType, elem)
	if sub {
		return g.cur.Sub(ir.ClsP, base, off)
	}
	return g.cur.Add(ir.ClsP, base, off)
}

// ptrDiff computes p - q for two pointers to the same element type: the byte
// distance between them divided by the element size, a signed ptrdiff_t.
func (g *gen) ptrDiff(l, r ir.Ref, elem int64) ir.Ref {
	diff := g.cur.Sub(ir.ClsL, g.cur.Copy(ir.ClsL, l), g.cur.Copy(ir.ClsL, r))
	if elem > 1 {
		diff = g.cur.Div(ir.ClsL, diff, g.fn.ConstInt(ir.ClsL, elem))
	}
	return diff
}

// ptrArith emits pointer +/- integer. For subtraction the pointer is the left
// operand (p - n); for addition either operand may be the pointer, since C
// makes p + n and n + p equivalent. The element size comes from the result
// (pointer) type, which matches whichever operand is the pointer.
func (g *gen) ptrArith(op string, ln, rn moderncc.ExpressionNode, pt *moderncc.PointerType) ir.Ref {
	baseN, idxN := ln, rn
	if op == "+" && !isPtrOrArray(ln.Type()) {
		baseN, idxN = rn, ln
	}
	return g.ptrIndex(g.genExpr(baseN), op == "-", g.genExpr(idxN), idxN.Type(), int64(pt.Elem().Size()))
}

// compare emits a relational/equality comparison producing a 0/1 int.
func (g *gen) compare(op string, ln, rn moderncc.ExpressionNode) ir.Ref {
	// Complex operands admit only == and !=, comparing both components.
	if isComplex(ln.Type()) || isComplex(rn.Type()) {
		return g.complexCompare(op, ln, rn)
	}
	// A 128-bit integer comparison (unless a float operand dominates, making the
	// common type floating) is done on the {lo,hi} halves. The comparison is
	// unsigned when either operand is an unsigned __int128.
	if (isInt128(ln.Type()) || isInt128(rn.Type())) && !isFloat(ln.Type()) && !isFloat(rn.Type()) {
		signedCmp := ln.Type().Kind() != moderncc.UInt128 && rn.Type().Kind() != moderncc.UInt128
		return g.int128Compare(op, ln, rn, signedCmp)
	}
	// Compare in the common type of the operands, following the usual
	// arithmetic conversions: long double > double > float > wider integer. A
	// floating operand always dominates an integer one -- comparing e.g. a
	// double against a long must widen the long to double, not truncate the
	// double to long.
	lt, rt := ln.Type(), rn.Type()
	var ct moderncc.Type
	ctSigned := false
	switch {
	case isLongDouble(lt):
		ct = lt
	case isLongDouble(rt):
		ct = rt
	case isFloat(lt) && isFloat(rt):
		ct = lt
		if rt.Kind() == moderncc.Double {
			ct = rt // double dominates float
		}
	case isFloat(lt):
		ct = lt
	case isFloat(rt):
		ct = rt
	default:
		// Both operands are integer (or pointer). The usual arithmetic conversions
		// apply integer promotion first: anything narrower than int becomes int,
		// because int holds every char and short value. That is what makes
		// `uc < -1` a SIGNED comparison and false -- promote first and the
		// unsigned-wins rule never gets to see the unsigned char at all.
		//
		// Promotion costs no instruction here: loadVal and the narrowing
		// conversions already present a sub-word value extended per its own type,
		// so its register holds exactly the int it promotes to. It only decides
		// which predicate to use, and whether a widening conversion is needed.
		lsz, lsg := promotedInt(lt)
		rsz, rsg := promotedInt(rt)
		switch {
		case lsz > rsz:
			ct, ctSigned = lt, lsg
		case rsz > lsz:
			ct, ctSigned = rt, rsg
		case lsg == rsg:
			ct, ctSigned = lt, lsg
		default:
			// Equal rank, one unsigned: the unsigned type wins, so -1 < 1u is false.
			ct, ctSigned = lt, false
			if lsg {
				ct = rt
			}
		}
		// Equal promoted width means both registers already hold the common type's
		// value, whatever the declared types were -- converting would only narrow
		// one of them back down.
		if lsz == rsz {
			l := g.genExpr(ln)
			r := g.genExpr(rn)
			return g.cur.Cmp(cmpPred(op, ctSigned, false), ir.ClsW, l, r)
		}
	}
	l := g.convert(g.genExpr(ln), lt, ct)
	r := g.convert(g.genExpr(rn), rt, ct)
	if isLongDouble(ct) { // soft-float comparison
		return g.quadCompare(op, l, r)
	}
	if isFloat(ct) {
		ctSigned = true // unused by the float predicates, but do not read as unsigned
	}
	pred := cmpPred(op, ctSigned, isFloat(ct))
	// A comparison yields an int; the operand class is carried by l and r, and
	// the predicate encodes signedness and float-ness.
	return g.cur.Cmp(pred, ir.ClsW, l, r)
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
func (g *gen) logical(and bool, ln, rn moderncc.ExpressionNode) ir.Ref {
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
func (g *gen) genCond3(n *moderncc.ConditionalExpression) ir.Ref {
	thenB, elseB, endB := g.block("qt"), g.block("qf"), g.block("qend")
	g.cur.Jnz(g.genCond(n.LogicalOrExpression), thenB, elseB)

	// A void ?: (both arms void -- e.g. `cond ? (void)0 : __builtin_unreachable()`,
	// the shape of assume/assert macros) yields no value, so evaluate each arm only
	// for its effect. Otherwise store each arm into a result slot. Either way an arm
	// may terminate its block (a __builtin_unreachable traps), so only fall through
	// to the join when it did not.
	voidCond := n.Type().Kind() == moderncc.Void
	var res ir.Ref
	if !voidCond {
		res = g.allocAligned(n.Type(), int(n.Type().Size()))
	}
	arm := func(e moderncc.ExpressionNode) {
		if voidCond {
			g.genExpr(e)
		} else {
			g.condArm(res, e, n.Type())
		}
		if !g.terminated() {
			g.cur.Goto(endB)
		}
	}
	g.cur = thenB
	arm(n.ExpressionList)
	g.cur = elseB
	arm(n.ConditionalExpression)
	g.cur = endB
	if voidCond {
		return ir.R
	}
	return g.rvalue(res, n.Type())
}

// condArm stores one arm of a ?: into the result slot: a byte copy for a
// memory value (struct/union/long double), a plain store otherwise.
func (g *gen) condArm(res ir.Ref, e moderncc.ExpressionNode, t moderncc.Type) {
	v := g.convert(g.genExpr(e), e.Type(), t)
	if isMemValue(t) {
		g.copyAgg(res, v, int(t.Size()))
		return
	}
	g.storeVal(res, v, t)
}

// --- assignment ------------------------------------------------------------

func (g *gen) genAssign(n *moderncc.AssignmentExpression) ir.Ref {
	// A bit-field target is spliced into its access unit rather than stored.
	if f, unit, ok := g.asBitfield(n.UnaryExpression); ok {
		rhs := g.convert(g.genExpr(n.AssignmentExpression), n.AssignmentExpression.Type(), f.Type())
		if n.Case == moderncc.AssignmentExpressionAssign {
			g.writeBitfield(unit, rhs, f)
			return rhs
		}
		v := g.combineOp(n.Case, g.readBitfield(unit, f), rhs, f.Type())
		g.writeBitfield(unit, v, f)
		return v
	}
	addr, t := g.genAddr(n.UnaryExpression)
	if n.Case == moderncc.AssignmentExpressionAssign {
		// va_copy(dst, src) arrives here as `dst = src`: modernc defines the
		// builtin as a macro that expands to a plain assignment. It models a
		// va_list as an 8-byte pointer, so that assignment copies 8 bytes -- but
		// cg12's va_list slot IS the state, and the state is vaListBytes long. The
		// copy has to be of the state, or the destination is left uninitialised and
		// the first va_arg through it reads whatever the slot held.
		if isVaList(t) {
			dst, src := g.vaListAddr(n.UnaryExpression), g.vaListAddr(n.AssignmentExpression)
			g.copyAgg(dst, src, vaListBytes)
			return dst
		}
		if isMemValue(t) { // struct/union/long-double/complex assignment is a byte copy
			src := g.rval(n.AssignmentExpression, t)
			g.copyAgg(addr, src, int(t.Size()))
			return addr
		}
		v := g.rval(n.AssignmentExpression, t)
		g.storeVal(addr, v, t)
		return v
	}
	// Compound assignment: load, combine, store.
	if isComplex(t) { // z op= x  combines both components (op is +, -, *, /)
		return g.complexCompound(compoundArithOp(n.Case), addr, t, n.AssignmentExpression)
	}
	if isInt128(t) { // 128-bit op= combines the {lo,hi} halves
		return g.int128Compound(compoundAllOp(n.Case), addr, t, n.AssignmentExpression)
	}
	if isLongDouble(t) { // r op= x  is a soft-float op on the operands' addresses
		old := g.rvalue(addr, t)
		rhs := g.convert(g.genExpr(n.AssignmentExpression), n.AssignmentExpression.Type(), t)
		v := g.quadArith(compoundArithOp(n.Case), old, rhs)
		g.copyAgg(addr, v, int(t.Size()))
		return addr
	}
	// p += n / p -= n on a pointer scales n by the element size, like p = p +/- n.
	if pt, ok := t.(*moderncc.PointerType); ok && (n.Case == moderncc.AssignmentExpressionAdd || n.Case == moderncc.AssignmentExpressionSub) {
		old := g.loadVal(addr, t)
		v := g.ptrIndex(old, n.Case == moderncc.AssignmentExpressionSub,
			g.genExpr(n.AssignmentExpression), n.AssignmentExpression.Type(), int64(pt.Elem().Size()))
		g.storeVal(addr, v, t)
		return v
	}
	old := g.loadVal(addr, t)
	rhs := g.convert(g.genExpr(n.AssignmentExpression), n.AssignmentExpression.Type(), t)
	v := g.combineOp(n.Case, old, rhs, t)
	g.storeVal(addr, v, t)
	return v
}

// compoundArithOp maps a compound-assignment case to its binary operator (only
// the arithmetic cases that apply to floating types).
func compoundArithOp(c moderncc.AssignmentExpressionCase) string {
	switch c {
	case moderncc.AssignmentExpressionAdd:
		return "+"
	case moderncc.AssignmentExpressionSub:
		return "-"
	case moderncc.AssignmentExpressionMul:
		return "*"
	case moderncc.AssignmentExpressionDiv:
		return "/"
	}
	return ""
}

// compoundAllOp maps every compound-assignment case to its binary operator,
// including the integer-only ones (%, shifts, bitwise) that __int128 admits.
func compoundAllOp(c moderncc.AssignmentExpressionCase) string {
	switch c {
	case moderncc.AssignmentExpressionMod:
		return "%"
	case moderncc.AssignmentExpressionLsh:
		return "<<"
	case moderncc.AssignmentExpressionRsh:
		return ">>"
	case moderncc.AssignmentExpressionAnd:
		return "&"
	case moderncc.AssignmentExpressionOr:
		return "|"
	case moderncc.AssignmentExpressionXor:
		return "^"
	}
	return compoundArithOp(c)
}

func (g *gen) combineOp(c moderncc.AssignmentExpressionCase, l, r ir.Ref, t moderncc.Type) ir.Ref {
	cls := clsOf(t)
	b := g.cur
	switch c {
	case moderncc.AssignmentExpressionAdd:
		return b.Add(cls, l, r)
	case moderncc.AssignmentExpressionSub:
		return b.Sub(cls, l, r)
	case moderncc.AssignmentExpressionMul:
		return b.Mul(cls, l, r)
	case moderncc.AssignmentExpressionDiv:
		if signed(t) || isFloat(t) {
			return b.Div(cls, l, r)
		}
		return b.UDiv(cls, l, r)
	case moderncc.AssignmentExpressionMod:
		if signed(t) {
			return b.Rem(cls, l, r)
		}
		return b.URem(cls, l, r)
	case moderncc.AssignmentExpressionAnd:
		return b.And(cls, l, r)
	case moderncc.AssignmentExpressionOr:
		return b.Or(cls, l, r)
	case moderncc.AssignmentExpressionXor:
		return b.Xor(cls, l, r)
	case moderncc.AssignmentExpressionLsh:
		return b.Shl(cls, l, r)
	case moderncc.AssignmentExpressionRsh:
		if signed(t) {
			return b.Sar(cls, l, r)
		}
		return b.Shr(cls, l, r)
	}
	return g.fail("cc: bad compound assignment")
}
