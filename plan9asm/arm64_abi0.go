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
	float  bool
}

type abi0Layout struct {
	inputs  []abi0Slot
	outputs []abi0Slot
}

func collectDirectABI0(file *File, layouts map[int]abi0Layout) map[int]bool {
	direct := make(map[int]bool)
	functionIndex := -1
	var text *Text
	called := false
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			functionIndex++
			text = statement
			called = false
			direct[functionIndex] = !text.Symbol.Static && text.Symbol.ABI == "" && len(layouts[functionIndex].outputs) <= 1
			if text.Symbol.Name == "·reflectcall" {
				direct[functionIndex] = false
			}
		case *Instruction:
			if text == nil || !direct[functionIndex] {
				continue
			}
			for _, operand := range statement.Operands {
				if strings.Contains(operand.Text, "(FP)") && operand.Kind != OperandMemory {
					direct[functionIndex] = false
					break
				}
			}
			if !direct[functionIndex] {
				continue
			}
			if statement.Opcode == "BL" || statement.Opcode == "CALL" {
				called = true
				continue
			}
			if len(statement.Operands) != 2 {
				for _, operand := range statement.Operands {
					if operand.Base == "FP" {
						direct[functionIndex] = false
						break
					}
				}
				continue
			}
			source := statement.Operands[0]
			destination := statement.Operands[1]
			if source.Kind == OperandMemory && source.Base == "FP" {
				offset, err := namedFrameOffset(source.Offset)
				index := abi0SlotIndex(layouts[functionIndex].inputs, offset)
				if called || err != nil || index < 0 || destination.Kind != OperandRegister {
					direct[functionIndex] = false
				}
			}
			if destination.Kind == OperandMemory && destination.Base == "FP" {
				offset, err := namedFrameOffset(destination.Offset)
				outputs := layouts[functionIndex].outputs
				if err != nil || len(outputs) != 1 || outputs[0].offset != offset {
					direct[functionIndex] = false
				}
			}
		}
	}
	return direct
}

func abi0SlotIndex(slots []abi0Slot, offset int) int {
	for index, slot := range slots {
		if slot.offset == offset {
			return index
		}
	}
	return -1
}

func abi0SlotRegisterIndex(slots []abi0Slot, offset int, floatingPoint bool) int {
	registerIndex := 0
	for _, slot := range slots {
		if slot.float != floatingPoint {
			continue
		}
		if slot.offset == offset {
			return registerIndex
		}
		registerIndex++
	}
	return -1
}

func (t *arm64Translator) emitDirectABI0Load(instruction *Instruction, source Operand, destination string) error {
	offset, err := namedFrameOffset(source.Offset)
	if err != nil {
		return err
	}
	slotIndex := abi0SlotIndex(t.currentABI0Layout.inputs, offset)
	if slotIndex < 0 {
		return fmt.Errorf("ABI0 input offset %d has no register", offset)
	}
	if t.currentABI0Layout.inputs[slotIndex].float {
		index := abi0SlotRegisterIndex(t.currentABI0Layout.inputs, offset, true)
		input, err := arm64FloatRegister("F"+strconv.Itoa(index), moveWidth(instruction.Opcode))
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destination, input)
		return nil
	}
	index := abi0SlotRegisterIndex(t.currentABI0Layout.inputs, offset, false)
	if index < 0 {
		return fmt.Errorf("ABI0 input offset %d has no register", offset)
	}
	width := moveWidth(instruction.Opcode)
	input, err := arm64Register("R"+strconv.Itoa(index), width)
	if err != nil {
		return err
	}
	if input != destination {
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", destination, input)
	}
	return nil
}

func (t *arm64Translator) emitDirectABI0Store(instruction *Instruction, sourceRegister string, destination Operand) error {
	offset, err := namedFrameOffset(destination.Offset)
	if err != nil {
		return err
	}
	if len(t.currentABI0Layout.outputs) != 1 || t.currentABI0Layout.outputs[0].offset != offset {
		return fmt.Errorf("ABI0 output offset %d is not the primary result", offset)
	}
	width := abi0MoveWidth(instruction.Opcode)
	if t.currentABI0Layout.outputs[0].float {
		result, err := arm64FloatRegister("F0", width*8)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tfmov %s, %s\n", result, sourceRegister)
		return nil
	}
	resultWidth := 64
	if width < 8 {
		resultWidth = 32
	}
	result, err := arm64Register("R0", resultWidth)
	if err != nil {
		return err
	}
	if sourceRegister != result {
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", result, sourceRegister)
	}
	return nil
}

func (t *arm64Translator) emitDirectABI0FloatLoad(source Operand, destination string, width int) error {
	offset, err := namedFrameOffset(source.Offset)
	if err != nil {
		return err
	}
	index := abi0SlotRegisterIndex(t.currentABI0Layout.inputs, offset, true)
	if index < 0 {
		return fmt.Errorf("ABI0 floating-point input offset %d has no register", offset)
	}
	input, err := arm64FloatRegister("F"+strconv.Itoa(index), width)
	if err != nil {
		return err
	}
	if input != destination {
		fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destination, input)
	}
	return nil
}

func (t *arm64Translator) emitDirectABI0FloatStore(source string, destination Operand, width int) error {
	offset, err := namedFrameOffset(destination.Offset)
	if err != nil {
		return err
	}
	if len(t.currentABI0Layout.outputs) != 1 || t.currentABI0Layout.outputs[0].offset != offset {
		return fmt.Errorf("ABI0 floating-point output offset %d is not the primary result", offset)
	}
	result, err := arm64FloatRegister("F0", width)
	if err != nil {
		return err
	}
	if result != source {
		fmt.Fprintf(&t.output, "\tfmov %s, %s\n", result, source)
	}
	return nil
}

func collectABI0Layouts(file *File, options ARM64Options) map[int]abi0Layout {
	layouts := make(map[int]abi0Layout)
	functionNames := make(map[string]int)
	aliases := make(map[int]string)
	functionIndex := -1
	var text *Text
	inputSlots := make(map[int]int)
	outputSlots := make(map[int]int)
	inputFloats := make(map[int]bool)
	outputFloats := make(map[int]bool)
	finish := func() {
		if text == nil || text.Symbol.Static || text.Symbol.ABI != "" {
			return
		}
		functionName := strings.TrimPrefix(text.Symbol.Name, "·")
		if text.Symbol.Name == "·reflectcall" {
			for _, slot := range []abi0Slot{
				{offset: 0, width: 8},
				{offset: 8, width: 8},
				{offset: 16, width: 8},
				{offset: 24, width: 4},
				{offset: 28, width: 4},
				{offset: 32, width: 4},
				{offset: 40, width: 8},
			} {
				inputSlots[slot.offset] = max(inputSlots[slot.offset], slot.width)
			}
		}
		for _, offset := range options.FloatInputs[functionName] {
			inputFloats[offset] = true
		}
		for _, offset := range options.FloatOutputs[functionName] {
			outputFloats[offset] = true
		}
		layouts[functionIndex] = abi0Layout{
			inputs:  sortedABI0Slots(inputSlots, inputFloats),
			outputs: sortedABI0Slots(outputSlots, outputFloats),
		}
	}

	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			finish()
			functionIndex++
			text = statement
			functionNames[statement.Symbol.Name] = functionIndex
			inputSlots = make(map[int]int)
			outputSlots = make(map[int]int)
			inputFloats = make(map[int]bool)
			outputFloats = make(map[int]bool)
		case *Instruction:
			if text == nil || text.Symbol.Static || text.Symbol.ABI != "" {
				continue
			}
			if statement.Opcode == "B" && len(statement.Operands) == 1 {
				if symbol, ok := operandSymbol(statement.Operands[0]); ok {
					aliases[functionIndex] = symbol.Name
				}
			}
			if len(statement.Operands) != 2 {
				continue
			}
			width := abi0MoveWidth(statement.Opcode)
			if width == 0 {
				continue
			}
			if source := statement.Operands[0]; source.Kind == OperandMemory && source.Base == "FP" {
				if offset, err := namedFrameOffset(source.Offset); err == nil {
					inputSlots[offset] = max(inputSlots[offset], width)
					inputFloats[offset] = strings.HasPrefix(statement.Opcode, "FMOV")
					if strings.HasSuffix(namedFrameField(source.Offset), "_base") {
						// Go names a slice's three ABI0 words name_base,
						// name_len, and name_cap. Assembly commonly reads only the
						// base, but ABIInternal still consumes registers for all
						// three words before assigning the following argument.
						inputSlots[offset+8] = max(inputSlots[offset+8], 8)
						inputSlots[offset+16] = max(inputSlots[offset+16], 8)
					}
				}
			}
			if destination := statement.Operands[1]; destination.Kind == OperandMemory && destination.Base == "FP" {
				if offset, err := namedFrameOffset(destination.Offset); err == nil {
					outputSlots[offset] = max(outputSlots[offset], width)
					outputFloats[offset] = strings.HasPrefix(statement.Opcode, "FMOV")
				}
			}
		}
	}
	finish()
	for range len(layouts) {
		changed := false
		for index, targetName := range aliases {
			layout := layouts[index]
			if len(layout.inputs) != 0 || len(layout.outputs) != 0 {
				continue
			}
			targetIndex, ok := functionNames[targetName]
			if !ok {
				continue
			}
			target := layouts[targetIndex]
			if len(target.inputs) == 0 && len(target.outputs) == 0 {
				continue
			}
			layouts[index] = target
			changed = true
		}
		if !changed {
			break
		}
	}
	return layouts
}

func namedFrameField(source string) string {
	source = strings.TrimSpace(source)
	for index := len(source) - 1; index > 0; index-- {
		if source[index] == '+' || source[index] == '-' {
			return strings.TrimSpace(source[:index])
		}
	}
	return ""
}

func sortedABI0Slots(widths map[int]int, floats map[int]bool) []abi0Slot {
	slots := make([]abi0Slot, 0, len(widths))
	for offset, width := range widths {
		slots = append(slots, abi0Slot{offset: offset, width: width, float: floats[offset]})
	}
	sort.Slice(slots, func(left, right int) bool {
		return slots[left].offset < slots[right].offset
	})
	return slots
}

func abi0MoveWidth(opcode string) int {
	switch opcode {
	case "MOVD", "FMOVD":
		return 8
	case "MOVW", "MOVWU", "FMOVS":
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
	integerInputs := 0
	floatInputs := 0
	for _, slot := range layout.inputs {
		if slot.float {
			floatInputs++
		} else {
			integerInputs++
		}
	}
	if integerInputs+extraResults > 16 || floatInputs > 16 {
		return "", 0, fmt.Errorf("ABI0 wrapper for %s needs more than 16 integer registers", name)
	}

	abiAreaEnd := 8
	for _, slots := range [][]abi0Slot{layout.inputs, layout.outputs} {
		for _, slot := range slots {
			abiAreaEnd = max(abiAreaEnd, 8+slot.offset+slot.width)
		}
	}
	preservedPointers := roundUpInteger(abiAreaEnd, 8)
	framePointerOffset := preservedPointers + extraResults*8
	// Leave the final stack word unused. Go ARM64 callers keep their saved frame
	// pointer at SP-8, so placing wrapper state at frameSize-8 would overwrite
	// the caller's frame chain.
	frameSize := roundUpInteger(framePointerOffset+16, 16)

	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", name)
	fmt.Fprintf(&t.output, ".type %s, %%function\n", name)
	fmt.Fprintf(&t.output, "%s:\n", name)
	fmt.Fprintf(&t.output, "\tsub sp, sp, #%d\n", frameSize)
	// ABI0 reserves the first stack word as the caller's link slot. Keeping
	// LR at 0(SP) matches Go's ARM64 frame layout and lets the runtime unwind
	// through the wrapper while the ABI0 body is active.
	t.output.WriteString("\tstr x30, [sp]\n")
	// Plan 9 assembly bodies with local frames establish their own frame pointer.
	// Preserve the wrapper's caller frame pointer outside the ABI0 argument and
	// result area so returning through the wrapper restores the Go frame chain.
	fmt.Fprintf(&t.output, "\tstr x29, [sp, #%d]\n", framePointerOffset)
	integerRegister := 0
	floatRegister := 0
	for _, slot := range layout.inputs {
		register := integerRegister
		if slot.float {
			register = floatRegister
			floatRegister++
		} else {
			integerRegister++
		}
		if err := emitABI0Store(&t.output, register, 8+slot.offset, slot); err != nil {
			return "", 0, err
		}
	}
	for index := 0; index < extraResults; index++ {
		register := integerInputs + index
		fmt.Fprintf(&t.output, "\tstr x%d, [sp, #%d]\n", register, preservedPointers+index*8)
	}
	fmt.Fprintf(&t.output, "\tbl %s\n", abi0Name)
	if len(layout.outputs) > 0 {
		if err := emitABI0Load(&t.output, 0, 8+layout.outputs[0].offset, layout.outputs[0]); err != nil {
			return "", 0, err
		}
	}
	for index := 1; index < len(layout.outputs); index++ {
		pointerOffset := preservedPointers + (index-1)*8
		fmt.Fprintf(&t.output, "\tldr x17, [sp, #%d]\n", pointerOffset)
		if err := emitABI0Load(&t.output, 16, 8+layout.outputs[index].offset, layout.outputs[index]); err != nil {
			return "", 0, err
		}
		if err := emitRegisterStore(&t.output, 16, 17, layout.outputs[index].width); err != nil {
			return "", 0, err
		}
	}
	fmt.Fprintf(&t.output, "\tldr x29, [sp, #%d]\n", framePointerOffset)
	t.output.WriteString("\tldr x30, [sp]\n")
	fmt.Fprintf(&t.output, "\tadd sp, sp, #%d\n", frameSize)
	t.output.WriteString("\tret\n")
	return abi0Name, frameSize, nil
}

func emitABI0Store(output *strings.Builder, register, offset int, slot abi0Slot) error {
	if slot.float {
		switch slot.width {
		case 4:
			fmt.Fprintf(output, "\tstr s%d, [sp, #%d]\n", register, offset)
		case 8:
			fmt.Fprintf(output, "\tstr d%d, [sp, #%d]\n", register, offset)
		default:
			return fmt.Errorf("unsupported floating-point ABI0 store width %d", slot.width)
		}
		return nil
	}
	switch slot.width {
	case 1:
		fmt.Fprintf(output, "\tstrb w%d, [sp, #%d]\n", register, offset)
	case 2:
		fmt.Fprintf(output, "\tstrh w%d, [sp, #%d]\n", register, offset)
	case 4:
		fmt.Fprintf(output, "\tstr w%d, [sp, #%d]\n", register, offset)
	case 8:
		fmt.Fprintf(output, "\tstr x%d, [sp, #%d]\n", register, offset)
	default:
		return fmt.Errorf("unsupported ABI0 store width %d", slot.width)
	}
	return nil
}

func emitABI0Load(output *strings.Builder, register, offset int, slot abi0Slot) error {
	if slot.float {
		switch slot.width {
		case 4:
			fmt.Fprintf(output, "\tldr s%d, [sp, #%d]\n", register, offset)
		case 8:
			fmt.Fprintf(output, "\tldr d%d, [sp, #%d]\n", register, offset)
		default:
			return fmt.Errorf("unsupported floating-point ABI0 load width %d", slot.width)
		}
		return nil
	}
	switch slot.width {
	case 1:
		fmt.Fprintf(output, "\tldrb w%d, [sp, #%d]\n", register, offset)
	case 2:
		fmt.Fprintf(output, "\tldrh w%d, [sp, #%d]\n", register, offset)
	case 4:
		fmt.Fprintf(output, "\tldr w%d, [sp, #%d]\n", register, offset)
	case 8:
		fmt.Fprintf(output, "\tldr x%d, [sp, #%d]\n", register, offset)
	default:
		return fmt.Errorf("unsupported ABI0 load width %d", slot.width)
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
	return t.abi0FrameAddressAvoiding(operand, suffix)
}

func (t *arm64Translator) abi0FrameAddressAvoiding(operand Operand, suffix string, avoid ...string) (string, error) {
	if suffix != "" {
		return "", fmt.Errorf("ABI0 frame operand %q does not accept .%s", operand.Text, suffix)
	}
	offset, err := namedFrameOffset(operand.Offset)
	if err != nil {
		return "", err
	}
	return t.memoryAddressAvoiding(Operand{
		Kind:   OperandMemory,
		Text:   operand.Text,
		Base:   "RSP",
		Offset: strconv.Itoa(t.currentFrame + 8 + offset),
	}, "", avoid...)
}

func abi0Symbol(name string) string {
	return name + "_abi0"
}

func roundUpInteger(value, alignment int) int {
	return (value + alignment - 1) &^ (alignment - 1)
}
