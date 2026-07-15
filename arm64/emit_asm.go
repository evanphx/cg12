package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// emitAsm passes a GNU inline-assembly template (an OAsm) through to the output,
// substituting each %N placeholder with the register the allocator bound to
// operand N. Operands are numbered output-first: %0 is the single output (when
// present), then the inputs. An operand already in a register uses it directly;
// a spilled or constant operand is materialized into a scratch register (loaded
// before, and, for the output, stored back after).
//
// The OAsm clobbers like a call, so any value live across it is already held in
// a callee-saved register and the template may freely use the caller-saved set.
func (e *emitter) emitAsm(in *ir.Instr) {
	asm := in.Asm
	nOp := asm.NumOut + len(in.Args)
	regs := make([]Reg, nOp)
	widths := make([]int, nOp)
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

	// The single output operand is %0.
	idx := 0
	var finals []func()
	if asm.NumOut == 1 {
		t := e.f.Temps[in.To.ID]
		w := in.Cls.Size()
		widths[0] = w
		if t.Reg != ir.NoReg {
			regs[0] = Reg(t.Reg)
		} else {
			r := nextScratch()
			regs[0] = r
			slot := e.spillBase + t.Slot
			finals = append(finals, func() { e.line("str %s, [x29, #%d]", r.Name(w), slot) })
		}
		idx = 1
	}

	// The remaining operands are the inputs.
	for _, a := range in.Args {
		r, w := e.asmInputReg(a, nextScratch)
		regs[idx], widths[idx] = r, w
		idx++
	}

	text, err := expandAsm(asm.Template, regs, widths)
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

// asmInputReg resolves an input operand to a register (loading a spilled temp or
// materializing a constant into a scratch register) and returns it with its
// natural byte width.
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
// names; %% is a literal percent.
func expandAsm(tmpl string, regs []Reg, widths []int) (string, error) {
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
		if num >= len(regs) {
			return "", fmt.Errorf("inline asm: operand %%%d is out of range", num)
		}
		switch mod {
		case 'w':
			sb.WriteString(regs[num].wName())
		case 'x':
			sb.WriteString(regs[num].xName())
		default:
			sb.WriteString(regs[num].Name(widths[num]))
		}
	}
	return sb.String(), nil
}
