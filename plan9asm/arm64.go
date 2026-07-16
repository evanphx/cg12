package plan9asm

import (
	"fmt"
	"strconv"
	"strings"
)

type arm64Translator struct {
	options           ARM64Options
	fileTag           string
	functionIndex     int
	labels            map[int]map[string]string
	references        map[string]bool
	abi0Layouts       map[int]abi0Layout
	directABI0        map[int]bool
	currentABI0       bool
	currentDirectABI0 bool
	currentABI0Layout abi0Layout
	currentFrame      int
	functions         []ARM64Function
	data              map[string][]arm64DataValue
	lseEnabled        bool
	output            strings.Builder
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
		if operand.Kind == OperandMemory && operand.Base == "SB" {
			name, _, err := t.symbolAddress(operand)
			if err != nil {
				return err
			}
			t.references[name] = true
			continue
		}
		symbol, ok := operandSymbol(operand)
		if ok && !symbol.Static {
			t.references[t.symbol(symbol)] = true
		}
	}
	opcode := instruction.Opcode
	if t.currentFrame != 0 && opcode == "BL" {
		return fmt.Errorf("calls from framed Plan 9 assembly are not supported yet")
	}
	switch opcode {
	case "MOVD", "MOVW", "MOVWU", "MOVH", "MOVHU", "MOVB", "MOVBU":
		return t.translateMove(instruction)
	case "FMOVD":
		return t.translateFloatMove(instruction)
	case "VMOV":
		return t.translateVectorMove(instruction)
	case "VLD1", "VLD1R", "VLD4R":
		return t.translateVectorLoad(instruction)
	case "VST1":
		return t.translateVectorStore(instruction)
	case "VCMEQ", "VAND", "VORR", "VEOR", "VADD", "VADDP":
		return t.translateVectorBinary(instruction)
	case "VREV32":
		return t.translateVectorUnary(instruction)
	case "VSHL", "VSRI":
		return t.translateVectorImmediate(instruction)
	case "VTBL":
		return t.translateVectorTable(instruction)
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
	case "UBFX", "UBFXW", "SBFX", "SBFXW":
		return t.translateBitfieldExtract(instruction)
	case "B", "JMP", "BL", "BEQ", "BNE", "BCS", "BCC", "BHS", "BLO", "BMI", "BPL", "BVS", "BVC", "BHI", "BLS", "BGE", "BLT", "BGT", "BLE":
		return t.translateBranch(instruction)
	case "CBZ", "CBZW", "CBNZ", "CBNZW":
		return t.translateCompareBranch(instruction)
	case "TBZ", "TBNZ":
		return t.translateTestBranch(instruction)
	case "DMB":
		return t.translateDMB(instruction)
	case "DSB", "ISB":
		return t.translateBarrier(instruction)
	case "MRS":
		return t.translateMRS(instruction)
	case "MSR":
		return t.translateMSR(instruction)
	case "DC":
		return t.translateDC(instruction)
	case "SVC":
		return t.translateSVC(instruction)
	case "LDAR", "LDARW", "LDARB", "LDAXR", "LDAXRW", "LDAXRB":
		return t.translateAtomicLoad(instruction)
	case "STLR", "STLRW", "STLRB":
		return t.translateAtomicStore(instruction)
	case "STLXR", "STLXRW", "STLXRB":
		return t.translateAtomicStoreExclusive(instruction)
	case "SWPALB", "SWPALW", "SWPALD", "CASALW", "CASALD", "LDADDALW", "LDADDALD", "LDCLRALB", "LDCLRALW", "LDCLRALD", "LDORALB", "LDORALW", "LDORALD":
		return t.translateLSEAtomic(instruction)
	case "MVN":
		return t.translateBitwiseNot(instruction)
	case "RET":
		if len(instruction.Operands) != 0 {
			return fmt.Errorf("RET operands are not supported yet")
		}
		if t.currentFrame != 0 {
			fmt.Fprintf(&t.output, "\tadd x29, sp, #%d\n", t.currentFrame-8)
			fmt.Fprintf(&t.output, "\tadd sp, sp, #%d\n", t.currentFrame)
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

func (t *arm64Translator) translateFloatMove(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("FMOVD requires a source and destination")
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind != OperandRegister || source.Register != "ZR" || destination.Kind != OperandRegister || !strings.HasPrefix(destination.Register, "F") {
		return fmt.Errorf("unsupported FMOVD operands %q, %q", source.Text, destination.Text)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(destination.Register, "F"))
	if err != nil || number < 0 || number > 31 {
		return fmt.Errorf("unsupported floating-point register %s", destination.Register)
	}
	fmt.Fprintf(&t.output, "\tfmov d%d, xzr\n", number)
	return nil
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
			if symbolOperand := parseOperand(source.Immediate); symbolOperand.Kind == OperandMemory && symbolOperand.Base == "SB" {
				name := t.symbol(symbolOperand.Symbol)
				t.references[name] = true
				fmt.Fprintf(&t.output, "\tadrp %s, %s\n", destinationRegister, name)
				fmt.Fprintf(&t.output, "\tadd %s, %s, :lo12:%s\n", destinationRegister, destinationRegister, name)
				return nil
			}
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
		name, offset, err := t.symbolAddress(source)
		if err != nil {
			return err
		}
		relocation := symbolRelocation(name, offset)
		fmt.Fprintf(&t.output, "\tadrp x27, %s\n", name)
		fmt.Fprintf(&t.output, "\t%s %s, [x27, :lo12:%s]\n", mnemonic, destination, relocation)
		return nil
	}
	var address string
	var err error
	if source.Base == "FP" && t.currentDirectABI0 {
		return t.emitDirectABI0Load(instruction, source, destination)
	} else if source.Base == "FP" && t.currentABI0 {
		address, err = t.abi0FrameAddress(source, instruction.Suffix)
	} else {
		address, err = t.memoryAddress(source, instruction.Suffix)
	}
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
		name, offset, err := t.symbolAddress(destination)
		if err != nil {
			return err
		}
		relocation := symbolRelocation(name, offset)
		fmt.Fprintf(&t.output, "\tadrp x27, %s\n", name)
		fmt.Fprintf(&t.output, "\t%s %s, [x27, :lo12:%s]\n", mnemonic, sourceRegister, relocation)
		return nil
	}
	var address string
	var err error
	if destination.Base == "FP" && t.currentDirectABI0 {
		return t.emitDirectABI0Store(instruction, sourceRegister, destination)
	} else if destination.Base == "FP" && t.currentABI0 {
		address, err = t.abi0FrameAddress(destination, instruction.Suffix)
	} else {
		address, err = t.memoryAddress(destination, instruction.Suffix)
	}
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
	address, err := t.memoryAddress(memory, instruction.Suffix)
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

func (t *arm64Translator) translateBitfieldExtract(instruction *Instruction) error {
	if len(instruction.Operands) != 4 {
		return fmt.Errorf("%s requires a bit offset, source, width, and destination", instruction.Opcode)
	}
	bitOffset := instruction.Operands[0]
	bitWidth := instruction.Operands[2]
	if bitOffset.Kind != OperandImmediate || bitWidth.Kind != OperandImmediate {
		return fmt.Errorf("%s bit offset and width must be immediate", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[3], width)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s, #%s, #%s\n", mnemonic, destination, source, bitOffset.Immediate, bitWidth.Immediate)
	return nil
}

func (t *arm64Translator) translateBranch(instruction *Instruction) error {
	if len(instruction.Operands) != 1 {
		return fmt.Errorf("%s requires one target", instruction.Opcode)
	}
	mnemonic := branchMnemonic(instruction.Opcode)
	target := t.branchTarget(instruction.Operands[0])
	if t.currentABI0 && !t.currentDirectABI0 && (instruction.Opcode == "B" || instruction.Opcode == "JMP") {
		if symbol, ok := operandSymbol(instruction.Operands[0]); ok && !symbol.Static && symbol.ABI == "" {
			target = abi0Symbol(t.symbol(symbol))
		}
	}
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

func (t *arm64Translator) translateBarrier(instruction *Instruction) error {
	if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != OperandImmediate {
		return fmt.Errorf("%s requires one immediate", instruction.Opcode)
	}
	options := map[string]string{
		"2":   "oshst",
		"0x2": "oshst",
		"3":   "osh",
		"0x3": "osh",
		"6":   "nshst",
		"0x6": "nshst",
		"7":   "nsh",
		"0x7": "nsh",
		"10":  "ishst",
		"0xa": "ishst",
		"11":  "ish",
		"0xb": "ish",
		"14":  "st",
		"0xe": "st",
		"15":  "sy",
		"0xf": "sy",
	}
	option := options[strings.ToLower(instruction.Operands[0].Immediate)]
	if option == "" {
		return fmt.Errorf("unsupported %s option $%s", instruction.Opcode, instruction.Operands[0].Immediate)
	}
	fmt.Fprintf(&t.output, "\t%s %s\n", strings.ToLower(instruction.Opcode), option)
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
	systemRegister := strings.ToLower(instruction.Operands[0].Text)
	if systemRegister == "dit" {
		t.output.WriteString("\t.arch armv8.4-a\n")
	}
	fmt.Fprintf(&t.output, "\tmrs %s, %s\n", destination, systemRegister)
	return nil
}

func (t *arm64Translator) translateMSR(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("MSR requires a source and system register")
	}
	systemRegister := strings.ToLower(instruction.Operands[1].Text)
	if systemRegister == "dit" {
		t.output.WriteString("\t.arch armv8.4-a\n")
	}
	source := instruction.Operands[0]
	switch source.Kind {
	case OperandImmediate:
		fmt.Fprintf(&t.output, "\tmsr %s, #%s\n", systemRegister, normalizeImmediate(source.Immediate))
		return nil
	case OperandRegister:
		register, err := registerOperand(source, 64)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tmsr %s, %s\n", systemRegister, register)
		return nil
	default:
		return fmt.Errorf("unsupported MSR source %q", source.Text)
	}
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

func (t *arm64Translator) translateSVC(instruction *Instruction) error {
	if len(instruction.Operands) == 0 {
		t.output.WriteString("\tsvc #0\n")
		return nil
	}
	if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != OperandImmediate {
		return fmt.Errorf("SVC accepts at most one immediate")
	}
	fmt.Fprintf(&t.output, "\tsvc #%s\n", instruction.Operands[0].Immediate)
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
