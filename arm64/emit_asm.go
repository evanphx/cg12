package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

func (e *emitter) emitAsm(in *ir.Instr) {
	asm := in.Asm
	vals := make([]asmVal, len(asm.Ops))
	scratchN := 0
	nextScratch := func() Reg {
		if scratchN >= len(intScratchRegs) {
			e.fail("arm64: inline asm needs more scratch registers than are available")
			return scratch0
		}
		r := intScratchRegs[scratchN]
		scratchN++
		return r
	}

	// Walk the operands in %N order, drawing register outputs from To/Defs and
	// every other operand's value from Args in order.
	outs := in.AsmRegOuts()
	oc, ac := 0, 0 // cursors into outs and in.Args
	var finals []func()
	resolveOut := func() (Reg, int) {
		oref := outs[oc]
		oc++
		t := e.f.Temps[oref.ID]
		w := e.f.ClassOf(oref).Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := nextScratch()
		slot := e.spillBase + t.Slot
		finals = append(finals, func() { e.line("str %s, [x29, #%d]", r.Name(w), slot) })
		return r, w
	}
	for i, kind := range asm.Ops {
		switch kind {
		case ir.AsmRegOut:
			r, w := resolveOut()
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmRegInOut:
			r, w := resolveOut()
			pre, _ := e.asmInputReg(in.Args[ac], nextScratch) // preload value
			ac++
			e.line("mov %s, %s", r.Name(w), pre.Name(w)) // preload the read-write register
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmImm:
			vals[i] = asmVal{lit: true, litS: fmt.Sprintf("#%d", e.f.Consts[in.Args[ac].ID].Int)}
			ac++
		case ir.AsmMem:
			r, _ := e.asmInputReg(in.Args[ac], nextScratch) // the operand's address
			vals[i] = asmVal{lit: true, litS: fmt.Sprintf("[%s]", r.xName())}
			ac++
		default: // AsmRegIn
			r, w := e.asmInputReg(in.Args[ac], nextScratch)
			vals[i] = asmVal{reg: r, width: w}
			ac++
		}
	}

	text, err := expandAsm(asm.Template, vals)
	if err != nil {
		e.fail("arm64: %v", err)
		return
	}
	for _, ln := range strings.Split(text, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			e.line("%s", ln)
		}
	}
	for _, f := range finals {
		f()
	}
}

// asmInputReg resolves a register input operand to a register (loading a spilled
// temp or materializing a constant into a scratch register) and returns it with
// its natural byte width.
func (e *emitter) asmInputReg(ref ir.Ref, nextScratch func() Reg) (Reg, int) {
	switch ref.Kind {
	case ir.RefTemp:
		t := e.f.Temps[ref.ID]
		w := e.f.ClassOf(ref).Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := nextScratch()
		e.line("ldr %s, [x29, #%d]", r.Name(w), e.spillBase+t.Slot)
		return r, w
	case ir.RefConst:
		c := e.f.Consts[ref.ID]
		r := nextScratch()
		switch c.Kind {
		case ir.ConstInt:
			e.movImm(r, c.Int, 8)
		case ir.ConstSym:
			e.materializeSym(r, c)
		default:
			e.fail("arm64: unsupported inline-asm constant operand")
		}
		return r, 8
	}
	e.fail("arm64: unsupported inline-asm operand %v", ref)
	return scratch0, 8
}
