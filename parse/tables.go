package parse

import "github.com/evanphx/cg12/ir"

// parseClass reads a base-class or sub-class type word and returns the register
// class it materialises into (sub-word types widen to word).
func (p *parser) parseClass() ir.Cls {
	w := p.expectWord()
	if c, ok := classByName[w]; ok {
		return c
	}
	if s, ok := subByName[w]; ok {
		return s.Cls()
	}
	p.fail("expected a type, got %q", w)
	return ir.ClsW
}

// parseSub reads a data/aggregate element type word (b/h/w/l/s/d), and also
// accepts the signed/unsigned param sub-types for leniency.
func (p *parser) parseSub() ir.SubCls {
	w := p.expectWord()
	if s, ok := elemByName[w]; ok {
		return s
	}
	if s, ok := subByName[w]; ok {
		return s
	}
	p.fail("expected an element type, got %q", w)
	return ir.SubW
}

var classByName = map[string]ir.Cls{
	"w": ir.ClsW, "l": ir.ClsL, "s": ir.ClsS, "d": ir.ClsD, "p": ir.ClsP, "m": ir.ClsM,
}

var subByName = map[string]ir.SubCls{
	"sb": ir.SubB, "ub": ir.SubUB, "sh": ir.SubH, "uh": ir.SubUH,
	"w": ir.SubW, "l": ir.SubL, "s": ir.SubS, "d": ir.SubD,
}

// elemByName maps data/aggregate element-type names (b/h/w/l/s/d) to sub-classes.
var elemByName = map[string]ir.SubCls{
	"b": ir.SubB, "h": ir.SubH, "w": ir.SubW, "l": ir.SubL, "s": ir.SubS, "d": ir.SubD,
}

// isDataElem reports whether w introduces a data element type.
func isDataElem(w string) bool { _, ok := elemByName[w]; return ok }

// opAlias maps QBE mnemonics that cg12 spells differently (width-specific
// conversions, and the plain word load) onto cg12's opcodes.
var opAlias = map[string]ir.Op{
	"dtosi": ir.OStosi, "stosi": ir.OStosi,
	"dtoui": ir.OStoui, "stoui": ir.OStoui,
	"swtof": ir.OSltof, "sltof": ir.OSltof,
	"uwtof": ir.OUltof, "ultof": ir.OUltof,
	"loadw": ir.OLoaduw, // a plain word load (no extension for a w result)
}

// loadOpForClass returns the load opcode for QBE's generic "load", which reads a
// value of the instruction's own class.
func loadOpForClass(cls ir.Cls) ir.Op {
	switch cls {
	case ir.ClsL:
		return ir.OLoadl
	case ir.ClsS:
		return ir.OLoads
	case ir.ClsD:
		return ir.OLoadd
	default:
		return ir.OLoaduw
	}
}

var intCmpByName = map[string]ir.Cmp{
	"eq": ir.CmpEq, "ne": ir.CmpNe,
	"sle": ir.CmpSle, "slt": ir.CmpSlt, "sge": ir.CmpSge, "sgt": ir.CmpSgt,
	"ule": ir.CmpUle, "ult": ir.CmpUlt, "uge": ir.CmpUge, "ugt": ir.CmpUgt,
}

var fltCmpByName = map[string]ir.Cmp{
	"eq": ir.CmpFeq, "ne": ir.CmpFne, "le": ir.CmpFle, "lt": ir.CmpFlt,
	"ge": ir.CmpFge, "gt": ir.CmpFgt, "o": ir.CmpFo, "uo": ir.CmpFuo,
}

// parseCmp decodes a comparison mnemonic like "csltw" or "cod" into a predicate
// and the operand class it compares at.
func parseCmp(word string) (ir.Cmp, ir.Cls, bool) {
	if len(word) < 3 || word[0] != 'c' {
		return 0, 0, false
	}
	rest := word[1:]
	clsCh := rest[len(rest)-1:]
	predStr := rest[:len(rest)-1]
	cls, ok := classByName[clsCh]
	if !ok {
		return 0, 0, false
	}
	if cls.IsInt() {
		if cmp, ok := intCmpByName[predStr]; ok {
			return cmp, cls, true
		}
	} else {
		if cmp, ok := fltCmpByName[predStr]; ok {
			return cmp, cls, true
		}
	}
	return 0, 0, false
}

// opArgClass returns the class used to type an integer constant appearing as the
// i-th operand of op, whose result class is cls. It only needs to be correct for
// constants; temporaries carry their own class.
func opArgClass(op ir.Op, cls ir.Cls, i int) ir.Cls {
	switch {
	case op.IsLoad():
		return ir.ClsL // address
	case op.IsStore():
		if i == 0 {
			return storeValueClass(op)
		}
		return ir.ClsL // address
	case op.IsAlloc():
		return ir.ClsL // size
	default:
		return cls
	}
}

func storeValueClass(op ir.Op) ir.Cls {
	switch op {
	case ir.OStorel:
		return ir.ClsL
	case ir.OStores:
		return ir.ClsS
	case ir.OStored:
		return ir.ClsD
	default: // storeb, storeh, storew
		return ir.ClsW
	}
}
