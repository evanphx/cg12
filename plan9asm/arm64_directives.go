package plan9asm

import (
	"fmt"
	"strconv"
)

func (t *arm64Translator) translateText(text *Text) error {
	t.functionIndex++
	if text.Frame != "" && text.Frame != "0" {
		return fmt.Errorf("TEXT frame $%s is not supported yet", text.Frame)
	}
	symbol := t.symbol(text.Symbol)
	t.functions = append(t.functions, ARM64Function{
		Name:  symbol,
		Frame: 0,
		Flags: append([]string(nil), text.Flags...),
	})
	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", symbol)
	if text.Symbol.Static {
		// The cg12 object carries Go runtime metadata for translated assembly
		// functions, so even a file-local Go symbol must be link-visible from
		// the separately assembled GNU object. Its file-qualified name remains
		// unique, and hidden visibility keeps it out of the public ABI.
		fmt.Fprintf(&t.output, ".hidden %s\n", symbol)
	}
	fmt.Fprintf(&t.output, ".type %s, %%function\n", symbol)
	fmt.Fprintf(&t.output, "%s:\n", symbol)
	return nil
}

func (t *arm64Translator) translateDirective(directive *Directive) error {
	switch directive.Name {
	case "PCALIGN":
		if len(directive.Operands) != 1 || directive.Operands[0].Kind != OperandImmediate {
			return fmt.Errorf("PCALIGN requires one immediate operand")
		}
		fmt.Fprintf(&t.output, "\t.balign %s\n", directive.Operands[0].Immediate)
		return nil
	case "GLOBL":
		return t.translateGlobal(directive)
	default:
		return fmt.Errorf("unsupported directive %s", directive.Name)
	}
}

func (t *arm64Translator) translateGlobal(directive *Directive) error {
	if len(directive.Operands) < 2 || len(directive.Operands) > 3 {
		return fmt.Errorf("GLOBL requires a symbol, optional flags, and size")
	}
	symbol, ok := operandSymbol(directive.Operands[0])
	if !ok {
		return fmt.Errorf("invalid GLOBL symbol %q", directive.Operands[0].Text)
	}
	sizeOperand := directive.Operands[len(directive.Operands)-1]
	if sizeOperand.Kind != OperandImmediate {
		return fmt.Errorf("invalid GLOBL size %q", sizeOperand.Text)
	}
	size, err := strconv.ParseUint(sizeOperand.Immediate, 0, 64)
	if err != nil {
		return fmt.Errorf("invalid GLOBL size %q", sizeOperand.Immediate)
	}
	name := t.symbol(symbol)
	t.output.WriteString("\n.bss\n")
	t.output.WriteString("\t.balign 8\n")
	if symbol.Static {
		fmt.Fprintf(&t.output, "\t.local %s\n", name)
	} else {
		fmt.Fprintf(&t.output, "\t.global %s\n", name)
	}
	fmt.Fprintf(&t.output, "\t.type %s, %%object\n", name)
	fmt.Fprintf(&t.output, "\t.size %s, %d\n", name, size)
	fmt.Fprintf(&t.output, "%s:\n", name)
	fmt.Fprintf(&t.output, "\t.zero %d\n", size)
	return nil
}
