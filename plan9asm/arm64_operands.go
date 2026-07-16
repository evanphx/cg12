package plan9asm

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
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
	if operand.Index != "" {
		index, err := arm64Register(operand.Index, 64)
		if err != nil {
			return "", err
		}
		if suffix == "P" {
			return fmt.Sprintf("[%s], %s", base, index), nil
		}
		if suffix != "" {
			return "", fmt.Errorf("indexed memory operand %q does not accept .%s", operand.Text, suffix)
		}
		return fmt.Sprintf("[%s, %s]", base, index), nil
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

func (t *arm64Translator) symbolAddress(operand Operand) (string, int64, error) {
	if operand.Kind != OperandMemory || operand.Base != "SB" {
		return "", 0, fmt.Errorf("operand %q is not relative to SB", operand.Text)
	}
	source := strings.TrimSpace(operand.Offset)
	name := source
	offset := int64(0)
	for index := len(source) - 1; index > 0; index-- {
		if source[index] != '+' && source[index] != '-' {
			continue
		}
		valueSource := strings.TrimSpace(source[index+1:])
		value, err := strconv.ParseInt(valueSource, 0, 64)
		if err != nil {
			defined, ok := t.options.Defines[valueSource]
			if !ok {
				continue
			}
			value = defined
		}
		if source[index] == '-' {
			value = -value
		}
		name = strings.TrimSpace(source[:index])
		offset = value
		break
	}
	if name == "" {
		return "", 0, fmt.Errorf("symbol address %q has no symbol", operand.Text)
	}
	if strings.Contains(name, "+") || strings.Contains(name, "-") {
		return "", 0, fmt.Errorf("unresolved symbol offset in %q", operand.Text)
	}
	return t.symbol(parseSymbol(name)), offset, nil
}

func symbolRelocation(name string, offset int64) string {
	if offset == 0 {
		return name
	}
	if offset > 0 {
		return fmt.Sprintf("%s+%d", name, offset)
	}
	return fmt.Sprintf("%s%d", name, offset)
}

func (t *arm64Translator) memoryAddress(operand Operand, suffix string) (string, error) {
	return t.memoryAddressAvoiding(operand, suffix)
}

func (t *arm64Translator) memoryAddressAvoiding(operand Operand, suffix string, avoid ...string) (string, error) {
	if operand.Offset != "" {
		offsetSource := operand.Offset
		if operand.Base == "SP" {
			offset, err := namedFrameOffset(offsetSource)
			if err != nil {
				return "", err
			}
			offsetSource = strconv.Itoa(t.currentPlan9Frame + offset)
		}
		offset, err := t.resolveIntegerExpression(offsetSource)
		if err != nil {
			return "", fmt.Errorf("invalid memory offset %q: %w", operand.Offset, err)
		}
		if suffix == "" && (offset < -256 || offset > 255) {
			base, err := arm64Register(operand.Base, 64)
			if err != nil {
				return "", err
			}
			addressRegister := "x16"
			unavailable := make(map[string]bool, len(avoid)+1)
			unavailable[base] = true
			for _, register := range avoid {
				unavailable[register] = true
			}
			for _, candidate := range []string{"x16", "x17", "x27"} {
				if !unavailable[candidate] {
					addressRegister = candidate
					break
				}
			}
			if unavailable[addressRegister] {
				return "", fmt.Errorf("no scratch register available for memory operand %q", operand.Text)
			}
			if err := t.emitRegisterOffset(addressRegister, base, offset); err != nil {
				return "", err
			}
			return "[" + addressRegister + "]", nil
		}
		operand.Offset = strconv.FormatInt(offset, 10)
	}
	return memoryAddress(operand, suffix)
}

func (t *arm64Translator) formatALUSource(operand Operand, width int) (string, error) {
	switch operand.Kind {
	case OperandRegister:
		return arm64Register(operand.Register, width)
	case OperandImmediate:
		value, err := t.resolveIntegerExpression(operand.Immediate)
		if err != nil {
			return "", err
		}
		return "#" + strconv.FormatInt(value, 10), nil
	case OperandShiftedRegister:
		register, err := arm64Register(operand.Register, width)
		if err != nil {
			return "", err
		}
		shift := map[string]string{"<<": "lsl", ">>": "lsr", "->": "asr", "@>": "ror"}[operand.Shift]
		return fmt.Sprintf("%s, %s #%s", register, shift, operand.ShiftAmount), nil
	case OperandExtendedRegister:
		registerWidth := 32
		if strings.HasSuffix(operand.Extend, "X") {
			registerWidth = 64
		}
		register, err := arm64Register(operand.Register, registerWidth)
		if err != nil {
			return "", err
		}
		extension := strings.ToLower(operand.Extend)
		if operand.ShiftAmount == "" {
			return fmt.Sprintf("%s, %s", register, extension), nil
		}
		return fmt.Sprintf("%s, %s #%s", register, extension, operand.ShiftAmount), nil
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
	case "G":
		return prefix + "28", nil
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
	case "B", "JMP":
		return "b"
	case "BL", "CALL":
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

func (t *arm64Translator) resolveIntegerExpression(source string) (int64, error) {
	source = strings.TrimSpace(source)
	if strings.HasPrefix(source, "~") {
		source = "^" + strings.TrimSpace(strings.TrimPrefix(source, "~"))
	}
	expression, err := goparser.ParseExpr(source)
	if err != nil {
		return 0, err
	}
	return t.evaluateIntegerExpression(expression)
}

func (t *arm64Translator) evaluateIntegerExpression(expression ast.Expr) (int64, error) {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if expression.Kind != token.INT {
			return 0, fmt.Errorf("%s is not an integer", expression.Value)
		}
		value, err := strconv.ParseInt(expression.Value, 0, 64)
		if err == nil {
			return value, nil
		}
		unsigned, unsignedErr := strconv.ParseUint(expression.Value, 0, 64)
		if unsignedErr != nil {
			return 0, err
		}
		return int64(unsigned), nil
	case *ast.Ident:
		value, ok := t.options.Defines[expression.Name]
		if !ok {
			return 0, fmt.Errorf("undefined assembly constant %s", expression.Name)
		}
		return value, nil
	case *ast.ParenExpr:
		return t.evaluateIntegerExpression(expression.X)
	case *ast.UnaryExpr:
		value, err := t.evaluateIntegerExpression(expression.X)
		if err != nil {
			return 0, err
		}
		switch expression.Op {
		case token.ADD:
			return value, nil
		case token.SUB:
			return -value, nil
		case token.XOR:
			return ^value, nil
		default:
			return 0, fmt.Errorf("unsupported unary operator %s", expression.Op)
		}
	case *ast.BinaryExpr:
		left, err := t.evaluateIntegerExpression(expression.X)
		if err != nil {
			return 0, err
		}
		right, err := t.evaluateIntegerExpression(expression.Y)
		if err != nil {
			return 0, err
		}
		switch expression.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return left / right, nil
		case token.REM:
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return left % right, nil
		case token.SHL:
			if right < 0 || right >= 64 {
				return 0, fmt.Errorf("invalid shift count %d", right)
			}
			return left << uint(right), nil
		case token.SHR:
			if right < 0 || right >= 64 {
				return 0, fmt.Errorf("invalid shift count %d", right)
			}
			return left >> uint(right), nil
		case token.AND:
			return left & right, nil
		case token.OR:
			return left | right, nil
		case token.XOR:
			return left ^ right, nil
		default:
			return 0, fmt.Errorf("unsupported binary operator %s", expression.Op)
		}
	default:
		return 0, fmt.Errorf("unsupported integer expression %T", expression)
	}
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
