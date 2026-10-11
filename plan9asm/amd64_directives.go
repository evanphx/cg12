package plan9asm

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

func (t *amd64Translator) translateDirective(directive *Directive) error {
	switch directive.Name {
	case "PCALIGN":
		if len(directive.Operands) != 1 || directive.Operands[0].Kind != OperandImmediate {
			return fmt.Errorf("PCALIGN requires one immediate operand")
		}
		alignment, err := t.resolveInteger(directive.Operands[0].Immediate)
		if err != nil {
			return err
		}
		// In a code section GNU as pads .balign with NOPs, as Go does.
		fmt.Fprintf(&t.output, "\t.balign %d\n", alignment)
		return nil
	case "GLOBL":
		return t.translateGlobal(directive)
	case "DATA":
		return nil
	case "FUNCDATA":
		// Go runtime metadata, which cg12 writes itself. Keep the standard
		// no-local-pointers declaration for the stack-map writer.
		if isNoLocalPointersDirective(directive) && t.current != nil {
			t.functions[t.bodyFunction].NoLocalPointers = true
		}
		return nil
	case "PCDATA":
		return nil
	default:
		return fmt.Errorf("unsupported directive %s", directive.Name)
	}
}

// recordData collects one DATA initializer for the GLOBL that emits it.
func (t *amd64Translator) recordData(directive *Directive) error {
	if len(directive.Operands) != 2 || directive.Operands[1].Kind != OperandImmediate {
		return fmt.Errorf("DATA requires an address/width and immediate value")
	}
	symbol, offset, width, err := t.dataAddress(directive.Operands[0].Text)
	if err != nil {
		return err
	}
	name := t.symbol(symbol)
	source := strings.TrimSpace(directive.Operands[1].Immediate)
	if strings.HasPrefix(source, "\"") {
		value, err := strconv.Unquote(source)
		if err != nil {
			return fmt.Errorf("invalid DATA string %q", directive.Operands[1].Text)
		}
		if uint64(len(value)) > width {
			return fmt.Errorf("DATA string length %d does not fit in %d bytes", len(value), width)
		}
		t.data[name] = append(t.data[name], arm64DataValue{offset: offset, width: width, bytes: []byte(value), stringData: true})
		return nil
	}
	if strings.HasSuffix(source, "(SB)") {
		if width != 4 && width != 8 {
			return fmt.Errorf("DATA relocation width %d is unsupported", width)
		}
		operand, ok, err := parseAMD64Memory(source)
		if err != nil || !ok || operand.Base != "SB" || operand.Index != "" {
			return fmt.Errorf("invalid DATA relocation %q", directive.Operands[1].Text)
		}
		operand.Text = source
		_, addend, err := t.symbolOffset(operand)
		if err != nil {
			return err
		}
		target := t.addressSymbolName(operand.Symbol)
		if !operand.Symbol.Static {
			t.references[t.symbol(operand.Symbol)] = true
		}
		t.data[name] = append(t.data[name], arm64DataValue{offset: offset, width: width, relocation: symbolRelocation(target, addend)})
		return nil
	}
	value, err := t.resolveInteger(source)
	if err != nil {
		float, floatErr := parseAMD64Float(source)
		if floatErr != nil {
			return fmt.Errorf("invalid DATA value %q", directive.Operands[1].Text)
		}
		switch width {
		case 4:
			value = int64(math.Float32bits(float32(float)))
		case 8:
			value = int64(math.Float64bits(float))
		default:
			return fmt.Errorf("floating-point DATA width %d is unsupported", width)
		}
	}
	if width != 1 && width != 2 && width != 4 && width != 8 {
		return fmt.Errorf("numeric DATA width %d is unsupported", width)
	}
	bits := uint64(value)
	if width < 8 {
		bits &= uint64(1)<<(width*8) - 1
	}
	t.data[name] = append(t.data[name], arm64DataValue{offset: offset, width: width, value: bits})
	return nil
}

// dataAddress parses DATA's first operand, sym+off(SB)/width.
func (t *amd64Translator) dataAddress(source string) (Symbol, uint64, uint64, error) {
	source = strings.TrimSpace(source)
	slash := strings.LastIndexByte(source, '/')
	if slash < 0 {
		return Symbol{}, 0, 0, fmt.Errorf("DATA address %q has no width", source)
	}
	width, err := t.resolveInteger(source[slash+1:])
	if err != nil || width <= 0 {
		return Symbol{}, 0, 0, fmt.Errorf("invalid DATA width in %q", source)
	}
	operand, ok, err := parseAMD64Memory(strings.TrimSpace(source[:slash]))
	if err != nil || !ok || operand.Base != "SB" || operand.Index != "" {
		return Symbol{}, 0, 0, fmt.Errorf("DATA address %q is not relative to SB", source)
	}
	operand.Text = source
	_, offset, err := t.symbolOffset(operand)
	if err != nil {
		return Symbol{}, 0, 0, err
	}
	if offset < 0 {
		return Symbol{}, 0, 0, fmt.Errorf("negative DATA offset in %q", source)
	}
	return operand.Symbol, uint64(offset), uint64(width), nil
}

// amd64DataAlignment is the alignment Go's linker gives a data symbol with no
// explicit alignment (cmd/link symalign): the largest power of two up to
// amd64's Maxalign of 32 that does not exceed the symbol's size. SSE code
// relies on it for aligned loads (MOVO, PSHUFB m128, ...) from tables.
func amd64DataAlignment(size uint64) uint64 {
	alignment := uint64(32)
	for alignment > size && alignment > 1 {
		alignment >>= 1
	}
	return alignment
}

func (t *amd64Translator) translateGlobal(directive *Directive) error {
	if len(directive.Operands) < 2 || len(directive.Operands) > 3 {
		return fmt.Errorf("GLOBL requires a symbol, optional flags, and size")
	}
	symbolOperand, ok, err := parseAMD64Memory(strings.TrimSpace(directive.Operands[0].Text))
	if err != nil || !ok || symbolOperand.Base != "SB" || symbolOperand.Index != "" {
		return fmt.Errorf("invalid GLOBL symbol %q", directive.Operands[0].Text)
	}
	symbolOperand.Text = directive.Operands[0].Text
	_, offset, err := t.symbolOffset(symbolOperand)
	if err != nil {
		return err
	}
	if offset != 0 {
		return fmt.Errorf("GLOBL symbol %q has nonzero offset %d", directive.Operands[0].Text, offset)
	}
	symbol := symbolOperand.Symbol
	sizeOperand := directive.Operands[len(directive.Operands)-1]
	if sizeOperand.Kind != OperandImmediate {
		return fmt.Errorf("invalid GLOBL size %q", sizeOperand.Text)
	}
	signedSize, err := t.resolveInteger(sizeOperand.Immediate)
	if err != nil || signedSize < 0 {
		return fmt.Errorf("invalid GLOBL size %q", sizeOperand.Immediate)
	}
	size := uint64(signedSize)
	name := t.symbol(symbol)
	values := append([]arm64DataValue(nil), t.data[name]...)
	flags := ""
	if len(directive.Operands) == 3 {
		flags = directive.Operands[1].Text
	}
	section := ".bss"
	if len(values) != 0 {
		section = ".data"
	}
	if hasPlan9Flag(flags, "RODATA") {
		section = ".section .rodata"
	}
	if hasPlan9Flag(flags, "TLSBSS") {
		return fmt.Errorf("GLOBL %s: TLSBSS data is unsupported; use AMD64TLSDefinition", name)
	}
	fmt.Fprintf(&t.output, "\n%s\n", section)
	fmt.Fprintf(&t.output, "\t.balign %d\n", amd64DataAlignment(size))
	switch {
	case symbol.Static:
		fmt.Fprintf(&t.output, "\t.local %s\n", name)
	case hasPlan9Flag(flags, "DUPOK"):
		fmt.Fprintf(&t.output, "\t.weak %s\n", name)
	default:
		fmt.Fprintf(&t.output, "\t.global %s\n", name)
	}
	fmt.Fprintf(&t.output, "\t.type %s, @object\n", name)
	fmt.Fprintf(&t.output, "\t.size %s, %d\n", name, size)
	fmt.Fprintf(&t.output, "%s:\n", name)
	if len(values) == 0 {
		if size != 0 {
			fmt.Fprintf(&t.output, "\t.zero %d\n", size)
		}
		t.output.WriteString("\t.text\n")
		return nil
	}
	sort.SliceStable(values, func(left, right int) bool {
		return values[left].offset < values[right].offset
	})
	written := uint64(0)
	for _, value := range values {
		if value.offset < written || value.offset+value.width > size {
			return fmt.Errorf("DATA range %d/%d is invalid for %s size %d", value.offset, value.width, name, size)
		}
		if value.offset > written {
			fmt.Fprintf(&t.output, "\t.zero %d\n", value.offset-written)
		}
		switch {
		case value.relocation != "":
			directive := map[uint64]string{4: ".long", 8: ".quad"}[value.width]
			fmt.Fprintf(&t.output, "\t%s %s\n", directive, value.relocation)
		case value.stringData:
			if len(value.bytes) != 0 {
				t.output.WriteString("\t.byte ")
				for index, b := range value.bytes {
					if index != 0 {
						t.output.WriteString(", ")
					}
					fmt.Fprintf(&t.output, "0x%02x", b)
				}
				t.output.WriteByte('\n')
			}
			if padding := value.width - uint64(len(value.bytes)); padding != 0 {
				fmt.Fprintf(&t.output, "\t.zero %d\n", padding)
			}
		default:
			directive := map[uint64]string{1: ".byte", 2: ".short", 4: ".long", 8: ".quad"}[value.width]
			fmt.Fprintf(&t.output, "\t%s 0x%x\n", directive, value.value)
		}
		written = value.offset + value.width
	}
	if written < size {
		fmt.Fprintf(&t.output, "\t.zero %d\n", size-written)
	}
	t.output.WriteString("\t.text\n")
	return nil
}
