package amd64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// amd64FixedReg maps an inline-asm fixed-register constraint letter to its
// physical register.
func amd64FixedReg(letter string) (Reg, bool) {
	switch letter {
	case "a":
		return RAX, true
	case "b":
		return RBX, true
	case "c":
		return RCX, true
	case "d":
		return RDX, true
	case "S":
		return RSI, true
	case "D":
		return RDI, true
	}
	return 0, false
}

// asmPrecolor pins each inline-asm operand carrying a fixed-register constraint
// to its physical register, so register allocation places it there. A fixed
// register output pre-colors its own (freshly created) result temporary; a fixed
// register input is materialized into a fresh pre-colored temporary by a copy
// inserted before the asm -- robust even when the optimizer has folded the input
// to a constant. It runs before allocation.
func asmPrecolor(f *ir.Func) error {
	pin := func(ref ir.Ref, reg Reg) {
		t := f.Temps[ref.ID]
		t.Fixed = true
		t.Reg = int(reg)
	}
	for _, b := range f.Blocks {
		var out []ir.Instr
		for i := range b.Instrs {
			in := b.Instrs[i]
			if in.Op != ir.OAsm {
				out = append(out, in)
				continue
			}
			outs := in.AsmRegOuts()
			oc, ac := 0, 0
			var pre []ir.Instr // copies materializing fixed inputs, emitted before the asm
			for opi, kind := range in.Asm.Ops {
				letter := ""
				if opi < len(in.Asm.Regs) {
					letter = in.Asm.Regs[opi]
				}
				switch kind {
				case ir.AsmRegOut, ir.AsmRegInOut:
					ref := outs[oc]
					oc++
					if kind == ir.AsmRegInOut {
						ac++ // the preload value is a plain Arg
					}
					if letter != "" {
						reg, ok := amd64FixedReg(letter)
						if !ok {
							return fmt.Errorf("amd64: unsupported inline-asm register constraint %q", letter)
						}
						pin(ref, reg) // the result temporary is dedicated; pin it directly
					}
				default:
					ai := ac
					ac++
					if letter == "" {
						continue
					}
					reg, ok := amd64FixedReg(letter)
					if !ok {
						return fmt.Errorf("amd64: unsupported inline-asm register constraint %q", letter)
					}
					arg := in.Args[ai]
					cls := f.ClassOf(arg)
					nt := f.NewTemp("asmfix", cls)
					pin(nt, reg)
					pre = append(pre, ir.Instr{Op: ir.OCopy, Cls: cls, To: nt, Args: []ir.Ref{arg}})
					in.Args[ai] = nt
				}
			}
			out = append(out, pre...)
			out = append(out, in)
		}
		b.Instrs = out
	}
	return nil
}

// asmVal is a resolved inline-asm operand: an immediate literal, a memory
// reference, or a register (named at its natural width, or forced by a
// %q/%k/%w/%b modifier).
type asmVal struct {
	lit   bool   // imm or mem: substitute litS verbatim, ignoring any width modifier
	litS  string // preformatted immediate ("$5") or memory reference ("(%rax)")
	reg   Reg
	width int
}

// emitAsm passes a GNU inline-assembly template (an OAsm) through to the output
// in AT&T syntax, substituting each %N placeholder with the register the
// allocator bound to operand N (or, for an "i" operand, a literal immediate).
// Operands are numbered output-first: %0 is the single output (when present),
// then the inputs. A register operand already in a register uses it directly; a
// spilled or constant register operand is materialized into a scratch register
// (loaded before, and, for the output, stored back after).
//
// The OAsm clobbers like a call, so any value live across it is already held in
// a callee-saved register and the template may freely use the caller-saved set.
func (e *emitter) emitAsm(in *ir.Instr) {
	asm := in.Asm
	vals := make([]asmVal, len(asm.Ops))
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
		r := next()
		slot := e.slotAddr(t.Slot)
		finals = append(finals, func() { e.line("mov%s %s, %s", suf(w), gpn(r, w), memn(RBP, slot)) })
		return r, w
	}
	for i, kind := range asm.Ops {
		switch kind {
		case ir.AsmRegOut:
			r, w := resolveOut()
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmRegInOut:
			r, w := resolveOut()
			pre, _ := e.asmInputReg(in.Args[ac], next) // preload value
			ac++
			e.line("mov%s %s, %s", suf(w), gpn(pre, w), gpn(r, w)) // preload the read-write register
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmImm:
			vals[i] = asmVal{lit: true, litS: fmt.Sprintf("$%d", e.f.Consts[in.Args[ac].ID].Int)}
			ac++
		case ir.AsmMem:
			r, _ := e.asmInputReg(in.Args[ac], next) // the operand's address
			vals[i] = asmVal{lit: true, litS: memn(r, 0)}
			ac++
		default: // AsmRegIn
			r, w := e.asmInputReg(in.Args[ac], next)
			vals[i] = asmVal{reg: r, width: w}
			ac++
		}
	}

	text, err := expandAsm(asm.Template, vals)
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

// asmInputReg resolves a register input operand to a register (loading a spilled
// temp or materializing a constant into a scratch register) and returns it with
// its natural byte width.
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
// 32-, 16-, and 8-bit register names; %% is a literal percent. An immediate
// operand ignores any width form.
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
		size := 0 // 0 means the operand's natural width
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
		if num >= len(vals) {
			return "", fmt.Errorf("inline asm: operand %%%d is out of range", num)
		}
		v := vals[num]
		if v.lit {
			sb.WriteString(v.litS)
			continue
		}
		if size == 0 {
			size = v.width
		}
		sb.WriteString(gpn(v.reg, size))
	}
	return sb.String(), nil
}
