package ir

import (
	"fmt"
	"strconv"
	"strings"
)

// String renders the module as QBE-compatible textual IL.
func (m *Module) String() string {
	var sb strings.Builder
	m.Print(&sb)
	return sb.String()
}

// Print writes the module as textual IL to sb.
func (m *Module) Print(sb *strings.Builder) {
	for i, t := range m.Types {
		if i > 0 {
			sb.WriteByte('\n')
		}
		printType(sb, t)
	}
	for _, d := range m.Data {
		printData(sb, d)
	}
	for i, f := range m.Funcs {
		if i > 0 || len(m.Types) > 0 || len(m.Data) > 0 {
			sb.WriteByte('\n')
		}
		f.Print(sb)
	}
}

func printLinkage(sb *strings.Builder, l Linkage) {
	if l.Export {
		sb.WriteString("export ")
	}
	if l.Thread {
		sb.WriteString("thread ")
	}
	if l.Section != "" {
		sb.WriteString("section \"")
		sb.WriteString(l.Section)
		sb.WriteString("\" ")
	}
}

func printType(sb *strings.Builder, t *AggType) {
	fmt.Fprintf(sb, "type :%s = ", t.Name)
	if t.Align > 0 {
		fmt.Fprintf(sb, "align %d ", t.Align)
	}
	if t.Opaque {
		fmt.Fprintf(sb, "{ %d }\n", t.Size)
		return
	}
	if t.Union {
		sb.WriteString("{ ")
		for _, c := range t.Cases {
			sb.WriteString("{ ")
			printFields(sb, c)
			sb.WriteString(" } ")
		}
		sb.WriteString("}\n")
		return
	}
	sb.WriteString("{ ")
	printFields(sb, t.Fields)
	sb.WriteString(" }\n")
}

func printFields(sb *strings.Builder, fields []Field) {
	for i, f := range fields {
		if i > 0 {
			sb.WriteString(", ")
		}
		if f.Type != nil {
			fmt.Fprintf(sb, ":%s", f.Type.Name)
		} else {
			sb.WriteString(f.Sub.ElemString())
		}
		if f.Count > 1 {
			fmt.Fprintf(sb, " %d", f.Count)
		}
	}
}

func printData(sb *strings.Builder, d *Data) {
	printLinkage(sb, d.Linkage)
	fmt.Fprintf(sb, "data $%s = ", d.Name)
	if d.Align > 0 {
		fmt.Fprintf(sb, "align %d ", d.Align)
	}
	sb.WriteString("{ ")
	for i, it := range d.Items {
		if i > 0 {
			sb.WriteString(", ")
		}
		switch {
		case it.Zero > 0:
			fmt.Fprintf(sb, "z %d", it.Zero)
		case it.Str != "":
			fmt.Fprintf(sb, "b \"%s\"", it.Str)
		case it.Sym != "":
			fmt.Fprintf(sb, "%s $%s", it.Sub.ElemString(), it.Sym)
			if it.Off != 0 {
				fmt.Fprintf(sb, "+%d", it.Off)
			}
		default:
			sb.WriteString(it.Sub.ElemString())
			for _, v := range it.Ints {
				fmt.Fprintf(sb, " %d", v)
			}
			for _, v := range it.Flts {
				fmt.Fprintf(sb, " %s", strconv.FormatFloat(v, 'g', -1, 64))
			}
		}
	}
	sb.WriteString(" }\n")
}

// String renders a single function as textual IL.
func (f *Func) String() string {
	var sb strings.Builder
	f.Print(&sb)
	return sb.String()
}

// Print writes the function as textual IL to sb.
func (f *Func) Print(sb *strings.Builder) {
	printLinkage(sb, f.Linkage)
	sb.WriteString("function ")
	if f.HasRet {
		if f.RetAgg != nil {
			fmt.Fprintf(sb, ":%s ", f.RetAgg.Name)
		} else {
			fmt.Fprintf(sb, "%s ", f.Retty)
		}
	}
	fmt.Fprintf(sb, "$%s(", f.Name)
	for i, p := range f.Params {
		if i > 0 {
			sb.WriteString(", ")
		}
		if p.Agg != nil {
			fmt.Fprintf(sb, ":%s %%%s", p.Agg.Name, p.Name)
		} else {
			fmt.Fprintf(sb, "%s %%%s", p.Cls, p.Name)
		}
	}
	if f.Variadic {
		if len(f.Params) > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("...")
	}
	sb.WriteString(") {\n")
	for _, b := range f.Blocks {
		f.printBlock(sb, b)
	}
	sb.WriteString("}\n")
}

func (f *Func) printBlock(sb *strings.Builder, b *Block) {
	fmt.Fprintf(sb, "@%s\n", b.Name)
	last := f.printPos(sb, b.Pos, SrcPos{})
	for _, p := range b.Phis {
		fmt.Fprintf(sb, "\t%s =%s phi ", f.refString(p.To), p.Cls)
		for i := range p.Args {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(sb, "@%s %s", p.Blocks[i].Name, f.refString(p.Args[i]))
		}
		sb.WriteByte('\n')
	}
	for i := range b.Instrs {
		last = f.printPos(sb, b.Instrs[i].Pos, last)
		f.printInstr(sb, &b.Instrs[i])
	}
	f.printJmp(sb, b.Jmp)
}

// printPos emits the loc directive needed to move from position last to pos,
// returning the new current position. loc carries the file, line, and column
// together; the file is repeated only when it changes, and the position persists
// until the next loc (QBE style).
func (f *Func) printPos(sb *strings.Builder, pos, last SrcPos) SrcPos {
	if !pos.Valid() || pos == last || f.mod == nil {
		return last
	}
	if pos.File != last.File {
		fmt.Fprintf(sb, "\tloc \"%s\" %d %d\n", f.mod.FileName(pos.File), pos.Line, pos.Col)
	} else {
		fmt.Fprintf(sb, "\tloc %d %d\n", pos.Line, pos.Col)
	}
	return pos
}

func (f *Func) printInstr(sb *strings.Builder, in *Instr) {
	sb.WriteByte('\t')
	if (in.Op.HasResult() || in.Op == OIntrinsic) && !in.To.IsNone() {
		if in.Op == OCall && in.RetAgg != nil {
			fmt.Fprintf(sb, "%s =:%s ", f.refString(in.To), in.RetAgg.Name)
		} else {
			fmt.Fprintf(sb, "%s =%s ", f.refString(in.To), in.Cls)
		}
	}
	if in.Op == OCall && in.Tail {
		sb.WriteString("tailcall")
	} else {
		sb.WriteString(f.mnemonic(in))
	}
	if in.Op == OBlockAddr {
		name := "?"
		if in.Blk != nil {
			name = in.Blk.Name
		}
		fmt.Fprintf(sb, " @%s\n", name)
		return
	}
	if in.Op == OIntrinsic {
		name := "?"
		if in.Intrin != nil {
			name = in.Intrin.Name
		}
		sb.WriteByte(' ')
		sb.WriteString(name)
		// The operands follow, printed by the generic loop below.
	}
	if in.Op == OCall {
		fmt.Fprintf(sb, " %s(", f.refString(in.Args[0]))
		for i, a := range in.Args[1:] {
			if i > 0 {
				sb.WriteString(", ")
			}
			if i < len(in.AggArgs) && in.AggArgs[i] != nil {
				fmt.Fprintf(sb, ":%s %s", in.AggArgs[i].Name, f.refString(a))
			} else {
				fmt.Fprintf(sb, "%s %s", f.ClassOf(a), f.refString(a))
			}
		}
		sb.WriteString(")\n")
		return
	}
	for i, a := range in.Args {
		if i == 0 {
			sb.WriteByte(' ')
		} else {
			sb.WriteString(", ")
		}
		sb.WriteString(f.refString(a))
	}
	sb.WriteByte('\n')
}

// mnemonic returns the printed opcode, computing compare mnemonics from the
// predicate and operand class (e.g. "csltw", "cod").
func (f *Func) mnemonic(in *Instr) string {
	if in.Op != OCmp {
		return in.Op.String()
	}
	argCls := f.ClassOf(in.Args[0])
	return "c" + in.Cmp.String() + argCls.String()
}

func (f *Func) printJmp(sb *strings.Builder, j Jmp) {
	switch j.Kind {
	case JmpRet:
		if j.Arg.IsNone() {
			sb.WriteString("\tret\n")
		} else {
			fmt.Fprintf(sb, "\tret %s\n", f.refString(j.Arg))
		}
	case JmpJmp:
		fmt.Fprintf(sb, "\tjmp @%s\n", j.To.Name)
	case JmpJnz:
		fmt.Fprintf(sb, "\tjnz %s, @%s, @%s\n", f.refString(j.Arg), j.To.Name, j.To2.Name)
	case JmpHlt:
		sb.WriteString("\thlt\n")
	case JmpBr:
		fmt.Fprintf(sb, "\tjmp *%s", f.refString(j.Arg))
		for i, t := range j.Targets {
			if i == 0 {
				sb.WriteString(" [")
			} else {
				sb.WriteString(", ")
			}
			fmt.Fprintf(sb, "@%s", t.Name)
			if i == len(j.Targets)-1 {
				sb.WriteString("]")
			}
		}
		sb.WriteByte('\n')
	case JmpSwitch:
		fmt.Fprintf(sb, "\tswitch %s, @%s [", f.refString(j.Arg), j.To.Name)
		for i, c := range j.Cases {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(sb, "%d: @%s", c.Val, c.Blk.Name)
		}
		sb.WriteString("]\n")
	case JmpTable:
		fmt.Fprintf(sb, "\tjumptable %s [", f.refString(j.Arg))
		for i, t := range j.Targets {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(sb, "@%s", t.Name)
		}
		sb.WriteString("]\n")
	}
}

func (f *Func) refString(r Ref) string {
	switch r.Kind {
	case RefNone:
		return ""
	case RefTemp:
		return "%" + f.Temps[r.ID].Name
	case RefConst:
		return f.constString(f.Consts[r.ID])
	case RefType:
		if int(r.ID) < len(f.mod.Types) {
			return ":" + f.mod.Types[r.ID].Name
		}
		return fmt.Sprintf(":t%d", r.ID)
	case RefSlot:
		return fmt.Sprintf("%%.slot%d", r.ID)
	case RefReg:
		return fmt.Sprintf("reg%d", r.ID)
	}
	return fmt.Sprintf("<ref %d:%d>", r.Kind, r.ID)
}

func (f *Func) constString(c Const) string {
	switch c.Kind {
	case ConstInt:
		return strconv.FormatInt(c.Int, 10)
	case ConstFloat:
		if c.Cls == ClsS {
			return "s_" + strconv.FormatFloat(c.Flt, 'g', -1, 32)
		}
		return "d_" + strconv.FormatFloat(c.Flt, 'g', -1, 64)
	case ConstSym:
		if c.Int != 0 {
			return fmt.Sprintf("$%s+%d", c.Sym, c.Int)
		}
		return "$" + c.Sym
	}
	return "?"
}
