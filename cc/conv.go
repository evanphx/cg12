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
	if fc == tc && ff == tf {
		return v
	}
	b := g.cur
	switch {
	case ff && tf: // float <-> float
		if tc == ir.ClsD {
			return b.Exts(v) // single -> double
		}
		return b.Truncd(v) // double -> single
	case ff && !tf: // float -> integer
		if signed(to) {
			return b.Stosi(tc, v)
		}
		return b.Stoui(tc, v)
	case !ff && tf: // integer -> float
		if signed(from) {
			return b.Sltof(tc, v)
		}
		return b.Ultof(tc, v)
	default: // integer width change (a long and a pointer are the same width)
		if wide(tc) && fc == ir.ClsW {
			if signed(from) {
				return b.Extsw(ir.ClsL, v)
			}
			return b.Extuw(ir.ClsL, v)
		}
		if tc == ir.ClsW && wide(fc) {
			return b.Copy(ir.ClsW, v) // truncate 64 -> 32
		}
		return v
	}
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
		v := g.genExpr(an)
		if isMemValue(an.Type()) {
			// A by-value aggregate or long double argument is passed as a pointer to
			// its storage (v is that address), tagged so the backend loads it per
			// the ABI. A fixed long double parameter still needs the conversion below.
			if ft != nil && i < nfixed {
				v = g.convert(v, an.Type(), ft.Parameters()[i].Type())
			}
			for len(aggArgs) <= i {
				aggArgs = append(aggArgs, nil)
			}
			aggArgs[i] = g.aggTypeOf(an.Type())
		} else if ft != nil && i < nfixed {
			v = g.convert(v, an.Type(), ft.Parameters()[i].Type())
		} else {
			v = g.promote(v, an.Type())
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
