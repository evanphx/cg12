package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// asmVal is a resolved inline-asm operand: an immediate literal, a memory
// reference, or a register (named at its natural width, or forced by a %w/%x
// modifier).
type asmVal struct {
	lit   bool   // imm or mem: substitute litS verbatim, ignoring any width modifier
	litS  string // preformatted immediate ("#5") or memory reference ("[x9]")
	reg   Reg
	width int
}

// emitAsm passes a GNU inline-assembly template (an OAsm) through to the output,
// substituting each %N placeholder with the register the allocator bound to
// operand N (or, for an "i" operand, a literal immediate). Operands are numbered
// output-first: %0 is the single output (when present), then the inputs. A
// register operand already in a register uses it directly; a spilled or constant
// register operand is materialized into a scratch register (loaded before, and,
// for the output, stored back after).
//
// The OAsm clobbers like a call, so any value live across it is already held in
// a callee-saved register and the template may freely use the caller-saved set.
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

// expandAsm substitutes the operand placeholders in a template. A %N names
// operand N at its natural width; %wN and %xN force the 32- and 64-bit register
// names; %% is a literal percent. An immediate operand ignores any width form.
func expandAsm(tmpl string, vals []asmVal) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '%' {
			sb.WriteByte(tmpl[i])
			i++
			continue
		}
		i++ // consume '%'
		if i >= len(tmpl) {
			return "", fmt.Errorf("inline asm: template ends with a bare %%")
		}
		if tmpl[i] == '%' {
			sb.WriteByte('%')
			i++
			continue
		}
		var mod byte
		if tmpl[i] == 'w' || tmpl[i] == 'x' {
			mod = tmpl[i]
			i++
		}
		start := i
		for i < len(tmpl) && tmpl[i] >= '0' && tmpl[i] <= '9' {
			i++
		}
		if start == i {
			return "", fmt.Errorf("inline asm: unsupported operand modifier %%%c", tmpl[i])
		}
		num := 0
		for _, d := range tmpl[start:i] {
			num = num*10 + int(d-'0')
		}
		if num >= len(vals) {
			return "", fmt.Errorf("inline asm: operand %%%d is out of range", num)
		}
		v := vals[num]
		switch {
		case v.lit:
			sb.WriteString(v.litS)
		case mod == 'w':
			sb.WriteString(v.reg.wName())
		case mod == 'x':
			sb.WriteString(v.reg.xName())
		default:
			sb.WriteString(v.reg.Name(v.width))
		}
	}
	return sb.String(), nil
}
