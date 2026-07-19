package plan9asm

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (t *arm64Translator) translateText(text *Text) error {
	t.functionIndex++
	localSize, err := parseTextSize("frame", text.Frame)
	if err != nil {
		return err
	}
	argumentSize, err := parseTextSize("arguments", text.Args)
	if err != nil {
		return err
	}
	t.currentABI0 = text.Symbol.ABI == "" && !text.Symbol.Static
	t.currentDirectABI0 = t.currentABI0 && t.options.PreferDirectABI0 && t.directABI0[t.functionIndex]
	t.currentABI0Layout = t.abi0Layouts[t.functionIndex]
	t.currentFrame = 0
	t.currentPlan9Frame = 0
	t.currentLeaf = !t.functionCalls[t.functionIndex]
	if localSize != 0 || t.functionCalls[t.functionIndex] && !textHasFlag(text, "NOFRAME") {
		t.currentFrame = roundUpInteger(localSize+16, 16)
		t.currentPlan9Frame = roundUpInteger(localSize+8, 16)
	}
	symbol := t.symbol(text.Symbol)
	t.currentSymbol = symbol
	if t.currentABI0 && !t.currentDirectABI0 {
		abi0Name, wrapperFrame, err := t.emitABI0Wrapper(text, t.abi0Layouts[t.functionIndex])
		if err != nil {
			return err
		}
		t.functions = append(t.functions, ARM64Function{
			Name:       symbol,
			Frame:      wrapperFrame,
			FrameStart: 4,
			Args:       argumentSize,
			Flags:      append([]string(nil), text.Flags...),
		})
		symbol = abi0Name
	}
	t.functions = append(t.functions, ARM64Function{
		Name:       symbol,
		Frame:      t.assemblyFrameSize(symbol),
		FrameStart: t.assemblyFrameStart(symbol),
		Args:       argumentSize,
		Flags:      append([]string(nil), text.Flags...),
	})
	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", symbol)
	if text.Symbol.Static || t.currentABI0 && !t.currentDirectABI0 {
		// Runtime metadata and ABI wrappers live in the separately emitted cg12
		// object, so implementation symbols must remain link-visible. Hidden
		// visibility keeps file-local and ABI0 entry points out of the public ABI.
		fmt.Fprintf(&t.output, ".hidden %s\n", symbol)
	}
	fmt.Fprintf(&t.output, ".type %s, %%function\n", symbol)
	fmt.Fprintf(&t.output, "%s:\n", symbol)
	if t.currentDirectABI0 {
		alias := abi0Symbol(symbol)
		fmt.Fprintf(&t.output, ".global %s\n", alias)
		fmt.Fprintf(&t.output, ".hidden %s\n", alias)
		fmt.Fprintf(&t.output, "%s:\n", alias)
	}
	if t.currentFrame != 0 {
		if err := t.emitRegisterOffset("sp", "sp", int64(-t.currentFrame)); err != nil {
			return err
		}
		t.output.WriteString("\tstr x30, [sp]\n")
		t.output.WriteString("\tstur x29, [sp, #-8]\n")
		t.output.WriteString("\tsub x29, sp, #8\n")
	}
	return nil
}

func parseTextSize(kind, source string) (int, error) {
	if source == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(source, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid TEXT %s $%s", kind, source)
	}
	return int(parsed), nil
}

func textHasFlag(text *Text, want string) bool {
	for _, flag := range text.Flags {
		if flag == want {
			return true
		}
	}
	return false
}

func (t *arm64Translator) isRuntimeAsyncPreempt(symbol string) bool {
	return symbol == "runtime_asyncPreempt" &&
		t.options.PackagePath == "runtime" &&
		filepath.Base(t.options.Filename) == "preempt_arm64.s"
}

func (t *arm64Translator) assemblyFrameSize(symbol string) int {
	if t.isRuntimeAsyncPreempt(symbol) {
		// asyncPreempt manually subtracts 240 bytes from SP. Its final 256-byte
		// adjustment also removes the 16-byte signal-injected call record and is
		// therefore not part of this function's frame.
		return 240
	}
	return t.currentFrame
}

func (t *arm64Translator) assemblyFrameStart(symbol string) int {
	if t.isRuntimeAsyncPreempt(symbol) {
		// The manual SP subtraction is the function's second instruction.
		return 8
	}
	return frameStart(t.currentFrame)
}

func frameStart(frame int) int {
	if frame == 0 {
		return 0
	}
	return 4
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
	case "DATA":
		return t.translateData(directive)
	case "FUNCDATA":
		// These directives describe Go runtime metadata rather than machine
		// instructions. Preserve the standard no-local-pointers declaration for
		// cg12's stack-map writer; it has no GNU assembler sidecar representation.
		if isNoLocalPointersDirective(directive) && len(t.functions) > 0 {
			t.functions[len(t.functions)-1].NoLocalPointers = true
		}
		return nil
	case "PCDATA":
		// cg12 emits pc-value tables in its object writer.
		return nil
	default:
		return fmt.Errorf("unsupported directive %s", directive.Name)
	}
}

func isNoLocalPointersDirective(directive *Directive) bool {
	if len(directive.Operands) != 2 {
		return false
	}
	index := directive.Operands[0]
	if index.Kind != OperandImmediate || index.Immediate != "1" {
		return false
	}
	symbol, ok := operandSymbol(directive.Operands[1])
	return ok && symbol.Name == "no_pointers_stackmap"
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
	if directive.Operands[0].Kind == OperandMemory {
		resolvedName, offset, err := t.symbolAddress(directive.Operands[0])
		if err != nil {
			return err
		}
		if offset != 0 {
			return fmt.Errorf("GLOBL symbol %q has nonzero offset %d", directive.Operands[0].Text, offset)
		}
		name = resolvedName
	}
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
	fmt.Fprintf(&t.output, "\n%s\n", section)
	t.output.WriteString("\t.balign 8\n")
	if symbol.Static {
		fmt.Fprintf(&t.output, "\t.local %s\n", name)
	} else {
		fmt.Fprintf(&t.output, "\t.global %s\n", name)
	}
	fmt.Fprintf(&t.output, "\t.type %s, %%object\n", name)
	fmt.Fprintf(&t.output, "\t.size %s, %d\n", name, size)
	fmt.Fprintf(&t.output, "%s:\n", name)
	if len(values) == 0 {
		fmt.Fprintf(&t.output, "\t.zero %d\n", size)
		return nil
	}
	sort.Slice(values, func(left, right int) bool {
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
			directive := map[uint64]string{4: ".word", 8: ".quad"}[value.width]
			if directive == "" {
				return fmt.Errorf("DATA relocation width %d is unsupported", value.width)
			}
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
			directive := map[uint64]string{1: ".byte", 2: ".hword", 4: ".word", 8: ".quad"}[value.width]
			fmt.Fprintf(&t.output, "\t%s 0x%x\n", directive, value.value)
		}
		written = value.offset + value.width
	}
	if written < size {
		fmt.Fprintf(&t.output, "\t.zero %d\n", size-written)
	}
	return nil
}

type arm64DataValue struct {
	offset     uint64
	width      uint64
	value      uint64
	bytes      []byte
	relocation string
	stringData bool
}

func (t *arm64Translator) translateData(directive *Directive) error {
	return nil
}

func (t *arm64Translator) recordData(directive *Directive) error {
	if len(directive.Operands) != 2 || directive.Operands[1].Kind != OperandImmediate {
		return fmt.Errorf("DATA requires an address/width and immediate value")
	}
	symbol, offset, width, err := parsePlan9DataAddress(directive.Operands[0].Text)
	if err != nil {
		return err
	}
	source := directive.Operands[1].Immediate
	if strings.HasPrefix(source, "\"") {
		value, err := strconv.Unquote(source)
		if err != nil {
			return fmt.Errorf("invalid DATA string %q", directive.Operands[1].Text)
		}
		if uint64(len(value)) > width {
			return fmt.Errorf("DATA string length %d does not fit in %d bytes", len(value), width)
		}
		name := t.symbol(symbol)
		t.data[name] = append(t.data[name], arm64DataValue{
			offset:     offset,
			width:      width,
			bytes:      []byte(value),
			stringData: true,
		})
		return nil
	}
	if strings.HasSuffix(source, "(SB)") {
		if width != 4 && width != 8 {
			return fmt.Errorf("DATA relocation width %d is unsupported", width)
		}
		target := parseSymbol(strings.TrimSpace(strings.TrimSuffix(source, "(SB)")))
		if target.Name == "" {
			return fmt.Errorf("invalid DATA relocation %q", directive.Operands[1].Text)
		}
		relocation := t.symbol(target)
		if !target.Static {
			t.references[relocation] = true
		}
		name := t.symbol(symbol)
		t.data[name] = append(t.data[name], arm64DataValue{
			offset:     offset,
			width:      width,
			relocation: relocation,
		})
		return nil
	}
	value, err := strconv.ParseUint(source, 0, 64)
	if err != nil {
		return fmt.Errorf("invalid DATA value %q", directive.Operands[1].Text)
	}
	if width != 1 && width != 2 && width != 4 && width != 8 {
		return fmt.Errorf("numeric DATA width %d is unsupported", width)
	}
	if width < 8 && value >= uint64(1)<<(width*8) {
		return fmt.Errorf("DATA value %#x does not fit in %d bytes", value, width)
	}
	name := t.symbol(symbol)
	t.data[name] = append(t.data[name], arm64DataValue{offset: offset, width: width, value: value})
	return nil
}

func parsePlan9DataAddress(source string) (Symbol, uint64, uint64, error) {
	source = strings.TrimSpace(source)
	slash := strings.LastIndexByte(source, '/')
	if slash < 0 {
		return Symbol{}, 0, 0, fmt.Errorf("DATA address %q has no width", source)
	}
	width, err := strconv.ParseUint(strings.TrimSpace(source[slash+1:]), 0, 64)
	if err != nil || width == 0 {
		return Symbol{}, 0, 0, fmt.Errorf("invalid DATA width in %q", source)
	}
	address := strings.TrimSpace(source[:slash])
	if !strings.HasSuffix(address, "(SB)") {
		return Symbol{}, 0, 0, fmt.Errorf("DATA address %q is not relative to SB", source)
	}
	nameAndOffset := strings.TrimSpace(strings.TrimSuffix(address, "(SB)"))
	name := nameAndOffset
	offset := uint64(0)
	for index := len(nameAndOffset) - 1; index > 0; index-- {
		if nameAndOffset[index] != '+' {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(nameAndOffset[index+1:]), 0, 64)
		if err != nil {
			continue
		}
		name = strings.TrimSpace(nameAndOffset[:index])
		offset = parsed
		break
	}
	if name == "" {
		return Symbol{}, 0, 0, fmt.Errorf("DATA address %q has no symbol", source)
	}
	return parseSymbol(name), offset, width, nil
}

func hasPlan9Flag(flags, want string) bool {
	for _, flag := range strings.Split(flags, "|") {
		if canonicalPlan9Flag(strings.TrimSpace(flag)) == want {
			return true
		}
	}
	return false
}
