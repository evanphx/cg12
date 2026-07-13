package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Compile lowers, allocates registers for, and emits GNU-assembler text for a
// single function. It mutates f in place (SSA destruction and ABI lowering), so
// pass a function you no longer need in SSA form.
// ptrCls is arm64's pointer class: pointers are 64-bit and live in x-registers,
// so the pointer width is identical to the general-register width.
const ptrCls = ir.ClsL

func Compile(f *ir.Func) (string, error) {
	ir.LowerPointers(f, ptrCls)
	if err := lower(f); err != nil {
		return "", err
	}
	alloc, err := regAlloc(f)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := emitFunc(f, alloc, &sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// CompileModule emits assembly for every function and data definition in m. If
// the module carries source positions, a DWARF file table is declared up front
// with .file directives; the per-instruction .loc directives then reference it.
func CompileModule(m *ir.Module) (string, error) {
	var sb strings.Builder
	for i, name := range m.Files {
		fmt.Fprintf(&sb, "\t.file %d %q\n", i+1, name)
	}
	for _, f := range m.Funcs {
		s, err := Compile(f)
		if err != nil {
			return "", fmt.Errorf("function %s: %w", f.Name, err)
		}
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	for _, d := range m.Data {
		emitData(&sb, d)
	}
	return sb.String(), nil
}

func emitData(sb *strings.Builder, d *ir.Data) {
	fmt.Fprintf(sb, "\t.data\n")
	if d.Align > 0 {
		fmt.Fprintf(sb, "\t.balign %d\n", d.Align)
	}
	if d.Linkage.Export {
		fmt.Fprintf(sb, "\t.globl %s\n", sanitize(d.Name))
	}
	fmt.Fprintf(sb, "%s:\n", sanitize(d.Name))
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			fmt.Fprintf(sb, "\t.zero %d\n", it.Zero)
		case it.Str != "":
			fmt.Fprintf(sb, "\t.ascii \"%s\"\n", asciiEscape(it.Str))
		case it.Sym != "":
			fmt.Fprintf(sb, "\t.quad %s+%d\n", sanitize(it.Sym), it.Off)
		default:
			directive := dataDirective(it.Sub)
			for _, v := range it.Ints {
				fmt.Fprintf(sb, "\t%s %d\n", directive, v)
			}
		}
	}
}

// asciiEscape renders raw bytes as the body of a GNU-as .ascii string: printable
// ASCII passes through, the special characters are C-escaped, and every other
// byte becomes a fixed three-digit octal escape (so binary data — a NUL
// terminator, a quad constant's bytes — survives assembly intact).
func asciiEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if c >= 0x20 && c < 0x7f {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, `\%03o`, c)
			}
		}
	}
	return b.String()
}

func dataDirective(s ir.SubCls) string {
	switch s.Size() {
	case 1:
		return ".byte"
	case 2:
		return ".hword"
	case 4:
		return ".word"
	default:
		return ".quad"
	}
}
