package plan9asm

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// ARM64Options supplies the package and file identity needed to resolve Go's
// package-local and file-local assembly symbols.
type ARM64Options struct {
	PackagePath string
	Filename    string
}

// TranslateARM64 converts a parsed Plan 9 ARM64 source file to GNU assembler
// syntax. It intentionally rejects constructs it cannot translate rather than
// silently emitting assembly with different semantics.
func TranslateARM64(file *File, options ARM64Options) (string, error) {
	translator := arm64Translator{
		options: options,
		fileTag: sanitizeSymbol(strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))),
		labels:  make(map[int]map[string]string),
	}
	translator.collectLabels(file)

	for _, statement := range file.Statements {
		if err := translator.translate(statement); err != nil {
			return "", fmt.Errorf("%s:%d: %w", options.Filename, statement.Position().Line, err)
		}
	}
	return translator.output.String(), nil
}

// ARM64ExternalReferences returns the non-file-local symbols referenced by
// instructions in a parsed source file. An external object emitter uses this
// set to give package-private Go definitions linker-visible binding when an
// assembly file refers to them.
func ARM64ExternalReferences(file *File, options ARM64Options) []string {
	translator := arm64Translator{
		options: options,
		fileTag: sanitizeSymbol(strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))),
	}
	references := make(map[string]bool)
	for _, statement := range file.Statements {
		instruction, ok := statement.(*Instruction)
		if !ok {
			continue
		}
		for _, operand := range instruction.Operands {
			symbol, ok := operandSymbol(operand)
			if ok && !symbol.Static {
				references[translator.symbol(symbol)] = true
			}
		}
	}
	result := make([]string, 0, len(references))
	for symbol := range references {
		result = append(result, symbol)
	}
	return result
}

type arm64Translator struct {
	options       ARM64Options
	fileTag       string
	functionIndex int
	labels        map[int]map[string]string
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

func (t *arm64Translator) translateText(text *Text) error {
	t.functionIndex++
	if text.Frame != "" && text.Frame != "0" {
		return fmt.Errorf("TEXT frame $%s is not supported yet", text.Frame)
	}
	symbol := t.symbol(text.Symbol)
	t.output.WriteString("\n.text\n")
	if !text.Symbol.Static {
		fmt.Fprintf(&t.output, ".global %s\n", symbol)
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

func (t *arm64Translator) translateInstruction(instruction *Instruction) error {
	opcode := instruction.Opcode
	switch opcode {
	case "MOVD", "MOVW", "MOVWU", "MOVH", "MOVHU", "MOVB", "MOVBU":
		return t.translateMove(instruction)
	case "LDP", "LDPW", "STP", "STPW":
		return t.translateRegisterPair(instruction)
	case "ADD", "ADDW", "ADDS", "ADDSW", "SUB", "SUBW", "SUBS", "SUBSW", "AND", "ANDW", "ANDS", "ANDSW", "BIC", "BICW", "BICS", "BICSW", "ORR", "ORRW", "EOR", "EORW", "LSL", "LSLW", "LSR", "LSRW", "ASR", "ASRW":
		return t.translateALU(instruction)
	case "NEG", "NEGW", "NEGS", "NEGSW":
		return t.translateNegate(instruction)
	case "CMP", "CMPW", "CMN", "CMNW", "TST", "TSTW":
		return t.translateCompare(instruction)
	case "CCMP", "CCMPW":
		return t.translateConditionalCompare(instruction)
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
			fmt.Fprintf(&t.output, "\tmov %s, #%s\n", destinationRegister, normalizeImmediate(source.Immediate))
			return nil
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
	source, err := registerOperand(instruction.Operands[0], width)
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

func memoryAddress(operand Operand, suffix string) (string, error) {
	if operand.Kind != OperandMemory || operand.Base == "SB" || operand.Base == "FP" {
		return "", fmt.Errorf("unsupported memory operand %q", operand.Text)
	}
	base, err := arm64Register(operand.Base, 64)
	if err != nil {
		return "", err
	}
	offset := operand.Offset
	if offset == "" {
		offset = "0"
	}
	switch suffix {
	case "":
		if offset == "0" {
			return fmt.Sprintf("[%s]", base), nil
		}
		return fmt.Sprintf("[%s, #%s]", base, offset), nil
	case "P":
		return fmt.Sprintf("[%s], #%s", base, offset), nil
	case "W":
		return fmt.Sprintf("[%s, #%s]!", base, offset), nil
	default:
		return "", fmt.Errorf("unsupported addressing suffix .%s", suffix)
	}
}

func formatALUSource(operand Operand, width int) (string, error) {
	switch operand.Kind {
	case OperandRegister:
		return arm64Register(operand.Register, width)
	case OperandImmediate:
		return "#" + normalizeImmediate(operand.Immediate), nil
	case OperandShiftedRegister:
		register, err := arm64Register(operand.Register, width)
		if err != nil {
			return "", err
		}
		shift := map[string]string{"<<": "lsl", ">>": "lsr", "->": "asr", "@>": "ror"}[operand.Shift]
		return fmt.Sprintf("%s, %s #%s", register, shift, operand.ShiftAmount), nil
	default:
		return "", fmt.Errorf("unsupported arithmetic operand %q", operand.Text)
	}
}

func registerOperand(operand Operand, width int) (string, error) {
	if operand.Kind != OperandRegister {
		return "", fmt.Errorf("operand %q must be a register", operand.Text)
	}
	return arm64Register(operand.Register, width)
}

func arm64Register(register string, width int) (string, error) {
	register = strings.ToUpper(register)
	prefix := "x"
	if width == 32 {
		prefix = "w"
	}
	switch register {
	case "ZR":
		return prefix + "zr", nil
	case "SP", "RSP":
		return "sp", nil
	case "LR":
		return prefix + "30", nil
	case "FP":
		return prefix + "29", nil
	}
	if strings.HasPrefix(register, "R") {
		number, err := strconv.Atoi(register[1:])
		if err == nil && number >= 0 && number <= 30 {
			return prefix + strconv.Itoa(number), nil
		}
	}
	return "", fmt.Errorf("unsupported ARM64 register %s", register)
}

func moveWidth(opcode string) int {
	if opcode == "MOVD" || opcode == "MOVW" || opcode == "MOVH" || opcode == "MOVB" {
		return 64
	}
	return 32
}

func storeWidth(opcode string) int {
	if opcode == "MOVD" {
		return 64
	}
	return 32
}

func instructionWidth(opcode string) int {
	if strings.HasSuffix(opcode, "W") {
		return 32
	}
	return 64
}

func aluMnemonic(opcode string) string {
	opcode = strings.TrimSuffix(opcode, "W")
	return strings.ToLower(opcode)
}

func branchMnemonic(opcode string) string {
	switch opcode {
	case "B":
		return "b"
	case "BL":
		return "bl"
	case "BCC", "BLO":
		return "b.lo"
	case "BCS", "BHS":
		return "b.hs"
	default:
		return "b." + strings.ToLower(strings.TrimPrefix(opcode, "B"))
	}
}

func normalizeImmediate(immediate string) string {
	immediate = strings.TrimSpace(immediate)
	if strings.HasPrefix(immediate, "~") {
		value, err := strconv.ParseInt(strings.TrimPrefix(immediate, "~"), 0, 64)
		if err == nil {
			return strconv.FormatInt(^value, 10)
		}
	}
	return immediate
}

func sanitizeSymbol(name string) string {
	var output strings.Builder
	for _, r := range name {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if r < unicode.MaxASCII {
				output.WriteRune(r)
			} else {
				output.WriteByte('_')
			}
		} else {
			output.WriteByte('_')
		}
	}
	if output.Len() == 0 {
		return "anon"
	}
	return output.String()
}
