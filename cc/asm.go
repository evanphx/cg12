package cc

import (
	"strconv"
	"strings"

	"github.com/evanphx/cg12/ir"
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

// genAsm lowers an inline-assembly statement to an OAsm instruction, which a
// backend emitting assembly text passes through (substituting the %N operand
// placeholders with the registers the allocator assigns). Supported operands are
// register outputs ("=r"/"=&r"), register inputs ("r"), immediate inputs ("i"),
// and memory operands ("m"/"=m", substituted as a reference through the operand's
// address); any other constraint fails loudly rather than miscompiling.
func (g *gen) genAsm(as *cc.AsmStatement) {
	if as == nil || as.Asm == nil {
		return
	}
	a := as.Asm
	tmpl := asmTemplate(a)

	if tmpl == "" || tmpl == "nop" {
		// The empty template is the standard compiler barrier
		// (`__asm__ volatile("" ::: "memory")`) and `nop` is a hint; both are
		// no-ops for the values cg12 computes. Input operands are still evaluated
		// so their side effects are not lost.
		g.asmEvalInputs(a)
		return
	}

	specs, outLvals, ok := g.asmCollect(a)
	if !ok {
		return // asmCollect already recorded the failure
	}
	res := g.cur.Asm(tmpl, specs)
	// res holds the register-output temporaries in order; store each back to its
	// lvalue. Memory outputs ("=m") are written by the asm directly.
	for i, lv := range outLvals {
		g.storeVal(lv.addr, res[i], lv.typ)
	}
}

// asmOut is a register-output lvalue: the address to store the asm's result to
// and the C type of that lvalue.
type asmOut struct {
	addr ir.Ref
	typ  cc.Type
}

// asmCollect gathers an inline-asm statement's operands into specs in %N order.
// Group 0 (the first `:`) is outputs, group 1 is inputs, and later groups are
// clobbers (bare strings, which carry no operand). Outputs may be "=r"/"=&r" (a
// register) or "=m" (memory); inputs may be "r" (a register), "i" (a constant
// immediate), or "m" (memory). outLvals, parallel to the register-output specs,
// records where to store each register result back. Unsupported constraints fail
// loudly.
func (g *gen) asmCollect(a *cc.Asm) (specs []ir.AsmSpec, outLvals []asmOut, ok bool) {
	group := 0
	for al := a.AsmArgList; al != nil; al = al.AsmArgList {
		for el := al.AsmExpressionList; el != nil; el = el.AsmExpressionList {
			cons, operand, isOp := asmOperand(el.AssignmentExpression)
			if !isOp {
				continue // a clobber string, not an operand
			}
			if group == 0 { // outputs
				switch cons {
				case "=r", "=&r": // "=&r" (early clobber) is the same here: outputs are always kept distinct
					addr, typ := g.genAddr(operand)
					specs = append(specs, ir.AsmSpec{Kind: ir.AsmRegOut, Cls: clsOf(typ)})
					outLvals = append(outLvals, asmOut{addr, typ})
				case "=m", "=&m":
					addr, _ := g.genAddr(operand)
					specs = append(specs, ir.AsmSpec{Kind: ir.AsmMem, Ref: addr})
				default:
					g.fail("cc: unsupported inline-asm output constraint %q", cons)
					return nil, nil, false
				}
				continue
			}
			// inputs
			switch cons {
			case "r":
				specs = append(specs, ir.AsmSpec{Kind: ir.AsmRegIn, Ref: g.genExpr(operand)})
			case "i":
				v, isConst := constInt(operand)
				if !isConst {
					g.fail("cc: inline-asm \"i\" operand is not a constant expression")
					return nil, nil, false
				}
				specs = append(specs, ir.AsmSpec{Kind: ir.AsmImm, Ref: g.fn.Long(v)})
			case "m":
				addr, _ := g.genAddr(operand)
				specs = append(specs, ir.AsmSpec{Kind: ir.AsmMem, Ref: addr})
			default:
				g.fail("cc: unsupported inline-asm input constraint %q", cons)
				return nil, nil, false
			}
		}
		group++
	}
	return specs, outLvals, true
}

// asmTemplate returns the assembler template with its surrounding quotes removed
// and its escape sequences (\n, \t, ...) decoded, so a multi-instruction template
// becomes real newlines the emitter can split on.
func asmTemplate(a *cc.Asm) string {
	s := a.Token3.SrcStr()
	if unq, err := strconv.Unquote(s); err == nil {
		return strings.TrimSpace(unq)
	}
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
