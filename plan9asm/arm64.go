package plan9asm

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

type arm64Translator struct {
	options                 ARM64Options
	fileTag                 string
	functionIndex           int
	labels                  map[int]map[string]string
	pcLabels                map[int]map[int]string
	instructionIndex        map[*Instruction]int
	currentInstructionIndex int
	references              map[string]bool
	abi0References          map[string]bool
	abi0Layouts             map[int]abi0Layout
	directABI0              map[int]bool
	abi0Symbols             map[string]bool
	directABI0Symbols       map[string]bool
	functionCalls           map[int]bool
	currentABI0             bool
	currentDirectABI0       bool
	currentABI0Layout       abi0Layout
	currentFrame            int
	currentPlan9Frame       int
	currentLeaf             bool
	currentSymbol           string
	functions               []ARM64Function
	data                    map[string][]arm64DataValue
	lseEnabled              bool
	cryptoEnabled           bool
	sha512Enabled           bool
	crcEnabled              bool
	output                  strings.Builder
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

func (t *arm64Translator) collectPCLabels(file *File) error {
	functionIndex := -1
	instructionIndex := 0
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			functionIndex++
			instructionIndex = 0
		case *Instruction:
			t.instructionIndex[statement] = instructionIndex
			for _, operand := range statement.Operands {
				if operand.Kind != OperandMemory || operand.Base != "PC" || operand.Index != "" {
					continue
				}
				offset, err := t.resolveIntegerExpression(operand.Offset)
				if err != nil {
					return err
				}
				targetIndex := instructionIndex + int(offset)
				if targetIndex < 0 {
					return fmt.Errorf("PC-relative target before function start")
				}
				if t.pcLabels[functionIndex] == nil {
					t.pcLabels[functionIndex] = make(map[int]string)
				}
				t.pcLabels[functionIndex][targetIndex] = fmt.Sprintf(".L%s_%d_pc_%d", t.fileTag, functionIndex, targetIndex)
			}
			instructionIndex++
		}
	}
	return nil
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
		if err := t.emitPCLabelForInstruction(statement); err != nil {
			return err
		}
		defer func() {
			t.currentInstructionIndex++
		}()
		return t.translateInstruction(statement)
	default:
		return fmt.Errorf("unsupported statement %T", statement)
	}
}

func (t *arm64Translator) emitPCLabelForInstruction(instruction *Instruction) error {
	index, ok := t.instructionIndex[instruction]
	if !ok {
		return fmt.Errorf("instruction index not collected")
	}
	t.currentInstructionIndex = index
	label := t.pcLabels[t.functionIndex][index]
	if label != "" {
		fmt.Fprintf(&t.output, "%s:\n", label)
	}
	return nil
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
	switch opcode {
	case "MOVD", "MOVW", "MOVWU", "MOVH", "MOVHU", "MOVB", "MOVBU":
		return t.translateMove(instruction)
	case "FMOVD", "FMOVS":
		return t.translateFloatMove(instruction)
	case "FADDD", "FADDS", "FSUBD", "FSUBS", "FMULD", "FMULS", "FDIVD", "FDIVS", "FMAXD", "FMAXS", "FMIND", "FMINS", "FNMULD", "FNMULS":
		return t.translateFloatBinary(instruction)
	case "FABSD", "FABSS", "FRINTMD", "FRINTMS", "FRINTPD", "FRINTPS", "FRINTZD", "FRINTZS":
		return t.translateFloatUnary(instruction)
	case "FMADDD", "FMADDS", "FMSUBD", "FMSUBS", "FNMSUBD", "FNMSUBS":
		return t.translateFloatFused(instruction)
	case "FCMPD", "FCMPS":
		return t.translateFloatCompare(instruction)
	case "FCSELD", "FCSELS":
		return t.translateFloatConditionalSelect(instruction)
	case "FCVTZSD", "FCVTZSS", "SCVTFD", "SCVTFS":
		return t.translateFloatConversion(instruction)
	case "VMOV":
		return t.translateVectorMove(instruction)
	case "VDUP":
		return t.translateVectorDuplicate(instruction)
	case "VLD1", "VLD1R", "VLD4R":
		return t.translateVectorLoad(instruction)
	case "VST1":
		return t.translateVectorStore(instruction)
	case "VCMEQ", "VAND", "VORR", "VEOR", "VADD", "VADDP":
		return t.translateVectorBinary(instruction)
	case "VREV32", "VREV64":
		return t.translateVectorUnary(instruction)
	case "AESE", "AESMC":
		return t.translateVectorCrypto(instruction)
	case "SHA256H", "SHA256H2", "SHA256SU0", "SHA256SU1":
		return t.translateSHA256(instruction)
	case "SHA1C", "SHA1P", "SHA1M", "SHA1H", "SHA1SU0", "SHA1SU1":
		return t.translateSHA1(instruction)
	case "SHA512H", "SHA512H2", "SHA512SU0", "SHA512SU1":
		return t.translateSHA512(instruction)
	case "CRC32B", "CRC32H", "CRC32W", "CRC32X", "CRC32CB", "CRC32CH", "CRC32CW", "CRC32CX":
		return t.translateCRC32(instruction)
	case "VSHL", "VSRI":
		return t.translateVectorImmediate(instruction)
	case "VEXT":
		return t.translateVectorExtract(instruction)
	case "VTBL":
		return t.translateVectorTable(instruction)
	case "VUADDLV":
		return t.translateVectorAddLongAcross(instruction)
	case "LDP", "LDPW", "STP", "STPW":
		return t.translateRegisterPair(instruction)
	case "FLDPD", "FSTPD":
		return t.translateFloatRegisterPair(instruction)
	case "ADD", "ADDW", "ADDS", "ADDSW", "ADC", "ADCW", "ADCS", "ADCSW", "SUB", "SUBW", "SUBS", "SUBSW", "SBC", "SBCW", "SBCS", "SBCSW", "AND", "ANDW", "ANDS", "ANDSW", "BIC", "BICW", "BICS", "BICSW", "ORR", "ORRW", "EOR", "EORW", "LSL", "LSLW", "LSR", "LSRW", "ASR", "ASRW":
		return t.translateALU(instruction)
	case "MUL", "MULW", "UMULH", "UDIV", "UDIVW":
		return t.translateMultiplyDivide(instruction)
	case "NEG", "NEGW", "NEGS", "NEGSW":
		return t.translateNegate(instruction)
	case "RBIT", "RBITW", "CLZ", "CLZW":
		return t.translateUnaryRegister(instruction)
	case "ROR", "RORW":
		return t.translateRotateRight(instruction)
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
	case "B", "JMP", "BL", "CALL", "BEQ", "BNE", "BCS", "BCC", "BHS", "BLO", "BMI", "BPL", "BVS", "BVC", "BHI", "BLS", "BGE", "BLT", "BGT", "BLE":
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
	case "PRFM":
		if len(instruction.Operands) != 2 || instruction.Operands[0].Kind != OperandMemory {
			return fmt.Errorf("PRFM requires an address and prefetch operation")
		}
		address, err := t.memoryAddress(instruction.Operands[0], instruction.Suffix)
		if err != nil {
			return err
		}
		operation := strings.ToLower(instruction.Operands[1].Text)
		fmt.Fprintf(&t.output, "\tprfm %s, %s\n", operation, address)
		return nil
	case "SVC":
		return t.translateSVC(instruction)
	case "BRK":
		if len(instruction.Operands) == 0 {
			t.output.WriteString("\tbrk #0\n")
			return nil
		}
		if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != OperandImmediate {
			return fmt.Errorf("BRK accepts at most one immediate")
		}
		fmt.Fprintf(&t.output, "\tbrk #%s\n", instruction.Operands[0].Immediate)
		return nil
	case "UNDEF":
		if len(instruction.Operands) != 0 {
			return fmt.Errorf("UNDEF does not accept operands")
		}
		t.output.WriteString("\t.inst 0\n")
		return nil
	case "LDAR", "LDARW", "LDARB", "LDAXR", "LDAXRW", "LDAXRB":
		return t.translateAtomicLoad(instruction)
	case "STLR", "STLRW", "STLRB":
		return t.translateAtomicStore(instruction)
	case "STLXR", "STLXRW", "STLXRB":
		return t.translateAtomicStoreExclusive(instruction)
	case "SWPALB", "SWPALW", "SWPALD", "CASALW", "CASALD", "LDADDALW", "LDADDALD", "LDCLRALB", "LDCLRALW", "LDCLRALD", "LDORALB", "LDORALW", "LDORALD":
		return t.translateLSEAtomic(instruction)
	case "MVN", "MVNW":
		return t.translateBitwiseNot(instruction)
	case "RET":
		if len(instruction.Operands) > 1 {
			return fmt.Errorf("RET accepts at most one target")
		}
		if len(instruction.Operands) == 1 {
			target := instruction.Operands[0]
			if target.Kind != OperandMemory || target.Offset != "" || target.Index != "" {
				return fmt.Errorf("unsupported RET target %q", target.Text)
			}
			register, err := arm64Register(target.Base, 64)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tret %s\n", register)
			return nil
		}
		if t.currentFrame != 0 {
			t.output.WriteString("\tldur x29, [sp, #-8]\n")
			if !t.currentLeaf {
				t.output.WriteString("\tldr x30, [sp]\n")
			}
			if err := t.emitRegisterOffset("sp", "sp", int64(t.currentFrame)); err != nil {
				return err
			}
		}
		t.output.WriteString("\tret\n")
		return nil
	case "NOOP", "NOP":
		if len(instruction.Operands) > 1 {
			return fmt.Errorf("%s accepts at most one operand", instruction.Opcode)
		}
		t.output.WriteString("\tnop\n")
		return nil
	case "WORD":
		if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != OperandImmediate {
			return fmt.Errorf("WORD requires one immediate operand")
		}
		value, err := t.resolveIntegerExpression(instruction.Operands[0].Immediate)
		if err != nil {
			return err
		}
		if value < 0 || value > 0xffffffff {
			return fmt.Errorf("WORD value %#x does not fit in 32 bits", value)
		}
		fmt.Fprintf(&t.output, "\t.inst 0x%08x\n", uint32(value))
		return nil
	default:
		return fmt.Errorf("unsupported ARM64 instruction %s", opcode)
	}
}

func (t *arm64Translator) translateFloatMove(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	width := 64
	if instruction.Opcode == "FMOVS" {
		width = 32
	}
	if source.Kind == OperandRegister && source.Register == "ZR" && destination.Kind == OperandRegister {
		destinationRegister, err := arm64FloatRegister(destination.Register, width)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destinationRegister, arm64ZeroRegister(width))
		return nil
	}
	if source.Kind == OperandImmediate && destination.Kind == OperandRegister {
		destinationRegister, err := arm64FloatRegister(destination.Register, width)
		if err != nil {
			return err
		}
		value, err := strconv.ParseFloat(source.Immediate, width)
		if err != nil {
			return fmt.Errorf("invalid floating-point immediate $%s: %w", source.Immediate, err)
		}
		integerRegister := "x27"
		bits := uint64(math.Float64bits(value))
		if width == 32 {
			integerRegister = "w27"
			bits = uint64(math.Float32bits(float32(value)))
		}
		if err := t.emitMoveImmediate(integerRegister, width, strconv.FormatUint(bits, 10)); err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destinationRegister, integerRegister)
		return nil
	}
	if source.Kind == OperandRegister && destination.Kind == OperandRegister {
		sourceFloat := isARM64FloatRegister(source.Register)
		destinationFloat := isARM64FloatRegister(destination.Register)
		switch {
		case sourceFloat && destinationFloat:
			sourceRegister, err := arm64FloatRegister(source.Register, width)
			if err != nil {
				return err
			}
			destinationRegister, err := arm64FloatRegister(destination.Register, width)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destinationRegister, sourceRegister)
			return nil
		case sourceFloat:
			sourceRegister, err := arm64FloatRegister(source.Register, width)
			if err != nil {
				return err
			}
			destinationRegister, err := arm64Register(destination.Register, width)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destinationRegister, sourceRegister)
			return nil
		case destinationFloat:
			sourceRegister, err := arm64Register(source.Register, width)
			if err != nil {
				return err
			}
			destinationRegister, err := arm64FloatRegister(destination.Register, width)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tfmov %s, %s\n", destinationRegister, sourceRegister)
			return nil
		}
	}
	if source.Kind == OperandMemory && destination.Kind == OperandRegister {
		destinationRegister, err := arm64FloatRegister(destination.Register, width)
		if err != nil {
			return err
		}
		if source.Base == "FP" && t.currentDirectABI0 {
			return t.emitDirectABI0FloatLoad(source, destinationRegister, width)
		}
		var address string
		if source.Base == "FP" && t.currentABI0 {
			address, err = t.abi0FrameAddress(source, instruction.Suffix)
		} else {
			address, err = t.memoryAddress(source, instruction.Suffix)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tldr %s, %s\n", destinationRegister, address)
		return nil
	}
	if source.Kind == OperandRegister && destination.Kind == OperandMemory {
		sourceRegister, err := arm64FloatRegister(source.Register, width)
		if err != nil {
			return err
		}
		if destination.Base == "FP" && t.currentDirectABI0 {
			return t.emitDirectABI0FloatStore(sourceRegister, destination, width)
		}
		var address string
		if destination.Base == "FP" && t.currentABI0 {
			address, err = t.abi0FrameAddressAvoiding(destination, instruction.Suffix)
		} else {
			address, err = t.memoryAddress(destination, instruction.Suffix)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\tstr %s, %s\n", sourceRegister, address)
		return nil
	}
	return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, source.Text, destination.Text)
}

func isARM64FloatRegister(register string) bool {
	register = strings.ToUpper(register)
	if !strings.HasPrefix(register, "F") {
		return false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(register, "F"))
	return err == nil && number >= 0 && number <= 31
}

func (t *arm64Translator) translateFloatBinary(instruction *Instruction) error {
	if len(instruction.Operands) != 2 && len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires two or three operands", instruction.Opcode)
	}
	width := floatInstructionWidth(instruction.Opcode)
	source, err := arm64FloatOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destinationOperand := instruction.Operands[len(instruction.Operands)-1]
	destination, err := arm64FloatOperand(destinationOperand, width)
	if err != nil {
		return err
	}
	left := destination
	if len(instruction.Operands) == 3 {
		left, err = arm64FloatOperand(instruction.Operands[1], width)
		if err != nil {
			return err
		}
	}
	mnemonic := floatInstructionMnemonic(instruction.Opcode)
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destination, left, source)
	return nil
}

func (t *arm64Translator) translateFloatUnary(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	width := floatInstructionWidth(instruction.Opcode)
	source, err := arm64FloatOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destination, err := arm64FloatOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	mnemonic := floatInstructionMnemonic(instruction.Opcode)
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
	return nil
}

func (t *arm64Translator) translateFloatFused(instruction *Instruction) error {
	if len(instruction.Operands) != 4 {
		return fmt.Errorf("%s requires four operands", instruction.Opcode)
	}
	width := floatInstructionWidth(instruction.Opcode)
	multiplier, err := arm64FloatOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	addend, err := arm64FloatOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	multiplicand, err := arm64FloatOperand(instruction.Operands[2], width)
	if err != nil {
		return err
	}
	destination, err := arm64FloatOperand(instruction.Operands[3], width)
	if err != nil {
		return err
	}
	mnemonic := floatInstructionMnemonic(instruction.Opcode)
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s, %s\n", mnemonic, destination, multiplicand, multiplier, addend)
	return nil
}

func (t *arm64Translator) translateFloatCompare(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	width := floatInstructionWidth(instruction.Opcode)
	left, err := arm64FloatOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	rightOperand := instruction.Operands[0]
	if rightOperand.Kind == OperandImmediate {
		value, err := strconv.ParseFloat(rightOperand.Immediate, width)
		if err != nil {
			return fmt.Errorf("invalid floating-point immediate $%s: %w", rightOperand.Immediate, err)
		}
		if value != 0 {
			return fmt.Errorf("%s only supports an immediate comparison with zero", instruction.Opcode)
		}
		fmt.Fprintf(&t.output, "\tfcmp %s, #0.0\n", left)
		return nil
	}
	right, err := arm64FloatOperand(rightOperand, width)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tfcmp %s, %s\n", left, right)
	return nil
}

func (t *arm64Translator) translateFloatConditionalSelect(instruction *Instruction) error {
	if len(instruction.Operands) != 4 {
		return fmt.Errorf("%s requires a condition, two sources, and destination", instruction.Opcode)
	}
	width := floatInstructionWidth(instruction.Opcode)
	first, err := arm64FloatOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	second, err := arm64FloatOperand(instruction.Operands[2], width)
	if err != nil {
		return err
	}
	destination, err := arm64FloatOperand(instruction.Operands[3], width)
	if err != nil {
		return err
	}
	condition := strings.ToLower(instruction.Operands[0].Text)
	fmt.Fprintf(&t.output, "\tfcsel %s, %s, %s, %s\n", destination, first, second, condition)
	return nil
}

func (t *arm64Translator) translateFloatConversion(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	width := floatInstructionWidth(instruction.Opcode)
	mnemonic := floatInstructionMnemonic(instruction.Opcode)
	if strings.HasPrefix(instruction.Opcode, "FCVT") {
		source, err := arm64FloatOperand(instruction.Operands[0], width)
		if err != nil {
			return err
		}
		destination, err := registerOperand(instruction.Operands[1], 64)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
		return nil
	}
	source, err := registerOperand(instruction.Operands[0], 64)
	if err != nil {
		return err
	}
	destination, err := arm64FloatOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, source)
	return nil
}

func floatInstructionWidth(opcode string) int {
	if strings.HasSuffix(opcode, "S") {
		return 32
	}
	return 64
}

func floatInstructionMnemonic(opcode string) string {
	return strings.ToLower(opcode[:len(opcode)-1])
}

func arm64FloatOperand(operand Operand, width int) (string, error) {
	if operand.Kind != OperandRegister {
		return "", fmt.Errorf("operand %q must be a floating-point register", operand.Text)
	}
	return arm64FloatRegister(operand.Register, width)
}

func arm64FloatRegister(register string, width int) (string, error) {
	register = strings.ToUpper(register)
	if !strings.HasPrefix(register, "F") {
		return "", fmt.Errorf("unsupported ARM64 floating-point register %s", register)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(register, "F"))
	if err != nil || number < 0 || number > 31 {
		return "", fmt.Errorf("unsupported ARM64 floating-point register %s", register)
	}
	prefix := "d"
	if width == 32 {
		prefix = "s"
	}
	return prefix + strconv.Itoa(number), nil
}

func arm64ZeroRegister(width int) string {
	if width == 32 {
		return "wzr"
	}
	return "xzr"
}

func (t *arm64Translator) translateMove(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if instruction.Opcode == "MOVD" {
		if systemRegister(source) && destination.Kind == OperandRegister {
			destinationRegister, err := arm64Register(destination.Register, 64)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tmrs %s, %s\n", destinationRegister, strings.ToLower(source.Register))
			return nil
		}
		if source.Kind == OperandRegister && systemRegister(destination) {
			sourceRegister, err := arm64Register(source.Register, 64)
			if err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\tmsr %s, %s\n", strings.ToLower(destination.Register), sourceRegister)
			return nil
		}
	}
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
				name, offset, err := t.symbolAddress(symbolOperand)
				if err != nil {
					return err
				}
				t.references[name] = true
				target := t.assemblyAddressSymbol(name)
				relocation := symbolRelocation(target, offset)
				fmt.Fprintf(&t.output, "\tadrp %s, %s\n", destinationRegister, target)
				fmt.Fprintf(&t.output, "\tadd %s, %s, :lo12:%s\n", destinationRegister, destinationRegister, relocation)
				return nil
			}
			if frameOperand := parseOperand(source.Immediate); frameOperand.Kind == OperandMemory && frameOperand.Base == "FP" {
				if t.currentDirectABI0 {
					return fmt.Errorf("direct ABI0 function cannot take the address of frame operand %q", source.Text)
				}
				offset, err := namedFrameOffset(frameOperand.Offset)
				if err != nil {
					return err
				}
				addressOffset := t.currentFrame + 8 + offset
				fmt.Fprintf(&t.output, "\tadd %s, sp, #%d\n", destinationRegister, addressOffset)
				return nil
			}
			if addressOperand := parseOperand(source.Immediate); addressOperand.Kind == OperandMemory && addressOperand.Index == "" {
				return t.emitAddressImmediate(destinationRegister, addressOperand)
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
	if source.Kind == OperandImmediate && destination.Kind == OperandMemory {
		width := storeWidth(instruction.Opcode)
		value, err := t.resolveIntegerExpression(source.Immediate)
		if err != nil {
			return err
		}
		sourceRegister := "xzr"
		if width == 32 {
			sourceRegister = "wzr"
		}
		if value != 0 {
			sourceRegister = "x27"
			if width == 32 {
				sourceRegister = "w27"
			}
			if err := t.emitMoveImmediate(sourceRegister, width, source.Immediate); err != nil {
				return err
			}
		}
		return t.emitStore(instruction, sourceRegister, destination)
	}
	return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, source.Text, destination.Text)
}

func (t *arm64Translator) assemblyAddressSymbol(name string) string {
	if !t.abi0Symbols[name] {
		return name
	}
	t.abi0References[name] = true
	if t.directABI0Symbols[name] {
		return name
	}
	return abi0Symbol(name)
}

func (t *arm64Translator) emitAddressImmediate(destination string, address Operand) error {
	base, err := arm64Register(address.Base, 64)
	if err != nil {
		return err
	}
	offset, err := t.resolveIntegerExpression(address.Offset)
	if err != nil {
		return fmt.Errorf("invalid address offset %q: %w", address.Offset, err)
	}
	return t.emitRegisterOffset(destination, base, offset)
}

func (t *arm64Translator) emitRegisterOffset(destination, base string, offset int64) error {
	if offset == 0 {
		fmt.Fprintf(&t.output, "\tmov %s, %s\n", destination, base)
		return nil
	}
	mnemonic := "add"
	magnitude := offset
	if magnitude < 0 {
		mnemonic = "sub"
		magnitude = -magnitude
	}
	if magnitude <= 4095 {
		fmt.Fprintf(&t.output, "\t%s %s, %s, #%d\n", mnemonic, destination, base, magnitude)
		return nil
	}
	if magnitude%4096 == 0 && magnitude/4096 <= 4095 {
		fmt.Fprintf(&t.output, "\t%s %s, %s, #%d, lsl #12\n", mnemonic, destination, base, magnitude/4096)
		return nil
	}
	scratch := "x27"
	if destination == scratch || base == scratch {
		scratch = "x16"
	}
	if err := t.emitMoveImmediate(scratch, 64, strconv.FormatInt(magnitude, 10)); err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destination, base, scratch)
	return nil
}

func systemRegister(operand Operand) bool {
	return operand.Kind == OperandRegister && (operand.Register == "NZCV" || operand.Register == "FPSR")
}

func (t *arm64Translator) emitMoveImmediate(destination string, width int, immediate string) error {
	resolved, err := t.resolveIntegerExpression(immediate)
	if err != nil {
		return fmt.Errorf("invalid integer immediate $%s: %w", immediate, err)
	}
	normalized := strconv.FormatInt(resolved, 10)
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
		address, err = t.abi0FrameAddressAvoiding(destination, instruction.Suffix, sourceRegister)
	} else {
		address, err = t.memoryAddressAvoiding(destination, instruction.Suffix, sourceRegister)
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
	var address string
	if strings.HasPrefix(instruction.Opcode, "ST") {
		address, err = t.memoryAddressAvoiding(memory, instruction.Suffix, first, second)
	} else {
		address, err = t.memoryAddress(memory, instruction.Suffix)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, first, second, address)
	return nil
}

func (t *arm64Translator) translateFloatRegisterPair(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	var pair Operand
	var memory Operand
	if instruction.Opcode == "FLDPD" {
		memory = instruction.Operands[0]
		pair = instruction.Operands[1]
	} else {
		pair = instruction.Operands[0]
		memory = instruction.Operands[1]
	}
	if pair.Kind != OperandRegisterPair || memory.Kind != OperandMemory || memory.Base == "SB" {
		return fmt.Errorf("unsupported %s operands %q, %q", instruction.Opcode, instruction.Operands[0].Text, instruction.Operands[1].Text)
	}
	first, err := arm64FloatRegister(pair.Registers[0], 64)
	if err != nil {
		return err
	}
	second, err := arm64FloatRegister(pair.Registers[1], 64)
	if err != nil {
		return err
	}
	address, err := t.memoryAddress(memory, instruction.Suffix)
	if err != nil {
		return err
	}
	mnemonic := "stp"
	if instruction.Opcode == "FLDPD" {
		mnemonic = "ldp"
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, first, second, address)
	return nil
}

func (t *arm64Translator) translateALU(instruction *Instruction) error {
	if len(instruction.Operands) != 2 && len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires two or three operands", instruction.Opcode)
	}
	if t.isRuntimeSystemstackFrameRestore(instruction) {
		// systemstack's noswitch path normally reconstructs the caller frame
		// pointer as callerSP-8. cg12's managed AAPCS frame keeps its frame
		// record above the outgoing argument area, so restore the value saved by
		// this assembly function's translated prologue instead. The preceding
		// post-indexed load has already advanced SP by 16 bytes.
		t.output.WriteString("\tldur x29, [sp, #-24]\n")
		return nil
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
	if source.Kind == OperandImmediate && (mnemonic == "add" || mnemonic == "adds" || mnemonic == "sub" || mnemonic == "subs") {
		value, err := t.resolveIntegerExpression(source.Immediate)
		if err != nil {
			return err
		}
		if !arm64AddSubImmediate(value) {
			scratch := "x27"
			if width == 32 {
				scratch = "w27"
			}
			if leftRegister == scratch || destinationRegister == scratch {
				return fmt.Errorf("%s with a large immediate cannot use reserved register R27", instruction.Opcode)
			}
			if err := t.emitMoveImmediate(scratch, width, source.Immediate); err != nil {
				return err
			}
			fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destinationRegister, leftRegister, scratch)
			return nil
		}
	}
	right, err := t.formatALUSource(source, width)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destinationRegister, leftRegister, right)
	return nil
}

func (t *arm64Translator) isRuntimeSystemstackFrameRestore(instruction *Instruction) bool {
	if t.options.PackagePath != "runtime" || filepath.Base(t.options.Filename) != "asm_arm64.s" {
		return false
	}
	if t.currentSymbol != "runtime_systemstack" || instruction.Opcode != "SUB" {
		return false
	}
	if len(instruction.Operands) != 3 {
		return false
	}
	source := instruction.Operands[0]
	left := instruction.Operands[1]
	destination := instruction.Operands[2]
	if source.Kind != OperandImmediate || left.Kind != OperandRegister || destination.Kind != OperandRegister {
		return false
	}
	value, err := t.resolveIntegerExpression(source.Immediate)
	if err != nil || value != 8 {
		return false
	}
	return left.Register == "RSP" && destination.Register == "R29"
}

func arm64AddSubImmediate(value int64) bool {
	if value < 0 {
		return false
	}
	if value <= 4095 {
		return true
	}
	return value%4096 == 0 && value/4096 <= 4095
}

func (t *arm64Translator) translateMultiplyDivide(instruction *Instruction) error {
	if len(instruction.Operands) != 2 && len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires two or three operands", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destinationOperand := instruction.Operands[len(instruction.Operands)-1]
	destination, err := registerOperand(destinationOperand, width)
	if err != nil {
		return err
	}
	left := destination
	if len(instruction.Operands) == 3 {
		left, err = registerOperand(instruction.Operands[1], width)
		if err != nil {
			return err
		}
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(instruction.Opcode, "W"))
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, destination, left, source)
	return nil
}

func (t *arm64Translator) translateNegate(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires two operands", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := t.formatALUSource(instruction.Operands[0], width)
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

func (t *arm64Translator) translateRotateRight(instruction *Instruction) error {
	if len(instruction.Operands) != 2 && len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires an immediate, optional source, and destination", instruction.Opcode)
	}
	immediate := instruction.Operands[0]
	if immediate.Kind != OperandImmediate {
		return fmt.Errorf("%s rotation must be immediate", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	destination, err := registerOperand(instruction.Operands[len(instruction.Operands)-1], width)
	if err != nil {
		return err
	}
	source := destination
	if len(instruction.Operands) == 3 {
		source, err = registerOperand(instruction.Operands[1], width)
		if err != nil {
			return err
		}
	}
	amount, err := t.resolveIntegerExpression(immediate.Immediate)
	if err != nil {
		return err
	}
	if amount < 0 || amount >= int64(width) {
		return fmt.Errorf("%s rotation %d is outside the %d-bit register", instruction.Opcode, amount, width)
	}
	fmt.Fprintf(&t.output, "\tror %s, %s, #%d\n", destination, source, amount)
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
	right, err := t.formatALUSource(instruction.Operands[0], width)
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
	target, indirect, err := t.branchTarget(instruction.Operands[0])
	if err != nil {
		return err
	}
	if indirect {
		if instruction.Opcode == "BL" || instruction.Opcode == "CALL" {
			mnemonic = "blr"
		} else if instruction.Opcode == "B" || instruction.Opcode == "JMP" {
			mnemonic = "br"
		} else {
			return fmt.Errorf("%s does not accept an indirect target", instruction.Opcode)
		}
	}
	if symbol, ok := operandSymbol(instruction.Operands[0]); ok && !symbol.Static && symbol.ABI == "" {
		name := t.symbol(symbol)
		t.abi0References[name] = true
		if !t.directABI0Symbols[name] {
			target = abi0Symbol(name)
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
	target, indirect, err := t.branchTarget(instruction.Operands[1])
	if err != nil {
		return err
	}
	if indirect {
		return fmt.Errorf("%s does not accept an indirect target", instruction.Opcode)
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, register, target)
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
	target, indirect, err := t.branchTarget(instruction.Operands[2])
	if err != nil {
		return err
	}
	if indirect {
		return fmt.Errorf("%s does not accept an indirect target", instruction.Opcode)
	}
	fmt.Fprintf(&t.output, "\t%s %s, #%s, %s\n", strings.ToLower(instruction.Opcode), register, bit.Immediate, target)
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

func (t *arm64Translator) branchTarget(operand Operand) (string, bool, error) {
	if symbol, ok := operandSymbol(operand); ok {
		return t.symbol(symbol), false, nil
	}
	if operand.Kind == OperandRaw {
		return t.localLabel(operand.Text), false, nil
	}
	if operand.Kind == OperandRegister {
		register, err := arm64Register(operand.Register, 64)
		return register, true, err
	}
	if operand.Kind == OperandMemory && operand.Offset == "" && operand.Index == "" {
		register, err := arm64Register(operand.Base, 64)
		return register, true, err
	}
	if operand.Kind == OperandMemory && operand.Base == "PC" && operand.Index == "" {
		offset, err := t.resolveIntegerExpression(operand.Offset)
		if err != nil {
			return "", false, err
		}
		targetIndex := t.currentInstructionIndex + int(offset)
		label := t.pcLabels[t.functionIndex][targetIndex]
		if label == "" {
			return "", false, fmt.Errorf("PC-relative target %d has no label", targetIndex)
		}
		return label, false, nil
	}
	return "", false, fmt.Errorf("unsupported branch target %q", operand.Text)
}
