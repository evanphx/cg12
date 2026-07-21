package cc

import (
	"strings"

	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// vaListBytes is the size of the target's va_list state. cg12's VaStart/VaArg
// implement the AArch64 AAPCS __va_list (a 32-byte struct of stack/register save
// pointers and offsets). modernc/cc models va_list as a plain 8-byte pointer, so
// the front end allocates this size instead and always addresses it by pointer.
const vaListBytes = 32

// isVaList reports whether t is the va_list type.
func isVaList(t moderncc.Type) bool {
	if _, ok := t.(*moderncc.PointerType); !ok {
		return false
	}
	return strings.Contains(t.String(), "va_list")
}

// Support for variadic function definitions. modernc/cc lowers the <stdarg.h>
// macros to builtin calls:
//
//	va_start(ap, last) -> __builtin_va_start(ap, last)
//	va_arg(ap, T)      -> *(T*)__builtin_va_arg_impl(ap)
//	va_end(ap)         -> __builtin_va_end(ap)
//
// cg12 models the same operations directly: VaStart initializes the list and
// VaArg fetches the next value, both addressing the va_list storage the callee
// declared (a plain local, which the backend lays out and fills per the ABI).

// calleeIdent returns the name of a direct call's callee, or "" if the callee is
// not a plain identifier.
func calleeIdent(pe *moderncc.PostfixExpression) string {
	if p, ok := pe.PostfixExpression.(*moderncc.PrimaryExpression); ok && p.Case == moderncc.PrimaryExpressionIdent {
		return p.Token.SrcStr()
	}
	return ""
}

// vaBuiltin handles the va_start/va_end builtin calls, reporting whether the
// call was one of them. va_arg is handled separately (it is wrapped in a
// dereference that carries the requested type).
func (g *gen) vaBuiltin(n *moderncc.PostfixExpression) (ir.Ref, bool) {
	switch calleeIdent(n) {
	case "__builtin_va_start":
		// __builtin_va_start(ap, last): the second argument (the last named
		// parameter) is only a marker; the backend derives the register save
		// area from the function's own parameter list.
		if a := n.ArgumentExpressionList; a != nil {
			g.cur.VaStart(g.vaListAddr(a.AssignmentExpression))
		}
		return ir.R, true
	case "__builtin_va_end":
		return ir.R, true // nothing to tear down
	}
	return ir.R, false
}

// vaArgExpr matches the *(T*)__builtin_va_arg_impl(ap) shape that va_arg expands
// to and, if it fits, emits a VaArg for the next argument of type T.
func (g *gen) vaArgExpr(n *moderncc.UnaryExpression) (ir.Ref, bool) {
	call := unwrapToCall(n.CastExpression)
	if call == nil || calleeIdent(call) != "__builtin_va_arg_impl" {
		return ir.R, false
	}
	ap := g.vaListAddr(call.ArgumentExpressionList.AssignmentExpression)
	return g.cur.VaArg(clsOf(n.Type()), ap), true // n.Type() is the dereferenced type T
}

// vaListAddr returns the address of the __va_list state named by e. A local
// va_list is that state (its address is used); a va_list parameter holds a
// pointer to state in the caller (the pointer is loaded).
func (g *gen) vaListAddr(e moderncc.ExpressionNode) ir.Ref {
	if name, ok := identName(e); ok {
		if v, found := g.lookup(name); found {
			return g.vaStorage(v)
		}
	}
	addr, _ := g.genAddr(e)
	return addr
}

// identName peels casts and parentheses to reach a bare identifier, returning
// its name.
func identName(e moderncc.ExpressionNode) (string, bool) {
	for {
		switch x := e.(type) {
		case *moderncc.PrimaryExpression:
			switch x.Case {
			case moderncc.PrimaryExpressionIdent:
				return x.Token.SrcStr(), true
			case moderncc.PrimaryExpressionExpr:
				e = x.ExpressionList
			default:
				return "", false
			}
		case *moderncc.CastExpression:
			e = x.CastExpression
		default:
			return "", false
		}
	}
}

// unwrapToCall peels casts and parentheses off e to reach the call it wraps, if
// any.
func unwrapToCall(e moderncc.ExpressionNode) *moderncc.PostfixExpression {
	for {
		switch x := e.(type) {
		case *moderncc.CastExpression:
			e = x.CastExpression
		case *moderncc.PrimaryExpression:
			if x.Case == moderncc.PrimaryExpressionExpr {
				e = x.ExpressionList
				continue
			}
			return nil
		case *moderncc.PostfixExpression:
			if x.Case == moderncc.PostfixExpressionCall {
				return x
			}
			return nil
		default:
			return nil
		}
	}
}
