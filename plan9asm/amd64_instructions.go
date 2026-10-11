package plan9asm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// amd64GPRNames gives the 64-, 32-, 16-, and 8-bit names of each
// general-purpose register.
var amd64GPRNames = map[string][4]string{
	"AX":  {"rax", "eax", "ax", "al"},
	"CX":  {"rcx", "ecx", "cx", "cl"},
	"DX":  {"rdx", "edx", "dx", "dl"},
	"BX":  {"rbx", "ebx", "bx", "bl"},
	"SP":  {"rsp", "esp", "sp", "spl"},
	"BP":  {"rbp", "ebp", "bp", "bpl"},
	"SI":  {"rsi", "esi", "si", "sil"},
	"DI":  {"rdi", "edi", "di", "dil"},
	"R8":  {"r8", "r8d", "r8w", "r8b"},
	"R9":  {"r9", "r9d", "r9w", "r9b"},
	"R10": {"r10", "r10d", "r10w", "r10b"},
	"R11": {"r11", "r11d", "r11w", "r11b"},
	"R12": {"r12", "r12d", "r12w", "r12b"},
	"R13": {"r13", "r13d", "r13w", "r13b"},
	"R14": {"r14", "r14d", "r14w", "r14b"},
	"R15": {"r15", "r15d", "r15w", "r15b"},
}

// amd64ByteRegisters are the explicitly byte-sized register spellings.
var amd64ByteRegisters = map[string]string{
	"AL": "al", "CL": "cl", "DL": "dl", "BL": "bl",
	"SPB": "spl", "BPB": "bpl", "SIB": "sil", "DIB": "dil",
	"R8B": "r8b", "R9B": "r9b", "R10B": "r10b", "R11B": "r11b",
	"R12B": "r12b", "R13B": "r13b", "R14B": "r14b", "R15B": "r15b",
	"AH": "ah", "CH": "ch", "DH": "dh", "BH": "bh",
}

func isAMD64GPR(register string) bool {
	if _, ok := amd64GPRNames[register]; ok {
		return true
	}
	_, ok := amd64ByteRegisters[register]
	return ok
}

func amd64GPR(register string, width int) (string, error) {
	if name, ok := amd64ByteRegisters[register]; ok {
		if width != 8 {
			return "", fmt.Errorf("byte register %s used as a %d-bit operand", register, width)
		}
		return "%" + name, nil
	}
	names, ok := amd64GPRNames[register]
	if !ok {
		return "", fmt.Errorf("%s is not a general-purpose register", register)
	}
	switch width {
	case 64:
		return "%" + names[0], nil
	case 32:
		return "%" + names[1], nil
	case 16:
		return "%" + names[2], nil
	case 8:
		return "%" + names[3], nil
	case 0:
		return "", fmt.Errorf("instruction does not take general-purpose register %s", register)
	default:
		return "", fmt.Errorf("unsupported register width %d", width)
	}
}

func amd64VectorRegister(register string) (string, bool) {
	if len(register) < 2 {
		return "", false
	}
	number := register[1:]
	if _, err := strconv.Atoi(number); err != nil {
		return "", false
	}
	switch register[0] {
	case 'X':
		return "%xmm" + number, true
	case 'Y':
		return "%ymm" + number, true
	case 'Z':
		return "%zmm" + number, true
	case 'K':
		return "%k" + number, true
	}
	return "", false
}

func isAMD64VectorOperand(operand Operand) bool {
	if operand.Kind != OperandRegister {
		return false
	}
	_, ok := amd64VectorRegister(operand.Register)
	return ok && operand.Register[0] != 'K'
}

func isAMD64MaskOperand(operand Operand) bool {
	return operand.Kind == OperandRegister && len(operand.Register) == 2 && operand.Register[0] == 'K'
}

// amd64ShiftFamilies take their count in CL when it is not an immediate.
var amd64ShiftFamilies = map[string]bool{
	"SHL": true, "SHR": true, "SAR": true, "SAL": true,
	"ROL": true, "ROR": true, "RCL": true, "RCR": true,
}

// amd64UnaryDestinations are the one-operand instructions whose operand is
// written (cmd/asm's UnaryDst), used to detect SP writes.
var amd64UnaryDestinations = map[string]bool{
	"INC": true, "DEC": true, "NEG": true, "NOT": true, "POP": true, "BSWAP": true,
}

// amd64FloatImmediate lists the instructions that accept a floating-point
// immediate, which obj6 replaces with a load from a constant in memory. The
// value is the constant's width in bytes.
var amd64FloatImmediate = map[string]int{
	"MOVSS": 4, "VMOVSS": 4, "ADDSS": 4, "SUBSS": 4, "MULSS": 4, "DIVSS": 4,
	"COMISS": 4, "UCOMISS": 4,
	"MOVSD": 8, "VMOVSD": 8, "ADDSD": 8, "SUBSD": 8, "MULSD": 8, "DIVSD": 8,
	"COMISD": 8, "UCOMISD": 8,
}

func (t *amd64Translator) recordReferences(instruction *Instruction) {
	for _, operand := range instruction.Operands {
		if operand.Base == "TLS" || operand.Index == "TLS" {
			if operand.Kind == OperandMemory {
				t.references[AMD64TLSSymbol] = true
			}
			continue
		}
		if operand.Kind != OperandMemory && operand.Kind != OperandAddress {
			continue
		}
		if operand.Base != "SB" || operand.Symbol.Static {
			continue
		}
		t.references[t.symbol(operand.Symbol)] = true
	}
}

func (t *amd64Translator) translateInstruction(instruction *Instruction) error {
	t.recordReferences(instruction)
	opcode := instruction.Opcode
	if opcode == "MOVD" || opcode == "MOVDQ2Q" {
		// cmd/asm aliases both to MOVQ.
		opcode = "MOVQ"
	}
	operands := instruction.Operands
	if instruction.Suffix != "" && instruction.Suffix != "Z" {
		return fmt.Errorf("unsupported %s suffix .%s", instruction.Opcode, instruction.Suffix)
	}
	if instruction.Suffix == "Z" && !strings.HasPrefix(opcode, "V") {
		return fmt.Errorf("%s does not accept .Z", instruction.Opcode)
	}

	switch opcode {
	case "BYTE", "WORD", "LONG", "QUAD":
		return t.translateRawData(opcode, operands)
	case "NOP":
		// NOP is a pseudo-instruction that assembles to nothing; NOP SP only
		// tells vet that SP changed.
		return nil
	case "RET":
		if len(operands) != 0 {
			return fmt.Errorf("RET with operands is unsupported")
		}
		if err := t.emitEpilogue(); err != nil {
			return err
		}
		t.output.WriteString("\tret\n")
		return nil
	case "JMP", "CALL":
		return t.translateBranch(instruction, strings.ToLower(opcode))
	case "ADJSP":
		if len(operands) != 1 || operands[0].Kind != OperandImmediate {
			return fmt.Errorf("ADJSP requires one immediate")
		}
		value, err := t.resolveInteger(operands[0].Immediate)
		if err != nil {
			return err
		}
		t.emitAdjustSP(value)
		t.deltasp += int(value)
		return nil
	case "INT":
		if len(operands) != 1 || operands[0].Kind != OperandImmediate {
			return fmt.Errorf("INT requires one immediate")
		}
		value, err := t.resolveInteger(operands[0].Immediate)
		if err != nil {
			return err
		}
		if value == 3 {
			// Go encodes INT $3 as CD 03; GNU as would pick the one-byte int3.
			t.output.WriteString("\t.byte 0xcd, 0x03\n")
			return nil
		}
		fmt.Fprintf(&t.output, "\tint $%d\n", value)
		return nil
	}
	if mnemonic, ok := amd64JumpMnemonics[opcode]; ok {
		return t.translateBranch(instruction, mnemonic)
	}
	if (opcode == "MOVQ" || opcode == "MOVL") && len(operands) == 2 && operands[0].Kind == OperandRegister && operands[0].Register == "TLS" {
		// MOVQ TLS, r loads the TLS base for a later off(r)(TLS*1) access.
		// With the one-instruction TLS form (CanUse1InsnTLS on linux without
		// -shared) obj6 turns it into a NOP and rewrites the access to
		// off(TLS).
		if operands[1].Kind != OperandRegister || !isAMD64GPR(operands[1].Register) {
			return fmt.Errorf("MOVQ TLS requires a general-purpose destination")
		}
		return nil
	}

	spec, ok := lookupAMD64(opcode)
	if !ok {
		return fmt.Errorf("unsupported amd64 instruction %s", instruction.Opcode)
	}
	t.noteSPWrite(opcode, operands)
	if err := t.emitInstruction(instruction, opcode, spec); err != nil {
		return err
	}
	t.trackStackAdjustment(opcode)
	return nil
}

// trackStackAdjustment follows obj6's deltasp: only PUSH and POP (and ADJSP,
// handled separately) move the pseudo-registers' base.
func (t *amd64Translator) trackStackAdjustment(opcode string) {
	switch opcode {
	case "PUSHQ", "PUSHFQ":
		t.deltasp += 8
	case "PUSHL", "PUSHFL":
		t.deltasp += 4
	case "PUSHW", "PUSHFW":
		t.deltasp += 2
	case "POPQ", "POPFQ":
		t.deltasp -= 8
	case "POPL", "POPFL":
		t.deltasp -= 4
	case "POPW", "POPFW":
		t.deltasp -= 2
	}
}

// noteSPWrite mirrors obj6's automatic SPWRITE marking: any instruction other
// than CMP whose destination is SP.
func (t *amd64Translator) noteSPWrite(opcode string, operands []Operand) {
	switch opcode {
	case "CMPL", "CMPQ", "PUSHQ", "PUSHL", "PUSHW", "PUSHFQ", "PUSHFL", "PUSHFW",
		"POPQ", "POPL", "POPW", "POPFQ", "POPFL", "POPFW":
		return
	}
	if len(operands) == 0 {
		return
	}
	destination := operands[len(operands)-1]
	if len(operands) == 1 {
		base := opcode[:len(opcode)-1]
		if !amd64UnaryDestinations[base] && !strings.HasPrefix(opcode, "SET") {
			return
		}
	}
	if destination.Kind == OperandRegister && destination.Register == "SP" {
		t.current.spWrite = true
	}
}

func (t *amd64Translator) translateRawData(opcode string, operands []Operand) error {
	directives := map[string]string{"BYTE": ".byte", "WORD": ".short", "LONG": ".long", "QUAD": ".quad"}
	masks := map[string]uint64{"BYTE": 0xff, "WORD": 0xffff, "LONG": 0xffffffff, "QUAD": math.MaxUint64}
	if len(operands) != 1 || operands[0].Kind != OperandImmediate {
		return fmt.Errorf("%s requires one immediate", opcode)
	}
	value, err := t.resolveInteger(operands[0].Immediate)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\t%s 0x%x\n", directives[opcode], uint64(value)&masks[opcode])
	return nil
}

func (t *amd64Translator) translateBranch(instruction *Instruction, mnemonic string) error {
	if len(instruction.Operands) != 1 {
		return fmt.Errorf("%s requires one target", instruction.Opcode)
	}
	operand := instruction.Operands[0]
	conditional := mnemonic != "jmp" && mnemonic != "call"
	switch operand.Kind {
	case OperandLabel:
		label := t.current.labels[operand.Text]
		if label == "" {
			return fmt.Errorf("undefined label %s", operand.Text)
		}
		fmt.Fprintf(&t.output, "\t%s %s\n", mnemonic, label)
		return nil
	case OperandRegister:
		if conditional {
			return fmt.Errorf("%s cannot branch through a register", instruction.Opcode)
		}
		register, err := amd64GPR(operand.Register, 64)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\t%s *%s\n", mnemonic, register)
		return nil
	case OperandMemory:
		if operand.Base == "PC" {
			offset, err := t.resolveInteger(operand.Offset)
			if err != nil {
				return err
			}
			target := t.progIndex[instruction] + int(offset)
			label := t.current.pcLabels[target]
			if label == "" {
				return fmt.Errorf("PC-relative target %d has no label", target)
			}
			fmt.Fprintf(&t.output, "\t%s %s\n", mnemonic, label)
			return nil
		}
		if operand.Base == "SB" && operand.Index == "" {
			name, offset, err := t.symbolOffset(operand)
			if err != nil {
				return err
			}
			if offset != 0 {
				return fmt.Errorf("branch target %q has an offset", operand.Text)
			}
			target := t.branchSymbolName(operand.Symbol)
			if target != name {
				t.abi0References[name] = true
			}
			fmt.Fprintf(&t.output, "\t%s %s\n", mnemonic, target)
			return nil
		}
		if conditional {
			return fmt.Errorf("%s cannot branch through memory", instruction.Opcode)
		}
		address, err := t.formatMemory(operand, false)
		if err != nil {
			return err
		}
		fmt.Fprintf(&t.output, "\t%s *%s\n", mnemonic, address)
		return nil
	default:
		return fmt.Errorf("unsupported branch target %q", operand.Text)
	}
}

// emitInstruction writes one table-driven instruction.
func (t *amd64Translator) emitInstruction(instruction *Instruction, opcode string, spec amd64Spec) error {
	operands := append([]Operand(nil), instruction.Operands...)
	mnemonic := spec.mnemonic

	mask := ""
	if strings.HasPrefix(opcode, "V") && len(operands) >= 3 && isAMD64MaskOperand(operands[len(operands)-2]) {
		register, _ := amd64VectorRegister(operands[len(operands)-2].Register)
		mask = "{" + register + "}"
		operands = append(operands[:len(operands)-2], operands[len(operands)-1])
	}
	if instruction.Suffix == "Z" {
		if mask == "" {
			return fmt.Errorf("%s.Z requires a mask register", instruction.Opcode)
		}
		mask += "{z}"
	}

	switch opcode {
	case "MOVQ", "MOVL":
		hasVector := false
		for _, operand := range operands {
			if isAMD64VectorOperand(operand) {
				hasVector = true
			}
		}
		if hasVector {
			if opcode == "MOVQ" {
				spec = amd64Spec{mnemonic: "movq", width: 64}
			} else {
				spec = amd64Spec{mnemonic: "movd", width: 32}
			}
			mnemonic = spec.mnemonic
		}
		if len(operands) == 2 && operands[0].Kind == OperandAddress {
			// obj6 rewrites MOVQ $addr, r as LEAQ addr, r.
			return t.emitLoadAddress(operands, spec.width)
		}
		if opcode == "MOVQ" && !hasVector && len(operands) == 2 && operands[0].Kind == OperandImmediate && operands[1].Kind == OperandRegister {
			value, err := t.resolveInteger(operands[0].Immediate)
			if err == nil && value >= 1<<31 && value < 1<<32 {
				// asm6 (Ziq_rp) loads a constant that is not a sign-extended
				// imm32 but has a zero upper half with the 32-bit MOV, which
				// zero-extends; match it rather than GNU as's MOVABS.
				register, err := amd64GPR(operands[1].Register, 32)
				if err != nil {
					return err
				}
				fmt.Fprintf(&t.output, "\tmovl $%d, %s\n", value, register)
				return nil
			}
		}
	case "LEAQ", "LEAL", "LEAW":
		if len(operands) != 2 || operands[0].Kind != OperandMemory {
			return fmt.Errorf("%s requires a memory source", opcode)
		}
		return t.emitLoadAddress(operands, spec.width)
	}

	if width, ok := amd64FloatImmediate[opcode]; ok && len(operands) >= 2 && operands[0].Kind == OperandImmediate {
		if handled, err := t.emitFloatImmediate(opcode, mnemonic, operands, width); handled || err != nil {
			return err
		}
	}

	shift := len(opcode) > 1 && amd64ShiftFamilies[opcode[:len(opcode)-1]]
	formatted := make([]string, len(operands))
	for index, operand := range operands {
		if shift && index == 0 && len(operands) >= 2 && operand.Kind == OperandRegister && (operand.Register == "CX" || operand.Register == "CL") {
			// A variable shift count is always CL.
			formatted[index] = "%cl"
			continue
		}
		width := spec.operandWidth(index)
		text, err := t.formatOperand(operand, width)
		if err != nil {
			return err
		}
		if operand.Kind == OperandImmediate && spec.width == 64 && opcode != "MOVQ" {
			text = t.wrapImmediate32(operand, text)
		}
		formatted[index] = text
	}

	if shift {
		if len(operands) == 3 {
			switch opcode[:len(opcode)-1] {
			case "SHL":
				mnemonic = "shld" + strings.ToLower(opcode[len(opcode)-1:])
			case "SHR":
				mnemonic = "shrd" + strings.ToLower(opcode[len(opcode)-1:])
			default:
				return fmt.Errorf("%s does not take three operands", opcode)
			}
		}
	}

	if mask != "" {
		formatted[len(formatted)-1] += mask
	}

	switch spec.order {
	case amd64SwapOrder:
		if len(formatted) != 2 {
			return fmt.Errorf("%s requires two operands", opcode)
		}
		formatted[0], formatted[1] = formatted[1], formatted[0]
	case amd64PredicateLast:
		if len(formatted) != 3 {
			return fmt.Errorf("%s requires two operands and a predicate", opcode)
		}
		if operands[2].Kind == OperandMemory && operands[2].Base == "" && operands[2].Index == "" {
			// obj6 rewrites a bare predicate (CMPPS X1, X0, 4) into $4.
			value, err := t.resolveInteger(operands[2].Offset)
			if err != nil {
				return err
			}
			formatted[2] = "$" + strconv.FormatInt(value, 10)
		}
		formatted = []string{formatted[2], formatted[0], formatted[1]}
	}

	if len(formatted) == 0 {
		fmt.Fprintf(&t.output, "\t%s\n", mnemonic)
		return nil
	}
	fmt.Fprintf(&t.output, "\t%s %s\n", mnemonic, strings.Join(formatted, ", "))
	return nil
}

// wrapImmediate32 reproduces how asm6 treats an unsigned 32-bit constant in
// a 64-bit operation other than MOVQ: it accepts it as an imm32 and encodes
// its low 32 bits, which the processor sign-extends (PUSHQ $0xf1234567 pushes
// 0xfffffffff1234567). GNU as refuses the out-of-range value, so the
// translation spells the sign-extended value explicitly.
func (t *amd64Translator) wrapImmediate32(operand Operand, text string) string {
	value, err := t.resolveInteger(operand.Immediate)
	if err != nil || value < 1<<31 || value >= 1<<32 {
		return text
	}
	return "$" + strconv.FormatInt(int64(int32(uint32(value))), 10)
}

// emitLoadAddress lowers LEAQ/LEAL and the MOVQ $addr form obj6 turns into
// one.
func (t *amd64Translator) emitLoadAddress(operands []Operand, width int) error {
	if len(operands) != 2 || operands[1].Kind != OperandRegister {
		return fmt.Errorf("address load requires a register destination")
	}
	address, err := t.formatMemory(operands[0], true)
	if err != nil {
		return err
	}
	destination, err := amd64GPR(operands[1].Register, width)
	if err != nil {
		return err
	}
	mnemonic := map[int]string{64: "leaq", 32: "leal", 16: "leaw"}[width]
	if mnemonic == "" {
		return fmt.Errorf("unsupported address width %d", width)
	}
	if operands[1].Register == "SP" {
		t.current.spWrite = true
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, address, destination)
	return nil
}

// emitFloatImmediate handles a floating-point immediate source the way obj6
// does: MOVSS/MOVSD of +0.0 into a register is XORPS, anything else loads a
// file-local constant.
func (t *amd64Translator) emitFloatImmediate(opcode, mnemonic string, operands []Operand, width int) (bool, error) {
	source := strings.TrimSpace(operands[0].Immediate)
	if _, err := t.resolveInteger(source); err == nil {
		return false, nil
	}
	value, err := parseAMD64Float(source)
	if err != nil {
		return true, fmt.Errorf("invalid floating-point immediate $%s", source)
	}
	if (opcode == "MOVSS" || opcode == "MOVSD") && math.Float64bits(value) == 0 && len(operands) == 2 && isAMD64VectorOperand(operands[1]) {
		register, _ := amd64VectorRegister(operands[1].Register)
		fmt.Fprintf(&t.output, "\txorps %s, %s\n", register, register)
		return true, nil
	}
	label := t.floatConstant(value, width)
	formatted := []string{label + "(%rip)"}
	for _, operand := range operands[1:] {
		text, err := t.formatOperand(operand, 0)
		if err != nil {
			return true, err
		}
		formatted = append(formatted, text)
	}
	fmt.Fprintf(&t.output, "\t%s %s\n", mnemonic, strings.Join(formatted, ", "))
	return true, nil
}

func parseAMD64Float(source string) (float64, error) {
	source = strings.TrimSpace(source)
	for strings.HasPrefix(source, "(") && strings.HasSuffix(source, ")") {
		source = strings.TrimSpace(source[1 : len(source)-1])
	}
	return strconv.ParseFloat(source, 64)
}

func (t *amd64Translator) floatConstant(value float64, width int) string {
	var bits uint64
	directive := ".quad"
	if width == 4 {
		bits = uint64(math.Float32bits(float32(value)))
		directive = ".long"
	} else {
		bits = math.Float64bits(value)
	}
	label := fmt.Sprintf(".L%s_f%d_%x", t.fileTag, width*8, bits)
	t.floatConstants[label] = fmt.Sprintf("%s 0x%x", directive, bits)
	return label
}

func (t *amd64Translator) emitFloatConstants() {
	if len(t.floatConstants) == 0 {
		return
	}
	t.output.WriteString("\n\t.section .rodata\n")
	for _, label := range sortedSet(mapKeys(t.floatConstants)) {
		t.output.WriteString("\t.balign 8\n")
		fmt.Fprintf(&t.output, "%s:\n\t%s\n", label, t.floatConstants[label])
	}
	t.output.WriteString("\t.text\n")
}

func mapKeys(values map[string]string) map[string]bool {
	keys := make(map[string]bool, len(values))
	for key := range values {
		keys[key] = true
	}
	return keys
}

func (t *amd64Translator) formatOperand(operand Operand, width int) (string, error) {
	switch operand.Kind {
	case OperandRegister:
		if register, ok := amd64VectorRegister(operand.Register); ok {
			return register, nil
		}
		return amd64GPR(operand.Register, width)
	case OperandImmediate:
		if strings.HasPrefix(operand.Immediate, "\"") {
			return "", fmt.Errorf("string immediate %s outside DATA", operand.Text)
		}
		value, err := t.resolveInteger(operand.Immediate)
		if err != nil {
			return "", fmt.Errorf("invalid immediate %s: %w", operand.Text, err)
		}
		return "$" + strconv.FormatInt(value, 10), nil
	case OperandAddress:
		if operand.Base != "SB" || operand.Index != "" {
			return "", fmt.Errorf("address constant %s is only valid as a MOVQ/MOVL source", operand.Text)
		}
		// An absolute address as an immediate ($sym in PUSHQ or CMPQ), which
		// like Go's encoding needs a non-PIE link.
		_, offset, err := t.symbolOffset(operand)
		if err != nil {
			return "", err
		}
		target := t.addressSymbolName(operand.Symbol)
		return "$" + symbolRelocation(target, offset), nil
	case OperandMemory:
		return t.formatMemory(operand, false)
	default:
		return "", fmt.Errorf("unsupported operand %q", operand.Text)
	}
}

// symbolOffset splits an SB reference into its resolved symbol name and
// constant displacement.
func (t *amd64Translator) symbolOffset(operand Operand) (string, int64, error) {
	text := strings.TrimSpace(operand.Offset)
	name := amd64SymbolName(text)
	rest := strings.TrimSpace(text[len(name):])
	offset := int64(0)
	if rest != "" {
		value, err := t.resolveInteger(rest)
		if err != nil {
			return "", 0, fmt.Errorf("invalid symbol offset in %q: %w", operand.Text, err)
		}
		offset = value
	}
	return t.symbol(operand.Symbol), offset, nil
}

// frameOffset resolves the displacement of an FP or SP reference. named
// reports a pseudo-register reference (x+8(FP), y-8(SP)); a displacement
// made only of numbers and go_asm.h constants is a hardware SP offset.
func (t *amd64Translator) frameOffset(text string) (int64, bool, error) {
	text = strings.TrimSpace(text)
	if value, err := t.resolveInteger(text); err == nil {
		return value, false, nil
	}
	end := 0
	for end < len(text) && (isIdentifierByte(text[end], end == 0) || text[end] == '.') {
		end++
	}
	if end == 0 {
		return 0, false, fmt.Errorf("invalid frame offset %q", text)
	}
	rest := strings.TrimSpace(text[end:])
	if rest == "" {
		return 0, true, nil
	}
	if rest[0] != '+' && rest[0] != '-' {
		return 0, false, fmt.Errorf("invalid frame offset %q", text)
	}
	value, err := t.resolveInteger(rest)
	if err != nil {
		return 0, false, fmt.Errorf("invalid frame offset %q: %w", text, err)
	}
	return value, true, nil
}

// formatMemory renders a memory operand in AT&T syntax, resolving the FP and
// named-SP pseudo-registers against the current frame as obj6 does.
func (t *amd64Translator) formatMemory(operand Operand, address bool) (string, error) {
	if operand.Base == "TLS" || operand.Index == "TLS" {
		if operand.Index == "TLS" && operand.Scale != "1" && operand.Scale != "2" {
			return "", fmt.Errorf("invalid TLS reference %q", operand.Text)
		}
		offset, err := t.resolveInteger(operand.Offset)
		if err != nil {
			return "", err
		}
		t.references[AMD64TLSSymbol] = true
		return "%fs:" + symbolRelocation(AMD64TLSSymbol+"@tpoff", offset), nil
	}
	index := ""
	if operand.Index != "" {
		register, err := amd64IndexRegister(operand.Index)
		if err != nil {
			return "", err
		}
		index = register + "," + operand.Scale
	}
	switch operand.Base {
	case "SB":
		_, offset, err := t.symbolOffset(operand)
		if err != nil {
			return "", err
		}
		name := t.symbol(operand.Symbol)
		if address {
			name = t.addressSymbolName(operand.Symbol)
		}
		target := symbolRelocation(name, offset)
		if index != "" {
			// An indexed symbol reference cannot be RIP-relative; it uses an
			// absolute 32-bit displacement, as Go's own encoding does, which
			// requires a non-PIE link.
			return fmt.Sprintf("%s(,%s)", target, index), nil
		}
		return target + "(%rip)", nil
	case "FP":
		offset, _, err := t.frameOffset(operand.Offset)
		if err != nil {
			return "", err
		}
		return amd64Displacement(offset+int64(t.deltasp)+8, "%rsp", ""), nil
	case "SP":
		offset, named, err := t.frameOffset(operand.Offset)
		if err != nil {
			return "", err
		}
		if named {
			offset += int64(t.deltasp - t.current.bpsize)
		}
		return amd64Displacement(offset, "%rsp", index), nil
	case "PC":
		return "", fmt.Errorf("PC-relative operand %q outside a branch", operand.Text)
	case "":
		offset, err := t.resolveInteger(operand.Offset)
		if err != nil {
			return "", err
		}
		if index == "" {
			return strconv.FormatInt(offset, 10), nil
		}
		return fmt.Sprintf("%d(,%s)", offset, index), nil
	default:
		base, err := amd64GPR(operand.Base, 64)
		if err != nil {
			return "", err
		}
		offset, err := t.resolveInteger(operand.Offset)
		if err != nil {
			return "", fmt.Errorf("invalid memory offset %q: %w", operand.Offset, err)
		}
		return amd64Displacement(offset, base, index), nil
	}
}

// amd64IndexRegister names an index register: a 64-bit general-purpose
// register, or a vector register for the VSIB addressing of gathers and
// scatters.
func amd64IndexRegister(register string) (string, error) {
	if vector, ok := amd64VectorRegister(register); ok && register[0] != 'K' {
		return vector, nil
	}
	return amd64GPR(register, 64)
}

func amd64Displacement(offset int64, base, index string) string {
	inside := base
	if index != "" {
		inside += "," + index
	}
	if offset == 0 {
		return "(" + inside + ")"
	}
	return fmt.Sprintf("%d(%s)", offset, inside)
}
