package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// computeNamedCounts replays argument assignment over a variadic function's named
// parameters, returning how many GP/SIMD registers and stack bytes they used.
func computeNamedCounts(f *ir.Func) (ngrn, nsrn, stack int) {
	var a argAssigner
	for _, p := range f.Params {
		if p.Agg != nil {
			cls := classifyAgg(p.Agg)
			switch cls.kind {
			case aggGP:
				a.assignGP(cls.nregs, cls.size)
			case aggHFA:
				a.assignHFA(cls.nregs, cls.size)
			default:
				a.assign(ir.ClsL)
			}
			continue
		}
		a.assign(p.Cls)
	}
	return a.ngrn, a.nsrn, a.nsaa
}

// vaPtr returns the register holding a va_list pointer operand, loading a
// spilled pointer into scratch2 (kept clear of the x16/x17 scratch that the
// vaarg/vastart sequences use for scratch values).
func (e *emitter) vaPtr(ref ir.Ref) string {
	t := e.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg).xName()
	}
	e.line("ldr %s, [x29, #%d]", scratch2.xName(), e.spillBase+t.Slot)
	return scratch2.xName()
}

// emitVaStart initialises the AAPCS64 va_list at the given pointer: the two
// register-save-area tops, the overflow stack pointer, and the two register
// offsets (negative, indexing from the tops up to zero).
func (e *emitter) emitVaStart(in *ir.Instr) {
	vp := e.vaPtr(in.Args[0])
	s := scratch1.xName()
	w := scratch1.wName()

	e.frameAddr(s, e.frame+roundUp(e.namedStack, 8))
	e.line("str %s, [%s, #0]", s, vp) // __stack
	e.frameAddr(s, e.gpSaveOff+8*8)
	e.line("str %s, [%s, #8]", s, vp) // __gr_top
	e.frameAddr(s, e.fpSaveOff+8*16)
	e.line("str %s, [%s, #16]", s, vp) // __vr_top
	e.movImm(scratch1, int64(-(8-e.namedGr)*8), 4)
	e.line("str %s, [%s, #24]", w, vp) // __gr_offs
	e.movImm(scratch1, int64(-(8-e.namedSr)*16), 4)
	e.line("str %s, [%s, #28]", w, vp) // __vr_offs
}

// emitVaArg fetches the next variadic argument. It is branch-free-free (uses a
// small diamond): if the relevant register offset is still negative the value
// comes from the save area, otherwise from the overflow stack; either way the
// offset/pointer is advanced.
func (e *emitter) emitVaArg(in *ir.Instr) {
	vp := e.vaPtr(in.Args[0])
	float := in.Cls.IsFloat()
	offsField, topField, stride := 24, 8, 8
	if float {
		offsField, topField, stride = 28, 16, 16
	}

	e.vaSeq++
	lStack := fmt.Sprintf(".L%s_va_stack_%d", e.label, e.vaSeq)
	lDone := fmt.Sprintf(".L%s_va_done_%d", e.label, e.vaSeq)

	offW := scratch1.wName() // __gr_offs / __vr_offs
	addr := scratch0.xName() // computed address

	e.line("ldr %s, [%s, #%d]", offW, vp, offsField)
	e.line("cmp %s, #0", offW)
	e.line("b.ge %s", lStack)
	// register save area: addr = top + sext(offs); advance the offset
	e.line("ldr %s, [%s, #%d]", addr, vp, topField)
	e.line("add %s, %s, %s, sxtw", addr, addr, offW)
	e.line("add %s, %s, #%d", offW, offW, stride)
	e.line("str %s, [%s, #%d]", offW, vp, offsField)
	e.line("b %s", lDone)
	// overflow stack: addr = __stack; advance __stack by one 8-byte slot (stacked
	// arguments always occupy 8 bytes, unlike the 16-byte SIMD save-area stride).
	fmt.Fprintf(e.sb, "%s:\n", lStack)
	e.line("ldr %s, [%s, #0]", addr, vp)
	e.line("add %s, %s, #8", scratch1.xName(), addr)
	e.line("str %s, [%s, #0]", scratch1.xName(), vp)
	fmt.Fprintf(e.sb, "%s:\n", lDone)

	d, done := e.dstReg(in.To, in.Cls.Size())
	e.line("ldr %s, [%s]", d, addr)
	done()
}
