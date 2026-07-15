package plan9asm

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Parse reads one Plan 9 assembly source file into a syntax tree.
func Parse(reader io.Reader) (*File, error) {
	file := &File{}
	scanner := bufio.NewScanner(reader)
	lineNumber := 0
	inBlockComment := false
	for scanner.Scan() {
		lineNumber++
		line := stripComments(scanner.Text(), &inBlockComment)
		for _, sourceStatement := range splitStatements(line) {
			statement, err := parseStatement(sourceStatement, lineNumber)
			if err != nil {
				return nil, err
			}
			if statement != nil {
				file.Statements = append(file.Statements, statement)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if inBlockComment {
		return nil, fmt.Errorf("plan9asm: unterminated block comment")
	}
	return file, nil
}

func parseStatement(source string, line int) (Statement, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, nil
	}
	position := statementPosition{Pos: Position{Line: line}}
	if strings.HasPrefix(source, "#") {
		return &Preprocessor{statementPosition: position, Text: source}, nil
	}
	if strings.HasSuffix(source, ":") && !strings.ContainsAny(strings.TrimSuffix(source, ":"), " \t") {
		name := strings.TrimSuffix(source, ":")
		return &Label{statementPosition: position, Name: name}, nil
	}

	name, rest := firstField(source)
	if name == "" {
		return nil, nil
	}
	upperName := strings.ToUpper(name)
	operands, err := parseOperands(rest)
	if err != nil {
		return nil, fmt.Errorf("plan9asm:%d: %w", line, err)
	}
	if upperName == "TEXT" {
		return parseText(position, operands, line)
	}
	if isDirective(upperName) {
		return &Directive{statementPosition: position, Name: upperName, Operands: operands}, nil
	}
	opcode := upperName
	suffix := ""
	if dot := strings.LastIndexByte(opcode, '.'); dot >= 0 {
		suffix = opcode[dot+1:]
		opcode = opcode[:dot]
	}
	return &Instruction{
		statementPosition: position,
		Opcode:            opcode,
		Suffix:            suffix,
		Operands:          operands,
	}, nil
}

func parseText(position statementPosition, operands []Operand, line int) (Statement, error) {
	if len(operands) < 2 || len(operands) > 3 {
		return nil, fmt.Errorf("plan9asm:%d: TEXT requires a symbol, optional flags, and frame", line)
	}
	symbol, ok := operandSymbol(operands[0])
	if !ok {
		return nil, fmt.Errorf("plan9asm:%d: invalid TEXT symbol %q", line, operands[0].Text)
	}
	text := &Text{statementPosition: position, Symbol: symbol}
	frameOperand := operands[len(operands)-1]
	if frameOperand.Kind != OperandImmediate {
		return nil, fmt.Errorf("plan9asm:%d: invalid TEXT frame %q", line, frameOperand.Text)
	}
	text.Frame, text.Args = splitFrame(frameOperand.Immediate)
	if len(operands) == 3 {
		for _, flag := range strings.Split(operands[1].Text, "|") {
			flag = strings.TrimSpace(flag)
			if flag != "" {
				text.Flags = append(text.Flags, flag)
			}
		}
	}
	return text, nil
}

func parseOperands(source string) ([]Operand, error) {
	if strings.TrimSpace(source) == "" {
		return nil, nil
	}
	parts, err := splitCommaSeparated(source)
	if err != nil {
		return nil, err
	}
	operands := make([]Operand, 0, len(parts))
	for _, part := range parts {
		operands = append(operands, parseOperand(part))
	}
	return operands, nil
}

func parseOperand(source string) Operand {
	source = strings.TrimSpace(source)
	operand := Operand{Kind: OperandRaw, Text: source}
	if strings.HasPrefix(source, "$") {
		operand.Kind = OperandImmediate
		operand.Immediate = strings.TrimSpace(strings.TrimPrefix(source, "$"))
		return operand
	}
	if isRegister(source) {
		operand.Kind = OperandRegister
		operand.Register = strings.ToUpper(source)
		return operand
	}
	for _, shift := range []string{"<<", ">>", "->", "@>"} {
		if index := strings.Index(source, shift); index > 0 {
			register := strings.TrimSpace(source[:index])
			if isRegister(register) {
				operand.Kind = OperandShiftedRegister
				operand.Register = strings.ToUpper(register)
				operand.Shift = shift
				operand.ShiftAmount = strings.TrimSpace(source[index+len(shift):])
				return operand
			}
		}
	}
	if strings.HasPrefix(source, "(") && strings.HasSuffix(source, ")") {
		inside := strings.TrimSpace(source[1 : len(source)-1])
		parts, err := splitCommaSeparated(inside)
		if err == nil && len(parts) == 2 && isRegister(parts[0]) && isRegister(parts[1]) {
			operand.Kind = OperandRegisterPair
			operand.Registers = []string{strings.ToUpper(parts[0]), strings.ToUpper(parts[1])}
			return operand
		}
	}
	if strings.HasSuffix(source, ")") {
		open := strings.LastIndexByte(source, '(')
		if open >= 0 {
			prefix := strings.TrimSpace(source[:open])
			base := strings.TrimSpace(source[open+1 : len(source)-1])
			if isRegister(base) || base == "SB" || base == "FP" || base == "SP" {
				operand.Kind = OperandMemory
				operand.Base = strings.ToUpper(base)
				operand.Offset = prefix
				if operand.Base == "SB" {
					operand.Symbol = parseSymbol(prefix)
				}
				return operand
			}
		}
	}
	if looksLikeSymbol(source) {
		operand.Kind = OperandSymbol
		operand.Symbol = parseSymbol(source)
	}
	return operand
}

func parseSymbol(source string) Symbol {
	symbol := Symbol{Raw: strings.TrimSpace(source)}
	name := symbol.Raw
	if strings.HasSuffix(name, "<>") {
		symbol.Static = true
		name = strings.TrimSuffix(name, "<>")
	}
	if close := strings.LastIndexByte(name, '>'); close == len(name)-1 {
		if open := strings.LastIndexByte(name, '<'); open >= 0 {
			symbol.ABI = name[open+1 : close]
			name = name[:open]
		}
	}
	symbol.Name = name
	return symbol
}

func operandSymbol(operand Operand) (Symbol, bool) {
	switch operand.Kind {
	case OperandMemory:
		if operand.Base == "SB" {
			return operand.Symbol, true
		}
	case OperandSymbol:
		return operand.Symbol, true
	}
	return Symbol{}, false
}

func splitFrame(source string) (string, string) {
	for index := 1; index < len(source); index++ {
		if source[index] == '-' {
			return strings.TrimSpace(source[:index]), strings.TrimSpace(source[index+1:])
		}
	}
	return strings.TrimSpace(source), ""
}

func splitCommaSeparated(source string) ([]string, error) {
	var parts []string
	start := 0
	depth := 0
	for index, r := range source {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced delimiters in %q", source)
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(source[start:index]))
				start = index + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced delimiters in %q", source)
	}
	parts = append(parts, strings.TrimSpace(source[start:]))
	return parts, nil
}

func splitStatements(line string) []string {
	var statements []string
	start := 0
	depth := 0
	for index, r := range line {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				statements = append(statements, line[start:index])
				start = index + 1
			}
		}
	}
	statements = append(statements, line[start:])
	return statements
}

func stripComments(line string, inBlockComment *bool) string {
	var result strings.Builder
	for index := 0; index < len(line); {
		if *inBlockComment {
			end := strings.Index(line[index:], "*/")
			if end < 0 {
				return result.String()
			}
			index += end + 2
			*inBlockComment = false
			continue
		}
		if strings.HasPrefix(line[index:], "//") {
			break
		}
		if strings.HasPrefix(line[index:], "/*") {
			*inBlockComment = true
			index += 2
			continue
		}
		result.WriteByte(line[index])
		index++
	}
	return result.String()
}

func firstField(source string) (string, string) {
	index := strings.IndexFunc(source, unicode.IsSpace)
	if index < 0 {
		return source, ""
	}
	return source[:index], strings.TrimSpace(source[index:])
}

func isDirective(name string) bool {
	switch name {
	case "DATA", "FUNCDATA", "GLOBL", "PCALIGN", "PCDATA":
		return true
	default:
		return false
	}
}

func isRegister(source string) bool {
	source = strings.ToUpper(strings.TrimSpace(source))
	if source == "ZR" || source == "RSP" || source == "LR" || source == "FP" || source == "SP" || source == "SB" {
		return true
	}
	if len(source) < 2 {
		return false
	}
	switch source[0] {
	case 'R', 'F', 'V':
		for _, r := range source[1:] {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func looksLikeSymbol(source string) bool {
	return strings.ContainsRune(source, '·') || strings.ContainsRune(source, '∕') || strings.Contains(source, "<>")
}
