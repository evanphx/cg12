package amd64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// emitAsm passes a GNU inline-assembly template (an OAsm) through to the output
// in AT&T syntax, substituting each %N placeholder with the register the
// allocator bound to operand N. Operands are numbered output-first: %0 is the
// single output (when present), then the inputs. An operand already in a
// register uses it directly; a spilled or constant operand is materialized into
// a scratch register (loaded before, and, for the output, stored back after).
//
// The OAsm clobbers like a call, so any value live across it is already held in
// a callee-saved register and the template may freely use the caller-saved set.
func (e *emitter) emitAsm(in *ir.Instr) {
	asm := in.Asm
	nOp := asm.NumOut + len(in.Args)
	regs := make([]Reg, nOp)
	widths := make([]int, nOp)
	scratch := [...]Reg{gpScratch0, gpScratch1}
	scratchN := 0
	next := func() Reg {
		if scratchN >= len(scratch) {
			panic("amd64: inline asm needs more scratch registers than are available")
		}
		r := scratch[scratchN]
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
			r := next()
			regs[0] = r
			slot := e.slotAddr(t.Slot)
			finals = append(finals, func() { e.line("mov%s %s, %s", suf(w), gpn(r, w), memn(RBP, slot)) })
		}
		idx = 1
	}

	// The remaining operands are the inputs.
	for _, a := range in.Args {
		r, w := e.asmInputReg(a, next)
		regs[idx], widths[idx] = r, w
		idx++
	}

	text, err := expandAsm(asm.Template, regs, widths)
	if err != nil {
		panic(fmt.Sprintf("amd64: %v", err))
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
func (e *emitter) asmInputReg(ref ir.Ref, next func() Reg) (Reg, int) {
	switch ref.Kind {
	case ir.RefTemp:
		t := e.f.Temps[ref.ID]
		w := e.f.ClassOf(ref).Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := next()
		e.line("mov%s %s, %s", suf(w), memn(RBP, e.slotAddr(t.Slot)), gpn(r, w))
		return r, w
	case ir.RefConst:
		c := e.f.Consts[ref.ID]
		r := next()
		switch c.Kind {
		case ir.ConstInt:
			e.movImm(r, c.Int, true)
		case ir.ConstSym:
			e.materializeSym(r, c.Sym, c.Int, c.Thread)
		default:
			panic("amd64: unsupported inline-asm constant operand")
		}
		return r, 8
	}
	panic(fmt.Sprintf("amd64: unsupported inline-asm operand %v", ref))
}

// expandAsm substitutes the operand placeholders in a template. A %N names
// operand N at its natural width; the %q, %k, %w, and %b forms force the 64-,
// 32-, 16-, and 8-bit register names; %% is a literal percent.
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
		size := 0 // 0 means operand's natural width
		switch tmpl[i] {
		case 'q':
			size, i = 8, i+1
		case 'k':
			size, i = 4, i+1
		case 'w':
			size, i = 2, i+1
		case 'b':
			size, i = 1, i+1
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
		if size == 0 {
			size = widths[num]
		}
		sb.WriteString(gpn(regs[num], size))
	}
	return sb.String(), nil
}
