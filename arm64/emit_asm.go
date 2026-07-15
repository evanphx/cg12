package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// asmVal is a resolved inline-asm operand: either an immediate literal or a
// register (named at its natural width, or forced by a %w/%x modifier).
type asmVal struct {
	imm   bool
	immS  string // preformatted immediate, e.g. "#5"
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
	vals := make([]asmVal, asm.NumOut+len(in.Args))
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

	// The output operands are %0, %1, ... -- the To (first) then the Defs.
	var finals []func()
	for oi, oref := range in.AsmOutputs() {
		t := e.f.Temps[oref.ID]
		w := e.f.ClassOf(oref).Size()
		if t.Reg != ir.NoReg {
			vals[oi] = asmVal{reg: Reg(t.Reg), width: w}
		} else {
			r := nextScratch()
			vals[oi] = asmVal{reg: r, width: w}
			slot := e.spillBase + t.Slot
			finals = append(finals, func() { e.line("str %s, [x29, #%d]", r.Name(w), slot) })
		}
	}

	// The remaining operands are the inputs.
	idx := asm.NumOut
	for k, a := range in.Args {
		if asm.InputImm(k) {
			vals[idx] = asmVal{imm: true, immS: fmt.Sprintf("#%d", e.f.Consts[a.ID].Int)}
		} else {
			r, w := e.asmInputReg(a, nextScratch)
			vals[idx] = asmVal{reg: r, width: w}
		}
		idx++
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
		case v.imm:
			sb.WriteString(v.immS)
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
