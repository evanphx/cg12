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

// asmIsGoto reports whether an asm statement carries the `goto` qualifier.
func asmIsGoto(a *cc.Asm) bool {
	for q := a.AsmQualifierList; q != nil; q = q.AsmQualifierList {
		if q.AsmQualifier != nil && q.AsmQualifier.Case == cc.AsmQualifierGoto {
			return true
		}
	}
	return false
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
	if asmIsGoto(a) {
		// `asm goto` branches to C labels, so it would need to lower to a
		// multi-successor terminator wired into the CFG. Fail loudly rather than
		// dropping the branch (which would fall through and miscompile).
		g.fail("cc: `asm goto` is not supported")
		return
	}
	tmpl := asmTemplate(a)

	// The empty template is the standard compiler barrier,
	// `__asm__ volatile("" ::: "memory")`. It emits no instructions, which is
	// exactly why it must still reach the IR: its whole purpose is to stop the
	// optimizer moving memory across it, and dropping it here for emitting
	// nothing gets that backwards. opt/loadelim and opt/dce already treat an OAsm
	// as an unknown memory effect, so the barrier works the moment it exists.
	//
	// Both assemblers encode an empty template to zero bytes, so it costs no code.

	specs, outLvals, clobbers, ok := g.asmCollect(a)
	if !ok {
		return // asmCollect already recorded the failure
	}
	res := g.cur.Asm(tmpl, specs)
	if len(clobbers) > 0 {
		g.cur.Instrs[len(g.cur.Instrs)-1].Asm.Clobbers = clobbers
	}
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
func (g *gen) asmCollect(a *cc.Asm) (specs []ir.AsmSpec, outLvals []asmOut, clobbers []string, ok bool) {
	group := 0
	for al := a.AsmArgList; al != nil; al = al.AsmArgList {
		for el := al.AsmExpressionList; el != nil; el = el.AsmExpressionList {
			cons, operand, isOp := asmOperand(el.AssignmentExpression)
			if !isOp {
				// A bare string in the third group is a clobber. It names a register
				// the template writes, which the allocator has to know about: a value
				// it is holding there cannot survive the asm.
				if group >= 2 {
					if name, isStr := asmStringLit(el.AssignmentExpression); isStr {
						clobbers = append(clobbers, name)
					}
				}
				continue
			}
			base, output, rw := asmParseConstraint(cons)
			if (group == 0) != output {
				g.fail("cc: inline-asm operand %q is in the wrong constraint group", cons)
				return nil, nil, nil, false
			}
			spec, lval, err := g.asmOperandSpec(base, output, rw, operand)
			if err != "" {
				g.fail("cc: %s", err)
				return nil, nil, nil, false
			}
			specs = append(specs, spec)
			if spec.Kind == ir.AsmRegOut || spec.Kind == ir.AsmRegInOut {
				outLvals = append(outLvals, lval)
			}
		}
		group++
	}
	return specs, outLvals, clobbers, true
}

// asmParseConstraint strips the '='/'+'/'&' modifiers from a constraint, yielding
// the base letter and whether the operand is an output and read-write.
func asmParseConstraint(c string) (base string, output, readwrite bool) {
	for len(c) > 0 {
		switch c[0] {
		case '=':
			output = true
		case '+':
			output, readwrite = true, true
		case '&':
			// early clobber: cg12 keeps outputs distinct anyway
		default:
			return c, output, readwrite
		}
		c = c[1:]
	}
	return c, output, readwrite
}

// asmFixedReg reports whether a base constraint names a single fixed register
// (the common x86 letters). A backend maps the letter to a physical register.
func asmFixedReg(base string) bool {
	switch base {
	case "a", "b", "c", "d", "S", "D":
		return true
	}
	return false
}

// asmOperandSpec builds the AsmSpec (and, for a register output, the store-back
// lvalue) for one operand from its base constraint.
func (g *gen) asmOperandSpec(base string, output, rw bool, operand cc.ExpressionNode) (ir.AsmSpec, asmOut, string) {
	switch {
	case base == "r" || base == "w" || (base == "x" && g.target == TargetAMD64) || asmFixedReg(base):
		// "w" is how AArch64 spells a floating-point register operand, "x" is how
		// x86-64 does. cg12's "r" already picks the register file from the operand's
		// own class -- a double gets a V or an XMM either way -- so the letters
		// agree here. Accepting them means source written for gcc, which requires
		// the target's own letter for a double and refuses "r", compiles as written.
		//
		// "x" is taken only for amd64, because it does not mean the same thing on
		// both: on x86-64 it is any XMM, which is exactly what the class picks, but
		// on AArch64 it is V0-V15 specifically -- a restriction cg12 cannot honour,
		// so accepting it there would quietly allocate V16+ for a template that
		// needs a low register.
		reg := ""
		if base != "r" && base != "w" && base != "x" {
			reg = base
		}
		switch {
		case rw: // "+r" / "+a": read-write register
			addr, typ := g.genAddr(operand)
			return ir.AsmSpec{Kind: ir.AsmRegInOut, Cls: clsOf(typ), Ref: g.loadVal(addr, typ), Reg: reg}, asmOut{addr, typ}, ""
		case output: // "=r" / "=a"
			addr, typ := g.genAddr(operand)
			return ir.AsmSpec{Kind: ir.AsmRegOut, Cls: clsOf(typ), Reg: reg}, asmOut{addr, typ}, ""
		default: // "r" / "a" input
			// A fixed-register input is materialized into a dedicated pre-colored
			// temporary by the backend, so no copy is inserted here.
			return ir.AsmSpec{Kind: ir.AsmRegIn, Ref: g.genExpr(operand), Reg: reg}, asmOut{}, ""
		}
	case base == "m":
		addr, _ := g.genAddr(operand) // memory is read/written in place; "+m" == "=m"
		return ir.AsmSpec{Kind: ir.AsmMem, Ref: addr}, asmOut{}, ""
	case base == "i" && !output:
		v, isConst := constInt(operand)
		if !isConst {
			return ir.AsmSpec{}, asmOut{}, "inline-asm \"i\" operand is not a constant expression"
		}
		return ir.AsmSpec{Kind: ir.AsmImm, Ref: g.fn.Long(v)}, asmOut{}, ""
	}
	return ir.AsmSpec{}, asmOut{}, "unsupported inline-asm constraint " + strconv.Quote(base)
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
