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
