package arm64

import (
	"fmt"
	"strconv"
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
			// A clobber list is a promise the allocator keeps. Honouring the names we
			// recognise and passing over the rest would keep exactly the promises
			// that needed no keeping, and break the one that did.
			for _, name := range in.Asm.Clobbers {
				if _, ok := asmClobberReg(name); !ok {
					return fmt.Errorf("arm64: inline-asm clobbers %q, which is not a register this backend knows", name)
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

// asmClobberRegs is the registers an inline-asm template declares it writes.
//
// "memory" and "cc" name no register: the memory clobber is already implied,
// since an OAsm is an unknown memory effect to every pass that could reason
// across it, and cg12 does not allocate the condition flags. Anything else must
// be a register we know, because the clobber list is a promise the allocator
// keeps -- honouring the ones we recognise and ignoring the rest would keep
// exactly the promises that need no keeping.
func asmClobberRegs(in *ir.Instr) []Reg {
	if in.Asm == nil {
		return nil
	}
	var out []Reg
	for _, name := range in.Asm.Clobbers {
		if r, ok := asmClobberReg(name); ok && r >= 0 {
			out = append(out, r)
		}
	}
	return out
}

// asmClobberReg resolves one clobber name. It reports ok for a name this backend
// understands, and a negative register for one that names no register at all:
// "memory" is already implied, since an OAsm is an unknown memory effect to
// every pass that could reason across it, and cg12 does not allocate the
// condition flags that "cc" names.
func asmClobberReg(name string) (Reg, bool) {
	switch name {
	case "memory", "cc":
		return -1, true
	}
	return regByName(name)
}

// regByName maps an AArch64 register's assembly name to its id. A w name and an
// x name are the same register through different windows, and clobbering either
// clobbers both.
func regByName(name string) (Reg, bool) {
	if len(name) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(name[1:])
	if err != nil {
		return 0, false
	}
	switch name[0] {
	case 'x', 'w':
		if n <= 30 {
			return Reg(n), true
		}
	case 'd', 's', 'q', 'v':
		if n <= 31 {
			return vReg(n), true
		}
	}
	return 0, false
}
