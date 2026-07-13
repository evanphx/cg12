package amd64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Compile lowers, allocates, and renders a single function to x86-64 assembly
// text (AT&T syntax). It is the readable counterpart to the machine-code path
// and is primarily used for inspection and testing.
func Compile(f *ir.Func) (string, error) {
	ir.LowerPointers(f, ir.ClsL)
	if err := lower(f); err != nil {
		return "", err
	}
	alloc, err := regAlloc(f)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("\t.text\n")
	emitFunc(f, alloc, &sb)
	return sb.String(), nil
}

// CompileModule renders a whole module to assembly text: source-file directives
// (for line debug info), each function's code, then the data section.
func CompileModule(m *ir.Module) (string, error) {
	var sb strings.Builder
	for i, name := range m.Files {
		fmt.Fprintf(&sb, "\t.file %d \"%s\"\n", i+1, name)
	}
	sb.WriteString("\t.text\n")
	for _, f := range m.Funcs {
		ir.LowerPointers(f, ir.ClsL)
		if err := lower(f); err != nil {
			return "", fmt.Errorf("function %s: %w", f.Name, err)
		}
		alloc, err := regAlloc(f)
		if err != nil {
			return "", fmt.Errorf("function %s: %w", f.Name, err)
		}
		emitFunc(f, alloc, &sb)
	}
	for _, d := range m.Data {
		emitData(&sb, d)
	}
	return sb.String(), nil
}

// dataDirective maps a sub-class to its GNU-assembler data directive.
func dataDirective(sub ir.SubCls) string {
	switch sub {
	case ir.SubB, ir.SubUB:
		return ".byte"
	case ir.SubH, ir.SubUH:
		return ".word"
	case ir.SubW:
		return ".long"
	case ir.SubS:
		return ".float"
	case ir.SubD:
		return ".double"
	default:
		return ".quad"
	}
}

func emitData(sb *strings.Builder, d *ir.Data) {
	sb.WriteString("\t.data\n")
	name := sanitize(d.Name)
	if d.Align > 0 {
		fmt.Fprintf(sb, "\t.balign %d\n", d.Align)
	}
	if d.Linkage.Export {
		fmt.Fprintf(sb, "\t.globl %s\n", name)
	}
	fmt.Fprintf(sb, "%s:\n", name)
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			fmt.Fprintf(sb, "\t.zero %d\n", it.Zero)
		case it.Str != "":
			fmt.Fprintf(sb, "\t.ascii \"%s\"\n", it.Str)
		case it.Sym != "":
			if it.Off != 0 {
				fmt.Fprintf(sb, "\t.quad %s+%d\n", sanitize(it.Sym), it.Off)
			} else {
				fmt.Fprintf(sb, "\t.quad %s\n", sanitize(it.Sym))
			}
		case len(it.Flts) > 0:
			for _, v := range it.Flts {
				fmt.Fprintf(sb, "\t%s %v\n", dataDirective(it.Sub), v)
			}
		default:
			for _, v := range it.Ints {
				fmt.Fprintf(sb, "\t%s %d\n", dataDirective(it.Sub), v)
			}
		}
	}
}
