package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Inline assembly: binding a template's operands to registers. The template
// itself is passed through to an assembler, so this is only about deciding what
// %0, %1 ... stand for.

// asmPrecolor rejects inline-asm fixed-register constraints, which are the
// x86-specific operand letters ("a", "d", ...); AArch64 asm does not use them.
// It runs before allocation, mirroring the amd64 pass that honours them.
func asmPrecolor(f *ir.Func) error {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OAsm {
				continue
			}
			for _, letter := range in.Asm.Regs {
				if letter != "" {
					return fmt.Errorf("arm64: inline-asm fixed-register constraint %q is not supported", letter)
				}
			}
		}
	}
	return nil
}

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
