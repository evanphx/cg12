package plan9asm

import (
	"fmt"
	"strconv"
	"strings"
)

func (t *arm64Translator) translateVectorLoad(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires an address and vector list", instruction.Opcode)
	}
	memory := instruction.Operands[0]
	list := instruction.Operands[1]
	if memory.Kind != OperandMemory || memory.Base == "SB" {
		return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, memory.Text, list.Text)
	}

	registers, lane, postIncrement, err := formatARM64VectorLoadList(instruction.Opcode, list)
	if err != nil {
		return err
	}
	address, err := t.vectorMemoryAddress(memory, instruction.Suffix, postIncrement)
	if err != nil {
		return err
	}

	mnemonic := map[string]string{"VLD1": "ld1", "VLD1R": "ld1r", "VLD4R": "ld4r"}[instruction.Opcode]
	registerList := fmt.Sprintf("{%s}", strings.Join(registers, ", "))
	if lane != "" {
		registerList += "[" + lane + "]"
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, registerList, address)
	return nil
}

func formatARM64VectorLoadList(opcode string, list Operand) ([]string, string, int, error) {
	if list.Kind == OperandVectorList {
		registers, width, err := formatARM64VectorList(list)
		return registers, "", width, err
	}
	if opcode != "VLD1" || list.Kind != OperandVectorRegister || list.Vector.Index == "" {
		return nil, "", 0, fmt.Errorf("unsupported %s vector operand %q", opcode, list.Text)
	}
	register := strings.ToLower(list.Vector.Register) + "." + strings.ToLower(list.Vector.Arrangement)
	width := map[byte]int{'B': 1, 'H': 2, 'S': 4, 'D': 8}[list.Vector.Arrangement[0]]
	if width == 0 {
		return nil, "", 0, fmt.Errorf("invalid vector lane arrangement %q", list.Vector.Arrangement)
	}
	return []string{register}, list.Vector.Index, width, nil
}

func (t *arm64Translator) translateVectorStore(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a vector list and address", instruction.Opcode)
	}
	list := instruction.Operands[0]
	memory := instruction.Operands[1]
	if list.Kind != OperandVectorList || memory.Kind != OperandMemory || memory.Base == "SB" {
		return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, list.Text, memory.Text)
	}
	registers, postIncrement, err := formatARM64VectorList(list)
	if err != nil {
		return err
	}
	address, err := t.vectorMemoryAddress(memory, instruction.Suffix, postIncrement)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tst1 {%s}, %s\n", strings.Join(registers, ", "), address)
	return nil
}

func formatARM64VectorList(list Operand) ([]string, int, error) {
	registers := make([]string, 0, len(list.Vectors))
	totalWidth := 0
	for _, vector := range list.Vectors {
		formatted, err := formatARM64Vector(vector)
		if err != nil {
			return nil, 0, err
		}
		registers = append(registers, formatted)
		width, err := vectorArrangementBytes(vector.Arrangement)
		if err != nil {
			return nil, 0, err
		}
		totalWidth += width
	}
	return registers, totalWidth, nil
}

func (t *arm64Translator) vectorMemoryAddress(memory Operand, suffix string, postIncrement int) (string, error) {
	if suffix == "P" && memory.Offset == "" && memory.Index == "" {
		memory.Offset = strconv.Itoa(postIncrement)
	}
	return t.memoryAddress(memory, suffix)
}

func (t *arm64Translator) translateVectorBinary(instruction *Instruction) error {
	if len(instruction.Operands) == 2 {
		return t.translateScalarVectorBinary(instruction)
	}
	if len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires two sources and a destination", instruction.Opcode)
	}
	first, err := vectorOperand(instruction.Operands[0])
	if err != nil {
		return err
	}
	second, err := vectorOperand(instruction.Operands[1])
	if err != nil {
		return err
	}
	destination, err := vectorOperand(instruction.Operands[2])
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimPrefix(instruction.Opcode, "V"))
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destination, second, first)
	return nil
}

func (t *arm64Translator) translateScalarVectorBinary(instruction *Instruction) error {
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind != OperandVectorRegister || destination.Kind != OperandVectorRegister ||
		source.Vector.Arrangement != "" || destination.Vector.Arrangement != "" {
		return fmt.Errorf("unsupported scalar %s operands %q, %q", instruction.Opcode, source.Text, destination.Text)
	}
	mnemonic := strings.ToLower(strings.TrimPrefix(instruction.Opcode, "V"))
	sourceRegister := "d" + strings.TrimPrefix(strings.ToLower(source.Vector.Register), "v")
	destinationRegister := "d" + strings.TrimPrefix(strings.ToLower(destination.Vector.Register), "v")
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destinationRegister, destinationRegister, sourceRegister)
	return nil
}

func (t *arm64Translator) translateVectorAddLongAcross(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("VUADDLV requires a source and destination")
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind != OperandVectorRegister || source.Vector.Arrangement == "" || source.Vector.Index != "" ||
		destination.Kind != OperandVectorRegister || destination.Vector.Arrangement != "" {
		return fmt.Errorf("unsupported VUADDLV operands %q, %q", source.Text, destination.Text)
	}
	sourceRegister, err := formatARM64Vector(source.Vector)
	if err != nil {
		return err
	}
	scalarPrefix := map[byte]string{'B': "h", 'H': "s", 'S': "d"}[source.Vector.Arrangement[0]]
	if scalarPrefix == "" {
		return fmt.Errorf("unsupported VUADDLV source arrangement %s", source.Vector.Arrangement)
	}
	destinationRegister := scalarPrefix + strings.TrimPrefix(strings.ToLower(destination.Vector.Register), "v")
	fmt.Fprintf(&t.output, "\tuaddlv %s, %s\n", destinationRegister, sourceRegister)
	return nil
}

func (t *arm64Translator) translateVectorMove(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("VMOV requires a source and destination")
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind == OperandVectorRegister && source.Vector.Arrangement != "" && source.Vector.Index == "" &&
		destination.Kind == OperandVectorRegister && destination.Vector.Arrangement != "" && destination.Vector.Index == "" {
		sourceRegister, err := formatARM64Vector(source.Vector)
		if err != nil {
			return err
		}
		destinationRegister, err := formatARM64Vector(destination.Vector)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", destinationRegister, sourceRegister)
		return nil
	}
	if source.Kind == OperandVectorRegister && source.Vector.Index != "" &&
		destination.Kind == OperandVectorRegister && destination.Vector.Arrangement == "" && destination.Vector.Index == "" {
		sourceRegister, err := formatARM64Vector(source.Vector)
		if err != nil {
			return err
		}
		destinationRegister, err := scalarSIMDRegister(destination.Vector.Register, source.Vector.Arrangement)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", destinationRegister, sourceRegister)
		return nil
	}

	if source.Kind == OperandVectorRegister && source.Vector.Index != "" && destination.Kind == OperandRegister {
		width := vectorElementRegisterWidth(source.Vector.Arrangement)
		destinationRegister, err := arm64Register(destination.Register, width)
		if err != nil {
			return err
		}
		sourceRegister, err := formatARM64Vector(source.Vector)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", destinationRegister, sourceRegister)
		return nil
	}

	if source.Kind == OperandRegister && destination.Kind == OperandVectorRegister &&
		destination.Vector.Arrangement != "" && destination.Vector.Index == "" {
		width := vectorElementRegisterWidth(destination.Vector.Arrangement[:1])
		sourceRegister, err := arm64Register(source.Register, width)
		if err != nil {
			return err
		}
		destinationRegister, err := formatARM64Vector(destination.Vector)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tdup %s, %s\n", destinationRegister, sourceRegister)
		return nil
	}

	if source.Kind == OperandRegister && destination.Kind == OperandVectorRegister && destination.Vector.Index != "" {
		width := vectorElementRegisterWidth(destination.Vector.Arrangement)
		sourceRegister, err := arm64Register(source.Register, width)
		if err != nil {
			return err
		}
		destinationRegister, err := formatARM64Vector(destination.Vector)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", destinationRegister, sourceRegister)
		return nil
	}

	return fmt.Errorf("unsupported VMOV operands %q, %q", source.Text, destination.Text)
}

func (t *arm64Translator) translateVectorDuplicate(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("VDUP requires a source and destination")
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind != OperandVectorRegister || source.Vector.Index == "" ||
		destination.Kind != OperandVectorRegister || destination.Vector.Arrangement == "" || destination.Vector.Index != "" {
		return fmt.Errorf("unsupported VDUP operands %q, %q", source.Text, destination.Text)
	}
	sourceRegister, err := formatARM64Vector(source.Vector)
	if err != nil {
		return err
	}
	destinationRegister, err := formatARM64Vector(destination.Vector)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tdup %s, %s\n", destinationRegister, sourceRegister)
	return nil
}

func scalarSIMDRegister(register, arrangement string) (string, error) {
	number := strings.TrimPrefix(strings.ToLower(register), "v")
	prefix := map[byte]string{'B': "b", 'H': "h", 'S': "s", 'D': "d"}[arrangement[0]]
	if prefix == "" {
		return "", fmt.Errorf("unsupported scalar SIMD arrangement %s", arrangement)
	}
	return prefix + number, nil
}

func (t *arm64Translator) translateVectorUnary(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	source, err := vectorOperand(instruction.Operands[0])
	if err != nil {
		return err
	}
	destination, err := vectorOperand(instruction.Operands[1])
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimPrefix(instruction.Opcode, "V"))
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
	return nil
}

func (t *arm64Translator) translateVectorCrypto(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind != OperandVectorRegister || source.Vector.Arrangement != "B16" || source.Vector.Index != "" ||
		destination.Kind != OperandVectorRegister || destination.Vector.Arrangement != "B16" || destination.Vector.Index != "" {
		return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, source.Text, destination.Text)
	}
	sourceRegister, err := formatARM64Vector(source.Vector)
	if err != nil {
		return err
	}
	destinationRegister, err := formatARM64Vector(destination.Vector)
	if err != nil {
		return err
	}
	if !t.cryptoEnabled {
		t.output.WriteString("\t.arch_extension crypto\n")
		t.cryptoEnabled = true
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", strings.ToLower(instruction.Opcode), destinationRegister, sourceRegister)
	return nil
}

func (t *arm64Translator) translateSHA256(instruction *Instruction) error {
	if !t.cryptoEnabled {
		t.output.WriteString("\t.arch_extension crypto\n")
		t.cryptoEnabled = true
	}

	switch instruction.Opcode {
	case "SHA256H", "SHA256H2":
		if len(instruction.Operands) != 3 {
			return fmt.Errorf("%s requires two sources and a destination", instruction.Opcode)
		}
		message, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		state, err := arm64QRegister(instruction.Operands[1])
		if err != nil {
			return err
		}
		destination, err := arm64QRegister(instruction.Operands[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", strings.ToLower(instruction.Opcode), destination, state, message)
		return nil
	case "SHA256SU0":
		if len(instruction.Operands) != 2 {
			return fmt.Errorf("SHA256SU0 requires a source and destination")
		}
		source, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		destination, err := vectorOperand(instruction.Operands[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha256su0 %s, %s\n", destination, source)
		return nil
	case "SHA256SU1":
		if len(instruction.Operands) != 3 {
			return fmt.Errorf("SHA256SU1 requires two sources and a destination")
		}
		second, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		first, err := vectorOperand(instruction.Operands[1])
		if err != nil {
			return err
		}
		destination, err := vectorOperand(instruction.Operands[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha256su1 %s, %s, %s\n", destination, first, second)
		return nil
	default:
		return fmt.Errorf("unsupported SHA-256 instruction %s", instruction.Opcode)
	}
}

func (t *arm64Translator) translateSHA1(instruction *Instruction) error {
	if !t.cryptoEnabled {
		t.output.WriteString("\t.arch_extension crypto\n")
		t.cryptoEnabled = true
	}

	switch instruction.Opcode {
	case "SHA1C", "SHA1P", "SHA1M":
		if len(instruction.Operands) != 3 {
			return fmt.Errorf("%s requires two sources and a destination", instruction.Opcode)
		}
		message, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		state, err := bareScalarSIMDOperand(instruction.Operands[1], "S")
		if err != nil {
			return err
		}
		destination, err := arm64QRegister(instruction.Operands[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", strings.ToLower(instruction.Opcode), destination, state, message)
		return nil
	case "SHA1H":
		if len(instruction.Operands) != 2 {
			return fmt.Errorf("SHA1H requires a source and destination")
		}
		source, err := bareScalarSIMDOperand(instruction.Operands[0], "S")
		if err != nil {
			return err
		}
		destination, err := bareScalarSIMDOperand(instruction.Operands[1], "S")
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha1h %s, %s\n", destination, source)
		return nil
	case "SHA1SU0":
		if len(instruction.Operands) != 3 {
			return fmt.Errorf("SHA1SU0 requires two sources and a destination")
		}
		second, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		first, err := vectorOperand(instruction.Operands[1])
		if err != nil {
			return err
		}
		destination, err := vectorOperand(instruction.Operands[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha1su0 %s, %s, %s\n", destination, first, second)
		return nil
	case "SHA1SU1":
		if len(instruction.Operands) != 2 {
			return fmt.Errorf("SHA1SU1 requires a source and destination")
		}
		source, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		destination, err := vectorOperand(instruction.Operands[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha1su1 %s, %s\n", destination, source)
		return nil
	default:
		return fmt.Errorf("unsupported SHA-1 instruction %s", instruction.Opcode)
	}
}

func bareScalarSIMDOperand(operand Operand, arrangement string) (string, error) {
	if operand.Kind != OperandVectorRegister || operand.Vector.Arrangement != "" || operand.Vector.Index != "" {
		return "", fmt.Errorf("operand %q must be a scalar SIMD register", operand.Text)
	}
	return scalarSIMDRegister(operand.Vector.Register, arrangement)
}

func (t *arm64Translator) translateSHA512(instruction *Instruction) error {
	if !t.sha512Enabled {
		t.output.WriteString("\t.arch_extension sha3\n")
		t.sha512Enabled = true
	}

	switch instruction.Opcode {
	case "SHA512H", "SHA512H2":
		if len(instruction.Operands) != 3 {
			return fmt.Errorf("%s requires two sources and a destination", instruction.Opcode)
		}
		message, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		state, err := arm64QRegister(instruction.Operands[1])
		if err != nil {
			return err
		}
		destination, err := arm64QRegister(instruction.Operands[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", strings.ToLower(instruction.Opcode), destination, state, message)
		return nil
	case "SHA512SU0":
		if len(instruction.Operands) != 2 {
			return fmt.Errorf("SHA512SU0 requires a source and destination")
		}
		source, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		destination, err := vectorOperand(instruction.Operands[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha512su0 %s, %s\n", destination, source)
		return nil
	case "SHA512SU1":
		if len(instruction.Operands) != 3 {
			return fmt.Errorf("SHA512SU1 requires two sources and a destination")
		}
		second, err := vectorOperand(instruction.Operands[0])
		if err != nil {
			return err
		}
		first, err := vectorOperand(instruction.Operands[1])
		if err != nil {
			return err
		}
		destination, err := vectorOperand(instruction.Operands[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tsha512su1 %s, %s, %s\n", destination, first, second)
		return nil
	default:
		return fmt.Errorf("unsupported SHA-512 instruction %s", instruction.Opcode)
	}
}

func arm64QRegister(operand Operand) (string, error) {
	if operand.Kind != OperandVectorRegister || operand.Vector.Index != "" {
		return "", fmt.Errorf("operand %q must be a vector register", operand.Text)
	}
	return "q" + strings.TrimPrefix(strings.ToLower(operand.Vector.Register), "v"), nil
}

func (t *arm64Translator) translateVectorImmediate(instruction *Instruction) error {
	if len(instruction.Operands) != 3 || instruction.Operands[0].Kind != OperandImmediate {
		return fmt.Errorf("%s requires an immediate, source, and destination", instruction.Opcode)
	}
	source, err := vectorOperand(instruction.Operands[1])
	if err != nil {
		return err
	}
	destination, err := vectorOperand(instruction.Operands[2])
	if err != nil {
		return err
	}
	mnemonic := map[string]string{"VSHL": "shl", "VSRI": "sri"}[instruction.Opcode]
	fmt.Fprintf(&t.output, "\t%s %s, %s, #%s\n", mnemonic, destination, source, instruction.Operands[0].Immediate)
	return nil
}

func (t *arm64Translator) translateVectorExtract(instruction *Instruction) error {
	if len(instruction.Operands) != 4 || instruction.Operands[0].Kind != OperandImmediate {
		return fmt.Errorf("VEXT requires an immediate, two sources, and a destination")
	}
	second, err := vectorOperand(instruction.Operands[1])
	if err != nil {
		return err
	}
	first, err := vectorOperand(instruction.Operands[2])
	if err != nil {
		return err
	}
	destination, err := vectorOperand(instruction.Operands[3])
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\text %s, %s, %s, #%s\n", destination, first, second, instruction.Operands[0].Immediate)
	return nil
}

func (t *arm64Translator) translateVectorTable(instruction *Instruction) error {
	if len(instruction.Operands) != 3 || instruction.Operands[1].Kind != OperandVectorList {
		return fmt.Errorf("VTBL requires an index, table list, and destination")
	}
	index, err := vectorOperand(instruction.Operands[0])
	if err != nil {
		return err
	}
	table, _, err := formatARM64VectorList(instruction.Operands[1])
	if err != nil {
		return err
	}
	destination, err := vectorOperand(instruction.Operands[2])
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\ttbl %s, {%s}, %s\n", destination, strings.Join(table, ", "), index)
	return nil
}

func vectorOperand(operand Operand) (string, error) {
	if operand.Kind != OperandVectorRegister || operand.Vector.Arrangement == "" || operand.Vector.Index != "" {
		return "", fmt.Errorf("operand %q must be an arranged vector register", operand.Text)
	}
	return formatARM64Vector(operand.Vector)
}

func formatARM64Vector(vector VectorRegister) (string, error) {
	register := strings.ToLower(vector.Register)
	if vector.Arrangement == "" {
		return register, nil
	}
	if vector.Index != "" {
		return fmt.Sprintf("%s.%s[%s]", register, strings.ToLower(vector.Arrangement), vector.Index), nil
	}
	if len(vector.Arrangement) < 2 {
		return "", fmt.Errorf("vector register %s has no lane count", vector.Register)
	}
	element := strings.ToLower(vector.Arrangement[:1])
	lanes := vector.Arrangement[1:]
	return fmt.Sprintf("%s.%s%s", register, lanes, element), nil
}

func vectorArrangementBytes(arrangement string) (int, error) {
	if len(arrangement) < 2 {
		return 0, fmt.Errorf("vector arrangement %q has no lane count", arrangement)
	}
	lanes, err := strconv.Atoi(arrangement[1:])
	if err != nil {
		return 0, fmt.Errorf("invalid vector arrangement %q", arrangement)
	}
	elementBytes := map[byte]int{'B': 1, 'H': 2, 'S': 4, 'D': 8}[arrangement[0]]
	if elementBytes == 0 {
		return 0, fmt.Errorf("invalid vector arrangement %q", arrangement)
	}
	return lanes * elementBytes, nil
}

func vectorElementRegisterWidth(arrangement string) int {
	if strings.HasPrefix(arrangement, "D") {
		return 64
	}
	return 32
}
