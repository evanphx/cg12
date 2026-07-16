package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// clsSize returns 8 for a 64-bit class, else 4.
func clsSize(cls ir.Cls) int {
	if cls == ir.ClsL {
		return 8
	}
	return 4
}

func (e *emitter) instr(in *ir.Instr) {
	// Two-operand integer arithmetic is selected once, through the shared builder.
	if (&xsel{f: e.f, b: &textXasm{e: e}}).selectInt(in) {
		return
	}
	switch in.Op {
	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		d, commit := e.gpDst(in.To)
		e.line("leaq %s, %s", memn(RBP, int32(-e.allocOff[in])), gpn(d, 8))
		commit()
	case ir.OAsm:
		e.emitAsm(in)
	case ir.OVaStart:
		e.vaStart(in)
	case ir.OVaArg:
		e.vaArg(in)
	case ir.OSafepoint:
		// No code (the text path has no GC-strategy hook).
	}
}

func (e *emitter) binInt(in *ir.Instr) {
	sz := clsSize(in.Cls)
	d, commit := e.gpDst(in.To)
	rb := e.gpValue(in.Arg(1), gpScratch1)
	if rb == d {
		e.line("mov%s %s, %s", suf(sz), gpn(rb, sz), gpn(gpScratch1, sz))
		rb = gpScratch1
	}
	e.gpInto(d, in.Arg(0))
	mn := map[ir.Op]string{ir.OAdd: "add", ir.OSub: "sub", ir.OAnd: "and", ir.OOr: "or", ir.OXor: "xor"}[in.Op]
	if in.Op == ir.OMul {
		e.line("imul%s %s, %s", suf(sz), gpn(rb, sz), gpn(d, sz))
	} else {
		e.line("%s%s %s, %s", mn, suf(sz), gpn(rb, sz), gpn(d, sz))
	}
	commit()
}

func (e *emitter) binFP(in *ir.Instr) {
	dbl := in.Cls == ir.ClsD
	d, commit := e.fpDst(in.To)
	rb := e.fpValue(in.Arg(1), fpScratch1)
	if rb == d {
		mv := "movsd"
		if !dbl {
			mv = "movss"
		}
		e.line("%s %s, %s", mv, xmmn(rb), xmmn(fpScratch1))
		rb = fpScratch1
	}
	e.fpInto(d, in.Arg(0))
	base := map[ir.Op]string{ir.OAdd: "add", ir.OSub: "sub", ir.OMul: "mul", ir.ODiv: "div"}[in.Op]
	sfx := "sd"
	if !dbl {
		sfx = "ss"
	}
	e.line("%s%s %s, %s", base, sfx, xmmn(rb), xmmn(d))
	commit()
}

func (e *emitter) divInt(in *ir.Instr, signed, rem bool) {
	sz := clsSize(in.Cls)
	rb := e.gpValue(in.Arg(1), gpScratch1)
	e.gpInto(RAX, in.Arg(0))
	if signed {
		if sz == 8 {
			e.line("cqto")
		} else {
			e.line("cltd")
		}
		e.line("idiv%s %s", suf(sz), gpn(rb, sz))
	} else {
		e.line("xorl %%edx, %%edx")
		e.line("div%s %s", suf(sz), gpn(rb, sz))
	}
	d, commit := e.gpDst(in.To)
	res := RAX
	if rem {
		res = RDX
	}
	e.line("mov%s %s, %s", suf(sz), gpn(res, sz), gpn(d, sz))
	commit()
}

func (e *emitter) shift(in *ir.Instr) {
	sz := clsSize(in.Cls)
	mn := map[ir.Op]string{ir.OShl: "shl", ir.OShr: "shr", ir.OSar: "sar"}[in.Op]
	d, commit := e.gpDst(in.To)
	if c := e.constOf(in.Arg(1)); c != nil && c.Kind == ir.ConstInt {
		e.gpInto(d, in.Arg(0))
		e.line("%s%s $%d, %s", mn, suf(sz), c.Int&63, gpn(d, sz))
		commit()
		return
	}
	e.gpInto(RCX, in.Arg(1))
	e.gpInto(d, in.Arg(0))
	e.line("%s%s %%cl, %s", mn, suf(sz), gpn(d, sz))
	commit()
}

func (e *emitter) neg(in *ir.Instr) {
	if in.Cls.IsFloat() {
		dbl := in.Cls == ir.ClsD
		d, commit := e.fpDst(in.To)
		e.fpInto(d, in.Arg(0))
		if dbl {
			e.line("movabsq $0x8000000000000000, %s", gpn(gpScratch0, 8))
			e.line("movq %s, %s", gpn(gpScratch0, 8), xmmn(fpScratch1))
			e.line("xorpd %s, %s", xmmn(fpScratch1), xmmn(d))
		} else {
			e.line("movl $0x80000000, %s", gpn(gpScratch0, 4))
			e.line("movd %s, %s", gpn(gpScratch0, 4), xmmn(fpScratch1))
			e.line("xorps %s, %s", xmmn(fpScratch1), xmmn(d))
		}
		commit()
		return
	}
	sz := clsSize(in.Cls)
	d, commit := e.gpDst(in.To)
	e.gpInto(d, in.Arg(0))
	e.line("neg%s %s", suf(sz), gpn(d, sz))
	commit()
}

// intCC maps an integer predicate to its condition suffix (flags from a - b).
var intCC = map[ir.Cmp]string{
	ir.CmpEq: "e", ir.CmpNe: "ne", ir.CmpSlt: "l", ir.CmpSle: "le",
	ir.CmpSgt: "g", ir.CmpSge: "ge", ir.CmpUlt: "b", ir.CmpUle: "be",
	ir.CmpUgt: "a", ir.CmpUge: "ae",
}

func (e *emitter) cmp(in *ir.Instr) {
	if in.Cmp.IsFloat() {
		e.fcmp(in)
		return
	}
	sz := clsSize(e.f.ClassOf(in.Arg(0)))
	ra := e.gpValue(in.Arg(0), gpScratch0)
	rb := e.gpValue(in.Arg(1), gpScratch1)
	e.line("cmp%s %s, %s", suf(sz), gpn(rb, sz), gpn(ra, sz))
	d, commit := e.gpDst(in.To)
	e.line("set%s %s", intCC[in.Cmp], gpn(d, 1))
	e.line("movzbl %s, %s", gpn(d, 1), gpn(d, 4))
	commit()
}

func (e *emitter) fcmp(in *ir.Instr) {
	dbl := e.f.ClassOf(in.Arg(0)) == ir.ClsD
	a := e.fpValue(in.Arg(0), fpScratch0)
	b := e.fpValue(in.Arg(1), fpScratch1)
	ucomi := func(x, y Reg) {
		mn := "ucomisd"
		if !dbl {
			mn = "ucomiss"
		}
		e.line("%s %s, %s", mn, xmmn(y), xmmn(x))
	}
	d, commit := e.gpDst(in.To)
	d8 := gpn(d, 1)
	switch in.Cmp {
	case ir.CmpFgt:
		ucomi(a, b)
		e.line("seta %s", d8)
	case ir.CmpFge:
		ucomi(a, b)
		e.line("setae %s", d8)
	case ir.CmpFlt:
		ucomi(b, a)
		e.line("seta %s", d8)
	case ir.CmpFle:
		ucomi(b, a)
		e.line("setae %s", d8)
	case ir.CmpFo:
		ucomi(a, b)
		e.line("setnp %s", d8)
	case ir.CmpFuo:
		ucomi(a, b)
		e.line("setp %s", d8)
	case ir.CmpFeq:
		ucomi(a, b)
		e.line("setnp %s", gpn(gpScratch1, 1))
		e.line("sete %s", d8)
		e.line("andb %s, %s", gpn(gpScratch1, 1), d8)
	case ir.CmpFne:
		ucomi(a, b)
		e.line("setp %s", gpn(gpScratch1, 1))
		e.line("setne %s", d8)
		e.line("orb %s, %s", gpn(gpScratch1, 1), d8)
	}
	e.line("movzbl %s, %s", d8, gpn(d, 4))
	commit()
}

// memAddr renders a load/store address as a memory operand: a direct
// (non-thread-local) symbol becomes RIP-relative; anything else is computed into
// a register.
func (e *emitter) memAddr(addr ir.Ref, scratch Reg) string {
	if c := e.constOf(addr); c != nil && c.Kind == ir.ConstSym && !c.Thread {
		name := sanitize(c.Sym)
		if c.Int != 0 {
			return fmt.Sprintf("%s+%d(%%rip)", name, c.Int)
		}
		return name + "(%rip)"
	}
	r := e.gpValue(addr, scratch)
	return memn(r, 0)
}

func (e *emitter) load(in *ir.Instr) {
	mem := e.memAddr(in.Arg(0), gpScratch1)
	if in.Op == ir.OLoads || in.Op == ir.OLoadd {
		d, commit := e.fpDst(in.To)
		mn := "movsd"
		if in.Op == ir.OLoads {
			mn = "movss"
		}
		e.line("%s %s, %s", mn, mem, xmmn(d))
		commit()
		return
	}
	d, commit := e.gpDst(in.To)
	w := in.Cls == ir.ClsL
	dw := 4
	tail := "l"
	if w {
		dw = 8
		tail = "q"
	}
	switch in.Op {
	case ir.OLoadub:
		e.line("movzb%s %s, %s", tail, mem, gpn(d, dw))
	case ir.OLoadsb:
		e.line("movsb%s %s, %s", tail, mem, gpn(d, dw))
	case ir.OLoaduh:
		e.line("movzw%s %s, %s", tail, mem, gpn(d, dw))
	case ir.OLoadsh:
		e.line("movsw%s %s, %s", tail, mem, gpn(d, dw))
	case ir.OLoaduw:
		e.line("movl %s, %s", mem, gpn(d, 4)) // 32-bit load zero-extends
	case ir.OLoadsw:
		e.line("movslq %s, %s", mem, gpn(d, 8))
	case ir.OLoadl:
		e.line("movq %s, %s", mem, gpn(d, 8))
	}
	commit()
}

func (e *emitter) store(in *ir.Instr) {
	if in.Op == ir.OStores || in.Op == ir.OStored {
		v := e.fpValue(in.Arg(0), fpScratch0)
		mem := e.memAddr(in.Arg(1), gpScratch1)
		mn := "movsd"
		if in.Op == ir.OStores {
			mn = "movss"
		}
		e.line("%s %s, %s", mn, xmmn(v), mem)
		return
	}
	v := e.gpValue(in.Arg(0), gpScratch0)
	mem := e.memAddr(in.Arg(1), gpScratch1)
	size := map[ir.Op]int{ir.OStoreb: 1, ir.OStoreh: 2, ir.OStorew: 4, ir.OStorel: 8}[in.Op]
	e.line("mov%s %s, %s", suf(size), gpn(v, size), mem)
}

func (e *emitter) extend(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	dw := 4
	tail := "l"
	if w {
		dw = 8
		tail = "q"
	}
	rs := e.gpValue(in.Arg(0), gpScratch1)
	d, commit := e.gpDst(in.To)
	switch in.Op {
	case ir.OExtsb:
		e.line("movsb%s %s, %s", tail, gpn(rs, 1), gpn(d, dw))
	case ir.OExtub:
		e.line("movzb%s %s, %s", tail, gpn(rs, 1), gpn(d, dw))
	case ir.OExtsh:
		e.line("movsw%s %s, %s", tail, gpn(rs, 2), gpn(d, dw))
	case ir.OExtuh:
		e.line("movzw%s %s, %s", tail, gpn(rs, 2), gpn(d, dw))
	case ir.OExtsw:
		e.line("movslq %s, %s", gpn(rs, 4), gpn(d, 8))
	case ir.OExtuw:
		e.line("movl %s, %s", gpn(rs, 4), gpn(d, 4))
	}
	commit()
}

func (e *emitter) convert(in *ir.Instr) {
	switch in.Op {
	case ir.OExts:
		rs := e.fpValue(in.Arg(0), fpScratch1)
		d, commit := e.fpDst(in.To)
		e.line("cvtss2sd %s, %s", xmmn(rs), xmmn(d))
		commit()
	case ir.OTruncd:
		rs := e.fpValue(in.Arg(0), fpScratch1)
		d, commit := e.fpDst(in.To)
		e.line("cvtsd2ss %s, %s", xmmn(rs), xmmn(d))
		commit()
	case ir.OStosi, ir.OStoui:
		srcD := e.f.ClassOf(in.Arg(0)) == ir.ClsD
		mn := "cvttsd2si"
		if !srcD {
			mn = "cvttss2si"
		}
		rs := e.fpValue(in.Arg(0), fpScratch1)
		d, commit := e.gpDst(in.To)
		e.line("%s %s, %s", mn, xmmn(rs), gpn(d, clsSize(in.Cls)))
		commit()
	case ir.OSltof, ir.OUltof:
		dstD := in.Cls == ir.ClsD
		w := e.f.ClassOf(in.Arg(0)) == ir.ClsL
		mn := "cvtsi2sd"
		if !dstD {
			mn = "cvtsi2ss"
		}
		if w {
			mn += "q"
		} else {
			mn += "l"
		}
		rs := e.gpValue(in.Arg(0), gpScratch1)
		d, commit := e.fpDst(in.To)
		e.line("%s %s, %s", mn, gpn(rs, clsSize(e.f.ClassOf(in.Arg(0)))), xmmn(d))
		commit()
	case ir.OCast:
		if in.Cls.IsFloat() {
			rs := e.gpValue(in.Arg(0), gpScratch1)
			d, commit := e.fpDst(in.To)
			if in.Cls == ir.ClsD {
				e.line("movq %s, %s", gpn(rs, 8), xmmn(d))
			} else {
				e.line("movd %s, %s", gpn(rs, 4), xmmn(d))
			}
			commit()
		} else {
			rs := e.fpValue(in.Arg(0), fpScratch1)
			d, commit := e.gpDst(in.To)
			if in.Cls == ir.ClsL {
				e.line("movq %s, %s", xmmn(rs), gpn(d, 8))
			} else {
				e.line("movd %s, %s", xmmn(rs), gpn(d, 4))
			}
			commit()
		}
	}
}

// --- variadics -------------------------------------------------------------

func (e *emitter) vaStart(in *ir.Instr) {
	e.gpInto(gpScratch0, in.Arg(0)) // r10 = &va_list
	ap := gpScratch0
	ngp, nfp, stackBytes := namedCounts(e.f)
	e.line("movl $%d, %s", 8*ngp, memn(ap, 0))
	e.line("movl $%d, %s", vaGPBytes+16*nfp, memn(ap, 4))
	e.line("leaq %s, %s", memn(RBP, int32(16+stackBytes)), gpn(gpScratch1, 8))
	e.line("movq %s, %s", gpn(gpScratch1, 8), memn(ap, 8))
	e.line("leaq %s, %s", memn(RBP, int32(-e.regSaveDist)), gpn(gpScratch1, 8))
	e.line("movq %s, %s", gpn(gpScratch1, 8), memn(ap, 16))
}

func (e *emitter) vaArg(in *ir.Instr) {
	float := in.Cls.IsFloat()
	offField, bound, step := int32(0), vaGPBytes, 8
	if float {
		offField, bound, step = 4, vaRegSaveSz, 16
	}
	e.gpInto(gpScratch0, in.Arg(0)) // r10 = &va_list
	ap := gpScratch0
	e.vaSeq++
	over := fmt.Sprintf(".L%s_va%d_over", e.fname, e.vaSeq)
	join := fmt.Sprintf(".L%s_va%d_join", e.fname, e.vaSeq)

	e.line("movl %s, %s", memn(ap, offField), gpn(gpScratch1, 4))
	e.line("cmpl $%d, %s", bound, gpn(gpScratch1, 4))
	e.line("jae %s", over)
	// register save area
	e.line("movq %s, %s", memn(ap, 16), gpn(RDX, 8))
	e.line("addq %s, %s", gpn(gpScratch1, 8), gpn(RDX, 8))
	e.line("addl $%d, %s", step, gpn(gpScratch1, 4))
	e.line("movl %s, %s", gpn(gpScratch1, 4), memn(ap, offField))
	e.line("jmp %s", join)
	// overflow (stack) area
	fmt.Fprintf(e.sb, "%s:\n", over)
	e.line("movq %s, %s", memn(ap, 8), gpn(RDX, 8))
	e.line("leaq %s, %s", memn(RDX, 8), gpn(gpScratch1, 8))
	e.line("movq %s, %s", gpn(gpScratch1, 8), memn(ap, 8))
	fmt.Fprintf(e.sb, "%s:\n", join)

	if float {
		d, commit := e.fpDst(in.To)
		mn := "movsd"
		if in.Cls != ir.ClsD {
			mn = "movss"
		}
		e.line("%s %s, %s", mn, memn(RDX, 0), xmmn(d))
		commit()
		return
	}
	d, commit := e.gpDst(in.To)
	sz := clsSize(in.Cls)
	e.line("mov%s %s, %s", suf(sz), memn(RDX, 0), gpn(d, sz))
	commit()
}
