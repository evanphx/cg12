package plan9asm

import (
	"fmt"
	"strconv"
	"strings"
)

type arm64Translator struct {
	options       ARM64Options
	fileTag       string
	functionIndex int
	labels        map[int]map[string]string
	references    map[string]bool
	functions     []ARM64Function
	output        strings.Builder
}

func (t *arm64Translator) collectLabels(file *File) {
	functionIndex := -1
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			functionIndex++
		case *Label:
			if t.labels[functionIndex] == nil {
				t.labels[functionIndex] = make(map[string]string)
			}
			t.labels[functionIndex][statement.Name] = fmt.Sprintf(".L%s_%d_%s", t.fileTag, functionIndex, sanitizeSymbol(statement.Name))
		}
	}
	t.functionIndex = -1
}

func (t *arm64Translator) translate(statement Statement) error {
	switch statement := statement.(type) {
	case *Preprocessor:
		if strings.HasPrefix(statement.Text, "#include ") {
			return nil
		}
		return fmt.Errorf("unsupported preprocessor directive %q", statement.Text)
	case *Text:
		return t.translateText(statement)
	case *Label:
		fmt.Fprintf(&t.output, "%s:\n", t.localLabel(statement.Name))
		return nil
	case *Directive:
		return t.translateDirective(statement)
	case *Instruction:
		return t.translateInstruction(statement)
	default:
		return fmt.Errorf("unsupported statement %T", statement)
	}
}

func (t *arm64Translator) translateInstruction(instruction *Instruction) error {
	for _, operand := range instruction.Operands {
		symbol, ok := operandSymbol(operand)
		if ok && !symbol.Static {
			t.references[t.symbol(symbol)] = true
		}
	}
	opcode := instruction.Opcode
	switch opcode {
	case "MOVD", "MOVW", "MOVWU", "MOVH", "MOVHU", "MOVB", "MOVBU":
		return t.translateMove(instruction)
	case "VMOV":
		return t.translateVectorMove(instruction)
	case "VLD1":
		return t.translateVectorLoad(instruction)
	case "VCMEQ", "VAND", "VORR", "VEOR", "VADD", "VADDP":
		return t.translateVectorBinary(instruction)
	case "VUADDLV":
		return t.translateVectorAddLongAcross(instruction)
	case "LDP", "LDPW", "STP", "STPW":
		return t.translateRegisterPair(instruction)
	case "ADD", "ADDW", "ADDS", "ADDSW", "SUB", "SUBW", "SUBS", "SUBSW", "AND", "ANDW", "ANDS", "ANDSW", "BIC", "BICW", "BICS", "BICSW", "ORR", "ORRW", "EOR", "EORW", "LSL", "LSLW", "LSR", "LSRW", "ASR", "ASRW":
		return t.translateALU(instruction)
	case "NEG", "NEGW", "NEGS", "NEGSW":
		return t.translateNegate(instruction)
	case "RBIT", "RBITW", "CLZ", "CLZW":
		return t.translateUnaryRegister(instruction)
	case "CMP", "CMPW", "CMN", "CMNW", "TST", "TSTW":
		return t.translateCompare(instruction)
	case "CCMP", "CCMPW":
		return t.translateConditionalCompare(instruction)
	case "CSEL", "CSELW":
		return t.translateConditionalSelect(instruction)
	case "CINC", "CINCW", "CNEG", "CNEGW":
		return t.translateConditionalUnary(instruction)
	case "CSET", "CSETW":
		return t.translateConditionalSet(instruction)
	case "REV", "REVW", "REV16", "REV16W", "REV32":
		return t.translateReverse(instruction)
	case "B", "BL", "BEQ", "BNE", "BCS", "BCC", "BHS", "BLO", "BMI", "BPL", "BVS", "BVC", "BHI", "BLS", "BGE", "BLT", "BGT", "BLE":
		return t.translateBranch(instruction)
	case "CBZ", "CBZW", "CBNZ", "CBNZW":
		return t.translateCompareBranch(instruction)
	case "TBZ", "TBNZ":
		return t.translateTestBranch(instruction)
	case "DMB":
		return t.translateDMB(instruction)
	case "MRS":
		return t.translateMRS(instruction)
	case "DC":
		return t.translateDC(instruction)
	case "RET":
		if len(instruction.Operands) != 0 {
			return fmt.Errorf("RET operands are not supported yet")
		}
		t.output.WriteString("\tret\n")
		return nil
	case "NOOP":
		t.output.WriteString("\tnop\n")
		return nil
	default:
		return fmt.Errorf("unsupported ARM64 instruction %s", opcode)
	}
}

func (t *arm64Translator) translateMove(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	width := moveWidth(instruction.Opcode)
	if destination.Kind == OperandRegister {
		destinationRegister, err := arm64Register(destination.Register, width)
		if err != nil {
			return err
		}
		switch source.Kind {
		case OperandRegister:
			sourceRegister, err := arm64Register(source.Register, width)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tmov %s, %s\n", destinationRegister, sourceRegister)
			return nil
		case OperandImmediate:
			return t.emitMoveImmediate(destinationRegister, width, source.Immediate)
		case OperandMemory:
			return t.emitLoad(instruction, source, destinationRegister)
		}
	}
	if source.Kind == OperandRegister && destination.Kind == OperandMemory {
		sourceRegister, err := arm64Register(source.Register, storeWidth(instruction.Opcode))
		if err != nil {
			return err
		}
		return t.emitStore(instruction, sourceRegister, destination)
	}
	return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, source.Text, destination.Text)
}

func (t *arm64Translator) emitMoveImmediate(destination string, width int, immediate string) error {
	normalized := normalizeImmediate(immediate)
	var value uint64
	if strings.HasPrefix(normalized, "-") {
		signed, err := strconv.ParseInt(normalized, 0, 64)
		if err != nil {
			return fmt.Errorf("invalid integer immediate $%s", immediate)
		}
		value = uint64(signed)
	} else {
		unsigned, err := strconv.ParseUint(normalized, 0, 64)
		if err != nil {
			return fmt.Errorf("invalid integer immediate $%s", immediate)
		}
		value = unsigned
	}
	if width == 32 {
		value &= 0xffffffff
	}
	allBits := uint64(0xffffffffffffffff)
	if width == 32 {
		allBits = 0xffffffff
	}
	if value <= 0xffff {
		fmt.Fprintf(&t.output, "\tmov %s, #%d\n", destination, value)
		return nil
	}
	if value == allBits {
		fmt.Fprintf(&t.output, "\tmov %s, #-1\n", destination)
		return nil
	}

	chunks := width / 16
	first := -1
	for index := 0; index < chunks; index++ {
		if (value>>uint(index*16))&0xffff != 0 {
			first = index
			break
		}
	}
	if first < 0 {
		fmt.Fprintf(&t.output, "\tmov %s, #0\n", destination)
		return nil
	}
	fmt.Fprintf(&t.output, "\tmovz %s, #0x%x, lsl #%d\n", destination, (value>>uint(first*16))&0xffff, first*16)
	for index := first + 1; index < chunks; index++ {
		chunk := (value >> uint(index*16)) & 0xffff
		if chunk != 0 {
			fmt.Fprintf(&t.output, "\tmovk %s, #0x%x, lsl #%d\n", destination, chunk, index*16)
		}
	}
	return nil
}

func (t *arm64Translator) emitLoad(instruction *Instruction, source Operand, destination string) error {
	mnemonic := map[string]string{
		"MOVD":  "ldr",
		"MOVW":  "ldrsw",
		"MOVWU": "ldr",
		"MOVH":  "ldrsh",
		"MOVHU": "ldrh",
		"MOVB":  "ldrsb",
		"MOVBU": "ldrb",
	}[instruction.Opcode]
	if source.Base == "SB" {
		if instruction.Suffix != "" {
			return fmt.Errorf("symbolic load does not accept .%s", instruction.Suffix)
		}
		name := t.symbol(source.Symbol)
		fmt.Fprintf(&t.output, "\tadrp x27, %s\n", name)
		fmt.Fprintf(&t.output, "\t%s %s, [x27, :lo12:%s]\n", mnemonic, destination, name)
		return nil
	}
	address, err := memoryAddress(source, instruction.Suffix)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, address)
	return nil
}

func (t *arm64Translator) emitStore(instruction *Instruction, sourceRegister string, destination Operand) error {
	mnemonic := map[string]string{
		"MOVD":  "str",
		"MOVW":  "str",
		"MOVWU": "str",
		"MOVH":  "strh",
		"MOVHU": "strh",
		"MOVB":  "strb",
		"MOVBU": "strb",
	}[instruction.Opcode]
	if destination.Base == "SB" {
		if instruction.Suffix != "" {
			return fmt.Errorf("symbolic store does not accept .%s", instruction.Suffix)
		}
		name := t.symbol(destination.Symbol)
		fmt.Fprintf(&t.output, "\tadrp x27, %s\n", name)
		fmt.Fprintf(&t.output, "\t%s %s, [x27, :lo12:%s]\n", mnemonic, sourceRegister, name)
		return nil
	}
	address, err := memoryAddress(destination, instruction.Suffix)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, sourceRegister, address)
	return nil
}

func (t *arm64Translator) translateRegisterPair(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	width := 64
	if strings.HasSuffix(instruction.Opcode, "W") {
		width = 32
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	var pair Operand
	var memory Operand
	if strings.HasPrefix(instruction.Opcode, "LD") {
		memory = instruction.Operands[0]
		pair = instruction.Operands[1]
	} else {
		pair = instruction.Operands[0]
		memory = instruction.Operands[1]
	}
	if pair.Kind != OperandRegisterPair || memory.Kind != OperandMemory || memory.Base == "SB" {
		return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, instruction.Operands[0].Text, instruction.Operands[1].Text)
	}
	first, err := arm64Register(pair.Registers[0], width)
	if err != nil {
		return err
	}
	second, err := arm64Register(pair.Registers[1], width)
	if err != nil {
		return err
	}
	address, err := memoryAddress(memory, instruction.Suffix)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, first, second, address)
	return nil
}

func (t *arm64Translator) translateALU(instruction *Instruction) error {
	if len(instruction.Operands) != 2 && len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires two or three operands", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	mnemonic := aluMnemonic(instruction.Opcode)
	source := instruction.Operands[0]
	destination := instruction.Operands[len(instruction.Operands)-1]
	if destination.Kind != OperandRegister {
		return fmt.Errorf("%s destination must be a register", instruction.Opcode)
	}
	destinationRegister, err := arm64Register(destination.Register, width)
	if err != nil {
		return err
	}
	left := destination
	if len(instruction.Operands) == 3 {
		left = instruction.Operands[1]
	}
	if left.Kind != OperandRegister {
		return fmt.Errorf("%s left operand must be a register", instruction.Opcode)
	}
	leftRegister, err := arm64Register(left.Register, width)
	if err != nil {
		return err
	}
	right, err := formatALUSource(source, width)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destinationRegister, leftRegister, right)
	return nil
}

func (t *arm64Translator) translateNegate(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := formatALUSource(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	mnemonic := "neg"
	if strings.Contains(instruction.Opcode, "S") {
		mnemonic = "negs"
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
	return nil
}

func (t *arm64Translator) translateUnaryRegister(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
	return nil
}

func (t *arm64Translator) translateCompare(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	left, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	right, err := formatALUSource(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, left, right)
	return nil
}

func (t *arm64Translator) translateConditionalCompare(instruction *Instruction) error {
	if len(instruction.Operands) != 4 {
		return fmt.Errorf("%s requires condition, two registers, and flags", instruction.Opcode)
	}
	condition := strings.ToLower(instruction.Operands[0].Text)
	width := instructionWidth(instruction.Opcode)
	left, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	right, err := registerOperand(instruction.Operands[2], width)
	if err != nil {
		return err
	}
	flags := instruction.Operands[3]
	if flags.Kind != OperandImmediate {
		return fmt.Errorf("%s flags must be immediate", instruction.Opcode)
	}
	fmt.Fprintf(&t.output, "\tccmp %s, %s, #%s, %s\n", left, right, flags.Immediate, condition)
	return nil
}

func (t *arm64Translator) translateConditionalSelect(instruction *Instruction) error {
	if len(instruction.Operands) != 4 {
		return fmt.Errorf("%s requires a condition, two sources, and destination", instruction.Opcode)
	}
	condition := strings.ToLower(instruction.Operands[0].Text)
	width := instructionWidth(instruction.Opcode)
	first, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	second, err := registerOperand(instruction.Operands[2], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[3], width)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tcsel %s, %s, %s, %s\n", destination, first, second, condition)
	return nil
}

func (t *arm64Translator) translateConditionalUnary(instruction *Instruction) error {
	if len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires a condition, source, and destination", instruction.Opcode)
	}
	condition := strings.ToLower(instruction.Operands[0].Text)
	width := instructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[2], width)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destination, source, condition)
	return nil
}

func (t *arm64Translator) translateConditionalSet(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a condition and destination", instruction.Opcode)
	}
	condition := strings.ToLower(instruction.Operands[0].Text)
	destination, err := registerOperand(instruction.Operands[1], instructionWidth(instruction.Opcode))
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tcset %s, %s\n", destination, condition)
	return nil
}

func (t *arm64Translator) translateReverse(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
	return nil
}

func (t *arm64Translator) translateBranch(instruction *Instruction) error {
	if len(instruction.Operands) != 1 {
		return fmt.Errorf("%s requires one target", instruction.Opcode)
	}
	mnemonic := branchMnemonic(instruction.Opcode)
	target := t.branchTarget(instruction.Operands[0])
	fmt.Fprintf(&t.output, "\t%s %s\n", mnemonic, target)
	return nil
}

func (t *arm64Translator) translateCompareBranch(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a register and target", instruction.Opcode)
	}
	width := 64
	if strings.HasSuffix(instruction.Opcode, "W") {
		width = 32
	}
	register, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, register, t.branchTarget(instruction.Operands[1]))
	return nil
}

func (t *arm64Translator) translateTestBranch(instruction *Instruction) error {
	if len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires a bit, register, and target", instruction.Opcode)
	}
	bit := instruction.Operands[0]
	if bit.Kind != OperandImmediate {
		return fmt.Errorf("%s bit must be immediate", instruction.Opcode)
	}
	register, err := registerOperand(instruction.Operands[1], 64)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, #%s, %s\n", strings.ToLower(instruction.Opcode), register, bit.Immediate, t.branchTarget(instruction.Operands[2]))
	return nil
}

func (t *arm64Translator) translateDMB(instruction *Instruction) error {
	if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != OperandImmediate {
		return fmt.Errorf("DMB requires one immediate")
	}
	barriers := map[string]string{
		"0xe": "st",
		"14":  "st",
	}
	barrier := barriers[strings.ToLower(instruction.Operands[0].Immediate)]
	if barrier == "" {
		return fmt.Errorf("unsupported DMB option $%s", instruction.Operands[0].Immediate)
	}
	fmt.Fprintf(&t.output, "\tdmb %s\n", barrier)
	return nil
}

func (t *arm64Translator) translateMRS(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("MRS requires a system register and destination")
	}
	destination, err := registerOperand(instruction.Operands[1], 64)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tmrs %s, %s\n", destination, strings.ToLower(instruction.Operands[0].Text))
	return nil
}

func (t *arm64Translator) translateDC(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("DC requires an operation and register")
	}
	register, err := registerOperand(instruction.Operands[1], 64)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tdc %s, %s\n", strings.ToLower(instruction.Operands[0].Text), register)
	return nil
}

func (t *arm64Translator) symbol(symbol Symbol) string {
	name := symbol.Name
	if strings.HasPrefix(name, "·") {
		name = t.options.PackagePath + name
	}
	name = sanitizeSymbol(name)
	if symbol.Static {
		return fmt.Sprintf(".L%s_%s", t.fileTag, name)
	}
	return name
}

func (t *arm64Translator) localLabel(name string) string {
	if label := t.labels[t.functionIndex][name]; label != "" {
		return label
	}
	return sanitizeSymbol(name)
}

func (t *arm64Translator) branchTarget(operand Operand) string {
	if symbol, ok := operandSymbol(operand); ok {
		return t.symbol(symbol)
	}
	return t.localLabel(operand.Text)
}
