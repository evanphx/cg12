package wasm

import "github.com/evanphx/cg12/ir"

// compare emits an OCmp: it pushes both operands and the predicate op, leaving a
// 0/1 i32 result. Ordered/unordered predicates that Wasm lacks natively are
// synthesised from eq/ne on each operand with itself.
func (e *emitter) compare(in *ir.Instr) {
	oc := e.f.ClassOf(in.Args[0])
	t := wasmType(oc)

	switch in.Cmp {
	case ir.CmpFo, ir.CmpFuo:
		// ordered   = (a==a) & (b==b); unordered = (a!=a) | (b!=b).
		self, comb := t+".eq", "i32.and"
		if in.Cmp == ir.CmpFuo {
			self, comb = t+".ne", "i32.or"
		}
		e.push(in.Args[0])
		e.push(in.Args[0])
		e.line("%s", self)
		e.push(in.Args[1])
		e.push(in.Args[1])
		e.line("%s", self)
		e.line("%s", comb)
		return
	}

	e.push(in.Args[0])
	e.push(in.Args[1])
	e.line("%s", cmpOp(in.Cmp, t))
}

// cmpOp returns the Wasm comparison mnemonic for a predicate at operand type t.
func cmpOp(p ir.Cmp, t string) string {
	switch p {
	case ir.CmpEq, ir.CmpFeq:
		return t + ".eq"
	case ir.CmpNe, ir.CmpFne:
		return t + ".ne"
	case ir.CmpSlt:
		return t + ".lt_s"
	case ir.CmpSle:
		return t + ".le_s"
	case ir.CmpSgt:
		return t + ".gt_s"
	case ir.CmpSge:
		return t + ".ge_s"
	case ir.CmpUlt:
		return t + ".lt_u"
	case ir.CmpUle:
		return t + ".le_u"
	case ir.CmpUgt:
		return t + ".gt_u"
	case ir.CmpUge:
		return t + ".ge_u"
	case ir.CmpFlt:
		return t + ".lt"
	case ir.CmpFle:
		return t + ".le"
	case ir.CmpFgt:
		return t + ".gt"
	case ir.CmpFge:
		return t + ".ge"
	}
	return "unreachable"
}

// convert emits a width/representation conversion. The operand class is read
// from the operand; the result class is in.Cls.
func (e *emitter) convert(in *ir.Instr) {
	src := e.f.ClassOf(in.Args[0])
	e.push(in.Args[0])
	for _, op := range convOps(in.Op, src, in.Cls) {
		e.line("%s", op)
	}
}

// convOps returns the Wasm op sequence realising a conversion opcode. The input
// value's type is src; the desired result type is dst. Sub-word extensions take
// a word operand (i32) and extend its low byte/halfword into the result class,
// which is why the long-result forms both narrow and then widen.
func convOps(op ir.Op, src, dst ir.Cls) []string {
	dt := wasmType(dst)
	switch op {
	case ir.OExtsb:
		if dst == ir.ClsL {
			return []string{"i32.extend8_s", "i64.extend_i32_s"}
		}
		return []string{"i32.extend8_s"}
	case ir.OExtsh:
		if dst == ir.ClsL {
			return []string{"i32.extend16_s", "i64.extend_i32_s"}
		}
		return []string{"i32.extend16_s"}
	case ir.OExtub:
		if dst == ir.ClsL {
			return []string{"i32.const 255", "i32.and", "i64.extend_i32_u"}
		}
		return []string{"i32.const 255", "i32.and"}
	case ir.OExtuh:
		if dst == ir.ClsL {
			return []string{"i32.const 65535", "i32.and", "i64.extend_i32_u"}
		}
		return []string{"i32.const 65535", "i32.and"}
	case ir.OExtsw:
		if dst == ir.ClsL {
			return []string{"i64.extend_i32_s"}
		}
		return nil
	case ir.OExtuw:
		if dst == ir.ClsL {
			return []string{"i64.extend_i32_u"}
		}
		return nil

	case ir.OExts: // single -> double
		return []string{"f64.promote_f32"}
	case ir.OTruncd: // double -> single
		return []string{"f32.demote_f64"}

	case ir.OStosi:
		return []string{dt + ".trunc_" + wasmType(src) + "_s"}
	case ir.OStoui:
		return []string{dt + ".trunc_" + wasmType(src) + "_u"}
	case ir.OSltof:
		return []string{dt + ".convert_" + wasmType(src) + "_s"}
	case ir.OUltof:
		return []string{dt + ".convert_" + wasmType(src) + "_u"}

	case ir.OCast:
		return []string{dt + ".reinterpret_" + wasmType(src)}
	}
	return nil
}
