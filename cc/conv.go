package cc

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// convert coerces a value from one C type to another (integer width and
// signedness, integer<->float, float<->float, pointers).
func (g *gen) convert(v ir.Ref, from, to cc.Type) ir.Ref {
	if from == nil || to == nil {
		return v
	}
	// A conversion touching a complex type is lowered on the {re,im} components.
	if isComplex(from) || isComplex(to) {
		return g.complexConvert(v, from, to)
	}
	if cc.IsComplexType(from) || cc.IsComplexType(to) {
		return g.fail("cc: unsupported complex conversion %v -> %v", from, to)
	}
	// A conversion touching a 128-bit integer is lowered on its {lo,hi} halves.
	if isInt128(from) || isInt128(to) {
		return g.int128Convert(v, from, to)
	}
	// long double conversions are soft-float calls; a quad-to-quad "conversion"
	// is a no-op on the value's address.
	if isLongDouble(from) || isLongDouble(to) {
		switch {
		case isLongDouble(from) && isLongDouble(to):
			return v
		case isLongDouble(to):
			return g.toQuad(v, from)
		default:
			return g.fromQuad(v, to)
		}
	}
	// A conversion to _Bool normalizes to 0/1 (any nonzero value becomes 1),
	// which a plain width change would not do.
	if to.Kind() == cc.Bool && from.Kind() != cc.Bool {
		if isFloat(from) {
			return g.cur.Cmp(ir.CmpFne, ir.ClsW, v, g.floatOf(0, from))
		}
		return g.cur.Cmp(ir.CmpNe, ir.ClsW, v, g.constOf(0, from))
	}
	fc, tc := clsOf(from), clsOf(to)
	ff, tf := isFloat(from), isFloat(to)
	b := g.cur
	switch {
	case fc == tc && ff == tf:
		// The computation class is unchanged, but the declared width may not be:
		// char and int are both ClsW, and a conversion between them still has to
		// throw bits away.
		return g.narrowInt(v, from, to, tc)
	case ff && tf: // float <-> float
		if tc == ir.ClsD {
			return b.Exts(v) // single -> double
		}
		return b.Truncd(v) // double -> single
	case ff && !tf: // float -> integer
		var iv ir.Ref
		if signed(to) {
			iv = b.Stosi(tc, v)
		} else {
			iv = b.Stoui(tc, v)
		}
		return g.narrowInt(iv, nil, to, tc)
	case !ff && tf: // integer -> float
		if signed(from) {
			return b.Sltof(tc, v)
		}
		return b.Ultof(tc, v)
	default: // integer width change (a long and a pointer are the same width)
		iv := v
		switch {
		case wide(tc) && fc == ir.ClsW:
			if signed(from) {
				iv = b.Extsw(ir.ClsL, v)
			} else {
				iv = b.Extuw(ir.ClsL, v)
			}
		case tc == ir.ClsW && wide(fc):
			iv = b.Copy(ir.ClsW, v) // truncate 64 -> 32
		}
		return g.narrowInt(iv, from, to, tc)
	}
}

// narrowInt re-presents v in a sub-word integer type's declared width. C
// conversion to char or short keeps only the low bits, and the target's
// signedness decides how they read back -- so (unsigned char)300 is 44 and
// (unsigned short)-1 is 65535. Nothing here is implied by the value class:
// char, short and int are all ClsW, so without this a narrowing cast is a no-op.
//
// from may be nil when the source is not an integer (a float conversion), in
// which case the narrowing always applies.
func (g *gen) narrowInt(v ir.Ref, from, to cc.Type, tc ir.Cls) ir.Ref {
	if !cc.IsIntegerType(to) || isInt128(to) {
		return v
	}
	sz := int(to.Size())
	if sz >= 4 {
		return v // a word or wider: the class change already presented it
	}
	// Already held in exactly this width and signedness: nothing to re-present.
	if from != nil && cc.IsIntegerType(from) && int(from.Size()) == sz && signed(from) == signed(to) {
		return v
	}
	switch sz {
	case 1:
		if signed(to) {
			return g.cur.Extsb(tc, v)
		}
		return g.cur.Extub(tc, v)
	case 2:
		if signed(to) {
			return g.cur.Extsh(tc, v)
		}
		return g.cur.Extuh(tc, v)
	}
	return v
}

// toPtr converts an integer index to the abstract pointer class so that address
// arithmetic computes at the target's pointer width. LowerPointers resolves the
// widen to a real word->long extend on a 64-bit target and to a no-op on wasm32
// (where the pointer and a word are the same width); a wider-than-pointer long
// index is reclassified with Copy, which truncates on a 32-bit target.
func (g *gen) toPtr(v ir.Ref, t cc.Type) ir.Ref {
	switch c := clsOf(t); {
	case c == ir.ClsP:
		return v
	case wide(c): // a long index: already >= pointer width
		return g.cur.Copy(ir.ClsP, v)
	case signed(t):
		return g.cur.Extsw(ir.ClsP, v)
	default:
		return g.cur.Extuw(ir.ClsP, v)
	}
}

// promote applies the default argument promotions for a variadic argument
// (float widens to double; integers already compute at int width).
func (g *gen) promote(v ir.Ref, t cc.Type) ir.Ref {
	if t.Kind() == cc.Float {
		return g.cur.Exts(v)
	}
	return v
}

// internStr interns a string literal as read-only data and returns its symbol
// name (independent of any function, so globals can point at it too).
func (g *gen) internStr(s string) string {
	name, ok := g.strs[s]
	if !ok {
		name = fmt.Sprintf("cstr%d", len(g.strs))
		g.strs[s] = name
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:    name,
			Align:   1,
			Linkage: ir.Linkage{Section: ".rodata"}, // string literals are read-only
			Items:   []ir.DataItem{{Str: s + "\x00"}},
		})
	}
	return name
}

// strSym interns a string literal and returns its address in the current function.
func (g *gen) strSym(s string) ir.Ref {
	return g.fn.Sym(g.internStr(s), 0)
}

// funcTypeOf extracts a function type from t (following one pointer level).
func funcTypeOf(t cc.Type) *cc.FunctionType {
	if ft, ok := t.(*cc.FunctionType); ok {
		return ft
	}
	if pt, ok := t.(*cc.PointerType); ok {
		if ft, ok := pt.Elem().(*cc.FunctionType); ok {
			return ft
		}
	}
	return nil
}

// genCall emits a function call, handling direct/indirect callees, fixed-argument
// coercion, and default promotions for variadic arguments.
func (g *gen) genCall(n *cc.PostfixExpression) ir.Ref {
	if r, ok := g.vaBuiltin(n); ok {
		return r
	}
	if r, ok := g.builtinCall(n); ok {
		return r
	}
	calleeNode := n.PostfixExpression
	ft := funcTypeOf(calleeNode.Type())

	var callee ir.Ref
	if pe, ok := calleeNode.(*cc.PrimaryExpression); ok && pe.Case == cc.PrimaryExpressionIdent {
		if v, found := g.lookup(pe.Token.SrcStr()); found {
			callee = g.loadVal(g.addrOf(v), v.typ) // a function-pointer variable
		} else {
			callee = g.fn.Sym(pe.Token.SrcStr(), 0)
		}
	} else {
		callee = g.genExpr(calleeNode)
	}

	// The argument list is stored head-first: each node's AssignmentExpression is
	// the earlier argument and ArgumentExpressionList holds the rest.
	var argNodes []cc.ExpressionNode
	for l := n.ArgumentExpressionList; l != nil; l = l.ArgumentExpressionList {
		argNodes = append(argNodes, l.AssignmentExpression)
	}

	nfixed := 0
	if ft != nil {
		nfixed = len(ft.Parameters())
	}
	args := make([]ir.Ref, 0, len(argNodes))
	var aggArgs []*ir.AggType // parallel to args; non-nil where an arg is by-value aggregate
	for i, an := range argNodes {
		var pt cc.Type
		if ft != nil && i < nfixed {
			pt = ft.Parameters()[i].Type()
		}
		// A by-value aggregate, long double, or complex argument is passed as a
		// pointer to its storage, tagged so the backend loads it per the ABI. The
		// effective type accounts for modernc dropping the complex kind from a
		// `real op _Complex float` argument.
		_, argComplex := g.effComplex(an)
		var v ir.Ref
		switch {
		case isMemValue(an.Type()) || (pt != nil && isMemValue(pt)) || argComplex:
			byValT := an.Type()
			if pt != nil {
				v = g.rval(an, pt) // coerce to the fixed parameter type
				byValT = pt
			} else {
				if argComplex && !isComplex(an.Type()) {
					return g.fail("cc: passing a %v-idiom complex through a variadic parameter is unsupported", an.Type())
				}
				v = g.genExpr(an) // variadic aggregate/complex: pass as-is
			}
			for len(aggArgs) <= i {
				aggArgs = append(aggArgs, nil)
			}
			aggArgs[i] = g.aggTypeOf(byValT)
		case pt != nil:
			v = g.rval(an, pt)
		default:
			v = g.promote(g.genExpr(an), an.Type())
		}
		args = append(args, v)
	}

	// A call returning a struct/union or long double by value yields a pointer to
	// the result.
	if ft != nil && isMemValue(ft.Result()) {
		r := g.cur.Call(ir.ClsL, callee, args...)
		call := g.lastInstr()
		call.RetAgg = g.aggTypeOf(ft.Result())
		call.AggArgs = aggArgs
		return r
	}
	if ft != nil && ft.Result().Kind() == cc.Void {
		g.cur.CallVoid(callee, args...)
		g.lastInstr().AggArgs = aggArgs
		return ir.R
	}
	retCls := ir.ClsW
	if ft != nil {
		retCls = clsOf(ft.Result())
	}
	r := g.cur.Call(retCls, callee, args...)
	g.lastInstr().AggArgs = aggArgs
	return r
}

// lastInstr returns the instruction most recently emitted into the current
// block, so a call's aggregate metadata can be attached after it is built.
func (g *gen) lastInstr() *ir.Instr {
	return &g.cur.Instrs[len(g.cur.Instrs)-1]
}

// promotedInt is a type's width and signedness after C's integer promotion: any
// integer narrower than int becomes int, since int holds every value of a char
// or a short. Pointers and anything else keep their own.
//
// This is the rank the usual arithmetic conversions actually compare. Comparing
// value classes instead collapses char, short and int into one width and hands
// the tiebreak to whichever operand happened to be unsigned.
func promotedInt(t cc.Type) (size int, isSigned bool) {
	if !cc.IsIntegerType(t) {
		return int(t.Size()), signed(t)
	}
	if sz := int(t.Size()); sz < 4 {
		return 4, true
	}
	return int(t.Size()), signed(t)
}
