package plan9asm

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

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
