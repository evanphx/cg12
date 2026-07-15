package plan9asm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type abi0Slot struct {
	offset int
	width  int
}

type abi0Layout struct {
	inputs  []abi0Slot
	outputs []abi0Slot
}

func collectABI0Layouts(file *File) map[int]abi0Layout {
	layouts := make(map[int]abi0Layout)
	functionIndex := -1
	var text *Text
	inputSlots := make(map[int]int)
	outputSlots := make(map[int]int)
	finish := func() {
		if text == nil || text.Symbol.Static || text.Symbol.ABI != "" {
			return
		}
		layouts[functionIndex] = abi0Layout{
			inputs:  sortedABI0Slots(inputSlots),
			outputs: sortedABI0Slots(outputSlots),
		}
	}

	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			finish()
			functionIndex++
			text = statement
			inputSlots = make(map[int]int)
			outputSlots = make(map[int]int)
		case *Instruction:
			if text == nil || text.Symbol.Static || text.Symbol.ABI != "" || len(statement.Operands) != 2 {
				continue
			}
			width := abi0MoveWidth(statement.Opcode)
			if width == 0 {
				continue
			}
			if source := statement.Operands[0]; source.Kind == OperandMemory && source.Base == "FP" {
				if offset, err := namedFrameOffset(source.Offset); err == nil {
					inputSlots[offset] = max(inputSlots[offset], width)
				}
			}
			if destination := statement.Operands[1]; destination.Kind == OperandMemory && destination.Base == "FP" {
				if offset, err := namedFrameOffset(destination.Offset); err == nil {
					outputSlots[offset] = max(outputSlots[offset], width)
				}
			}
		}
	}
	finish()
	return layouts
}

func sortedABI0Slots(widths map[int]int) []abi0Slot {
	slots := make([]abi0Slot, 0, len(widths))
	for offset, width := range widths {
		slots = append(slots, abi0Slot{offset: offset, width: width})
	}
	sort.Slice(slots, func(left, right int) bool {
		return slots[left].offset < slots[right].offset
	})
	return slots
}

func abi0MoveWidth(opcode string) int {
	switch opcode {
	case "MOVD":
		return 8
	case "MOVW", "MOVWU":
		return 4
	case "MOVH", "MOVHU":
		return 2
	case "MOVB", "MOVBU":
		return 1
	default:
		return 0
	}
}

func namedFrameOffset(source string) (int, error) {
	source = strings.TrimSpace(source)
	if offset, err := strconv.ParseInt(source, 0, 32); err == nil {
		return int(offset), nil
	}
	for index := len(source) - 1; index > 0; index-- {
		if source[index] != '+' && source[index] != '-' {
			continue
		}
		offset, err := strconv.ParseInt(source[index:], 0, 32)
		if err == nil {
			return int(offset), nil
		}
	}
	return 0, fmt.Errorf("invalid named frame offset %q", source)
}

func (t *arm64Translator) emitABI0Wrapper(text *Text, layout abi0Layout) (string, int, error) {
	name := t.symbol(text.Symbol)
	abi0Name := abi0Symbol(name)
	extraResults := max(0, len(layout.outputs)-1)
	if len(layout.inputs)+extraResults > 16 {
		return "", 0, fmt.Errorf("ABI0 wrapper for %s needs more than 16 integer registers", name)
	}

	abiAreaEnd := 8
	for _, slots := range [][]abi0Slot{layout.inputs, layout.outputs} {
		for _, slot := range slots {
			abiAreaEnd = max(abiAreaEnd, 8+slot.offset+slot.width)
		}
	}
	preservedPointers := roundUpInteger(abiAreaEnd, 8)
	frameSize := roundUpInteger(preservedPointers+extraResults*8+8, 16)

	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", name)
	fmt.Fprintf(&t.output, ".type %s, %%function\n", name)
	fmt.Fprintf(&t.output, "%s:\n", name)
	fmt.Fprintf(&t.output, "\tsub sp, sp, #%d\n", frameSize)
	// ABI0 reserves the first stack word as the caller's link slot. Keeping
	// LR at 0(SP) matches Go's ARM64 frame layout and lets the runtime unwind
	// through the wrapper while the ABI0 body is active.
	t.output.WriteString("\tstr x30, [sp]\n")
	for index, slot := range layout.inputs {
		if err := emitABI0Store(&t.output, index, 8+slot.offset, slot.width); err != nil {
			return "", 0, err
		}
	}
	for index := 0; index < extraResults; index++ {
		register := len(layout.inputs) + index
		fmt.Fprintf(&t.output, "\tstr x%d, [sp, #%d]\n", register, preservedPointers+index*8)
	}
	fmt.Fprintf(&t.output, "\tbl %s\n", abi0Name)
	if len(layout.outputs) > 0 {
		if err := emitABI0Load(&t.output, 0, 8+layout.outputs[0].offset, layout.outputs[0].width); err != nil {
			return "", 0, err
		}
	}
	for index := 1; index < len(layout.outputs); index++ {
		pointerOffset := preservedPointers + (index-1)*8
		fmt.Fprintf(&t.output, "\tldr x17, [sp, #%d]\n", pointerOffset)
		if err := emitABI0Load(&t.output, 16, 8+layout.outputs[index].offset, layout.outputs[index].width); err != nil {
			return "", 0, err
		}
		if err := emitRegisterStore(&t.output, 16, 17, layout.outputs[index].width); err != nil {
			return "", 0, err
		}
	}
	t.output.WriteString("\tldr x30, [sp]\n")
	fmt.Fprintf(&t.output, "\tadd sp, sp, #%d\n", frameSize)
	t.output.WriteString("\tret\n")
	return abi0Name, frameSize, nil
}

func emitABI0Store(output *strings.Builder, register, offset, width int) error {
	switch width {
	case 1:
		fmt.Fprintf(output, "\tstrb w%d, [sp, #%d]\n", register, offset)
	case 2:
		fmt.Fprintf(output, "\tstrh w%d, [sp, #%d]\n", register, offset)
	case 4:
		fmt.Fprintf(output, "\tstr w%d, [sp, #%d]\n", register, offset)
	case 8:
		fmt.Fprintf(output, "\tstr x%d, [sp, #%d]\n", register, offset)
	default:
		return fmt.Errorf("unsupported ABI0 store width %d", width)
	}
	return nil
}

func emitABI0Load(output *strings.Builder, register, offset, width int) error {
	switch width {
	case 1:
		fmt.Fprintf(output, "\tldrb w%d, [sp, #%d]\n", register, offset)
	case 2:
		fmt.Fprintf(output, "\tldrh w%d, [sp, #%d]\n", register, offset)
	case 4:
		fmt.Fprintf(output, "\tldr w%d, [sp, #%d]\n", register, offset)
	case 8:
		fmt.Fprintf(output, "\tldr x%d, [sp, #%d]\n", register, offset)
	default:
		return fmt.Errorf("unsupported ABI0 load width %d", width)
	}
	return nil
}

func emitRegisterStore(output *strings.Builder, source, address, width int) error {
	switch width {
	case 1:
		fmt.Fprintf(output, "\tstrb w%d, [x%d]\n", source, address)
	case 2:
		fmt.Fprintf(output, "\tstrh w%d, [x%d]\n", source, address)
	case 4:
		fmt.Fprintf(output, "\tstr w%d, [x%d]\n", source, address)
	case 8:
		fmt.Fprintf(output, "\tstr x%d, [x%d]\n", source, address)
	default:
		return fmt.Errorf("unsupported ABI0 result width %d", width)
	}
	return nil
}

func (t *arm64Translator) abi0FrameAddress(operand Operand, suffix string) (string, error) {
	if suffix != "" {
		return "", fmt.Errorf("ABI0 frame operand %q does not accept .%s", operand.Text, suffix)
	}
	offset, err := namedFrameOffset(operand.Offset)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("[sp, #%d]", t.currentFrame+8+offset), nil
}

func abi0Symbol(name string) string {
	return name + "_abi0"
}

func roundUpInteger(value, alignment int) int {
	return (value + alignment - 1) &^ (alignment - 1)
}
