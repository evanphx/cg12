package amd64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Inline assembly: binding a template's operands to registers and writing the
// template out with them substituted in. The result goes to x64.Assemble, so the
// operands are spelled in the AT&T syntax it reads -- which is why the register
// and memory naming lives here rather than with the encoders.

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

// gpNames gives each GP register's name at width [8, 16, 32, 64] bits.
var gpNames = [16][4]string{
	RAX: {"al", "ax", "eax", "rax"},
	RCX: {"cl", "cx", "ecx", "rcx"},
	RDX: {"dl", "dx", "edx", "rdx"},
	RBX: {"bl", "bx", "ebx", "rbx"},
	RSP: {"spl", "sp", "esp", "rsp"},
	RBP: {"bpl", "bp", "ebp", "rbp"},
	RSI: {"sil", "si", "esi", "rsi"},
	RDI: {"dil", "di", "edi", "rdi"},
	R8:  {"r8b", "r8w", "r8d", "r8"},
	R9:  {"r9b", "r9w", "r9d", "r9"},
	R10: {"r10b", "r10w", "r10d", "r10"},
	R11: {"r11b", "r11w", "r11d", "r11"},
	R12: {"r12b", "r12w", "r12d", "r12"},
	R13: {"r13b", "r13w", "r13d", "r13"},
	R14: {"r14b", "r14w", "r14d", "r14"},
	R15: {"r15b", "r15w", "r15d", "r15"},
}

func sizeIdx(size int) int {
	switch size {
	case 1:
		return 0
	case 2:
		return 1
	case 4:
		return 2
	default:
		return 3
	}
}

// suf returns the AT&T operand-size suffix for a byte width.

func gpn(r Reg, size int) string { return "%" + gpNames[r][sizeIdx(size)] }

func memn(base Reg, disp int32) string {
	if disp == 0 {
		return "(" + gpn(base, 8) + ")"
	}
	return fmt.Sprintf("%d(%s)", disp, gpn(base, 8))
}
