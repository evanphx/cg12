package cc

import (
	"strings"

	"modernc.org/cc/v4"
)

// GNU inline assembly.
//
// modernc has no dedicated representation for an asm operand: it parses each
// `"constraint"(expr)` operand as an ordinary call expression whose callee is
// the constraint string literal and whose single argument is the operand. So
// the constraint and the operand are both recoverable (asmOperand), but the
// template's %-placeholder semantics are not modelled -- binding operands to the
// registers a real assembler template names would require a constraint-directed
// register allocator cg12 does not have.
//
// Instead, a curated set of well-known asm templates is recognized and lowered
// to IR whose meaning cg12 defines itself, using the recovered operands.
// Anything not recognized fails loudly rather than being silently dropped, which
// would miscompile the surrounding code.

// asmOperand recovers the constraint string and operand expression from an asm
// operand, which modernc parses as a call `"constraint"(operand)`.
func asmOperand(e cc.ExpressionNode) (constraint string, operand cc.ExpressionNode, ok bool) {
	pe, ok := e.(*cc.PostfixExpression)
	if !ok || pe.Case != cc.PostfixExpressionCall {
		return "", nil, false
	}
	if s, ok := asmStringLit(pe.PostfixExpression); ok {
		constraint = s
	}
	if pe.ArgumentExpressionList != nil {
		operand = pe.ArgumentExpressionList.AssignmentExpression
	}
	return constraint, operand, operand != nil
}

// asmStringLit returns the value of a string-literal expression.
func asmStringLit(e cc.ExpressionNode) (string, bool) {
	if pe, ok := e.(*cc.PrimaryExpression); ok && pe.Case == cc.PrimaryExpressionString {
		if s, ok := pe.Value().(cc.StringValue); ok {
			return strings.TrimRight(string(s), "\x00"), true
		}
	}
	return "", false
}

// genAsm lowers an inline-assembly statement.
func (g *gen) genAsm(as *cc.AsmStatement) {
	if as == nil || as.Asm == nil {
		return
	}
	a := as.Asm
	tmpl := asmTemplate(a)

	switch {
	case tmpl == "" || tmpl == "nop":
		// The empty template is the standard compiler barrier
		// (`__asm__ volatile("" ::: "memory")`) and `nop` is a hint; both are
		// no-ops for the values cg12 computes. Input operands are still evaluated
		// so their side effects are not lost.
		g.asmEvalInputs(a)
		return
	}
	g.fail("cc: unsupported inline asm %q: constraint-directed assembly is not supported", tmpl)
}

// asmTemplate returns the assembler template with its surrounding quotes removed.
func asmTemplate(a *cc.Asm) string {
	s := a.Token3.SrcStr()
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}

// asmEvalInputs evaluates the input operand expressions (the second `:` group)
// for their side effects, discarding the values. Outputs (group 0) are lvalues
// and clobbers (group 2) are string literals, so neither is evaluated.
func (g *gen) asmEvalInputs(a *cc.Asm) {
	group := 0
	for al := a.AsmArgList; al != nil; al = al.AsmArgList {
		if group == 1 {
			for el := al.AsmExpressionList; el != nil; el = el.AsmExpressionList {
				if _, operand, ok := asmOperand(el.AssignmentExpression); ok {
					g.genExpr(operand)
				}
			}
		}
		group++
	}
}
