package plan9asm

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// Parse reads one Plan 9 assembly source file into a syntax tree.
func Parse(reader io.Reader) (*File, error) {
	file := &File{}
	scanner := bufio.NewScanner(reader)
	macros := make(map[string]sourceMacro)
	var conditionals []sourceConditional
	active := true
	lineNumber := 0
	logicalLine := 0
	var logicalLineSource strings.Builder
	inBlockComment := false
	for scanner.Scan() {
		lineNumber++
		line := stripComments(scanner.Text(), &inBlockComment)
		if logicalLineSource.Len() == 0 {
			logicalLine = lineNumber
		}
		trimmed := strings.TrimSpace(line)
		continued := strings.HasSuffix(trimmed, "\\")
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(trimmed, "\\"))
		}
		if logicalLineSource.Len() > 0 && line != "" {
			logicalLineSource.WriteByte(' ')
		}
		logicalLineSource.WriteString(line)
		if continued {
			logicalLineSource.WriteByte('\n')
			continue
		}

		source := logicalLineSource.String()
		logicalLineSource.Reset()
		if handled, nextActive, err := handleSourceConditional(source, macros, conditionals, active); handled {
			if err != nil {
				return nil, fmt.Errorf("plan9asm:%d: %w", logicalLine, err)
			}
			if strings.HasPrefix(strings.TrimSpace(source), "#if") {
				conditionals = append(conditionals, sourceConditional{parentActive: active, conditionActive: nextActive})
			} else if strings.HasPrefix(strings.TrimSpace(source), "#else") {
				conditionals[len(conditionals)-1].elseSeen = true
				conditionals[len(conditionals)-1].conditionActive = nextActive
			} else {
				conditionals = conditionals[:len(conditionals)-1]
			}
			active = nextActive
			continue
		}
		if !active {
			continue
		}
		if macro, ok, err := parseSourceMacro(source); err != nil {
			return nil, fmt.Errorf("plan9asm:%d: %w", logicalLine, err)
		} else if ok {
			macros[macro.name] = macro
			continue
		}
		expanded, err := expandSourceMacro(source, macros)
		if err != nil {
			return nil, fmt.Errorf("plan9asm:%d: %w", logicalLine, err)
		}
		for _, sourceStatement := range splitStatements(expanded) {
			statement, err := parseStatement(sourceStatement, logicalLine)
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
	if logicalLineSource.Len() != 0 {
		return nil, fmt.Errorf("plan9asm:%d: unterminated line continuation", logicalLine)
	}
	if len(conditionals) != 0 {
		return nil, fmt.Errorf("plan9asm: unterminated conditional directive")
	}
	return file, nil
}

type sourceConditional struct {
	parentActive    bool
	conditionActive bool
	elseSeen        bool
}

func handleSourceConditional(source string, macros map[string]sourceMacro, stack []sourceConditional, active bool) (bool, bool, error) {
	fields := strings.Fields(strings.TrimSpace(source))
	if len(fields) == 0 {
		return false, active, nil
	}
	switch fields[0] {
	case "#ifdef", "#ifndef":
		if len(fields) != 2 {
			return true, active, fmt.Errorf("%s requires one name", fields[0])
		}
		_, defined := macros[fields[1]]
		condition := defined
		if fields[0] == "#ifndef" {
			condition = !condition
		}
		return true, active && condition, nil
	case "#else":
		if len(fields) != 1 || len(stack) == 0 {
			return true, active, fmt.Errorf("unexpected #else")
		}
		frame := stack[len(stack)-1]
		if frame.elseSeen {
			return true, active, fmt.Errorf("duplicate #else")
		}
		return true, frame.parentActive && !frame.conditionActive, nil
	case "#endif":
		if len(fields) != 1 || len(stack) == 0 {
			return true, active, fmt.Errorf("unexpected #endif")
		}
		return true, stack[len(stack)-1].parentActive, nil
	default:
		return false, active, nil
	}
}

type sourceMacro struct {
	name       string
	parameters []string
	body       string
}

func parseSourceMacro(source string) (sourceMacro, bool, error) {
	source = strings.TrimSpace(source)
	const prefix = "#define "
	if !strings.HasPrefix(source, prefix) {
		return sourceMacro{}, false, nil
	}
	source = strings.TrimSpace(strings.TrimPrefix(source, prefix))
	open := strings.IndexByte(source, '(')
	if open <= 0 {
		return sourceMacro{}, false, nil
	}
	close := strings.IndexByte(source[open+1:], ')')
	if close < 0 {
		return sourceMacro{}, false, fmt.Errorf("unterminated macro parameter list")
	}
	close += open + 1
	name := strings.TrimSpace(source[:open])
	if !isIdentifier(name) {
		return sourceMacro{}, false, fmt.Errorf("invalid macro name %q", name)
	}

	parameterSource := strings.TrimSpace(source[open+1 : close])
	var parameters []string
	if parameterSource != "" {
		for _, parameter := range strings.Split(parameterSource, ",") {
			parameter = strings.TrimSpace(parameter)
			if !isIdentifier(parameter) {
				return sourceMacro{}, false, fmt.Errorf("invalid macro parameter %q", parameter)
			}
			parameters = append(parameters, parameter)
		}
	}
	body := strings.ReplaceAll(source[close+1:], "\n", "; ")
	return sourceMacro{name: name, parameters: parameters, body: strings.TrimSpace(body)}, true, nil
}

func expandSourceMacro(source string, macros map[string]sourceMacro) (string, error) {
	trimmed := strings.TrimSpace(source)
	open := strings.IndexByte(trimmed, '(')
	if open <= 0 || !strings.HasSuffix(trimmed, ")") {
		return source, nil
	}
	macro, ok := macros[strings.TrimSpace(trimmed[:open])]
	if !ok {
		return source, nil
	}
	arguments, err := splitCommaSeparated(trimmed[open+1 : len(trimmed)-1])
	if err != nil {
		return "", err
	}
	if len(arguments) != len(macro.parameters) {
		return "", fmt.Errorf("macro %s requires %d arguments, got %d", macro.name, len(macro.parameters), len(arguments))
	}
	values := make(map[string]string, len(arguments))
	for index, parameter := range macro.parameters {
		values[parameter] = strings.TrimSpace(arguments[index])
	}

	var expanded strings.Builder
	for index := 0; index < len(macro.body); {
		if !isIdentifierByte(macro.body[index], true) {
			expanded.WriteByte(macro.body[index])
			index++
			continue
		}
		end := index + 1
		for end < len(macro.body) && isIdentifierByte(macro.body[end], false) {
			end++
		}
		identifier := macro.body[index:end]
		if value, ok := values[identifier]; ok {
			expanded.WriteString(value)
		} else {
			expanded.WriteString(identifier)
		}
		index = end
	}
	return expanded.String(), nil
}

func isIdentifier(source string) bool {
	if source == "" {
		return false
	}
	for index := 0; index < len(source); index++ {
		if !isIdentifierByte(source[index], index == 0) {
			return false
		}
	}
	return true
}

func isIdentifierByte(value byte, first bool) bool {
	if value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' {
		return true
	}
	return !first && value >= '0' && value <= '9'
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
	if strings.HasPrefix(source, "[") && strings.HasSuffix(source, "]") {
		parts, err := splitCommaSeparated(strings.TrimSpace(source[1 : len(source)-1]))
		if err == nil && len(parts) > 0 {
			vectors := make([]VectorRegister, 0, len(parts))
			for _, part := range parts {
				vector, ok := parseVectorRegister(part)
				if !ok {
					vectors = nil
					break
				}
				vectors = append(vectors, vector)
			}
			if vectors != nil {
				operand.Kind = OperandVectorList
				operand.Vectors = vectors
				return operand
			}
		}
	}
	if vector, ok := parseVectorRegister(source); ok {
		operand.Kind = OperandVectorRegister
		operand.Vector = vector
		return operand
	}
	if register, extend, shiftAmount, ok := parseExtendedRegister(source); ok {
		operand.Kind = OperandExtendedRegister
		operand.Register = register
		operand.Extend = extend
		operand.ShiftAmount = shiftAmount
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
		if close := strings.IndexByte(source, ')'); close > 0 && close < len(source)-1 {
			base := strings.TrimSpace(source[1:close])
			remainder := strings.TrimSpace(source[close+1:])
			if strings.HasPrefix(remainder, "(") && strings.HasSuffix(remainder, ")") {
				index := strings.TrimSpace(remainder[1 : len(remainder)-1])
				if isRegister(base) && isRegister(index) {
					operand.Kind = OperandMemory
					operand.Base = strings.ToUpper(base)
					operand.Index = strings.ToUpper(index)
					return operand
				}
			}
		}
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

func parseExtendedRegister(source string) (register, extend, shiftAmount string, ok bool) {
	source = strings.ToUpper(strings.TrimSpace(source))
	dot := strings.IndexByte(source, '.')
	if dot <= 0 {
		return "", "", "", false
	}
	register = source[:dot]
	if !isRegister(register) || strings.HasPrefix(register, "V") || strings.HasPrefix(register, "F") {
		return "", "", "", false
	}
	remainder := source[dot+1:]
	if shift := strings.Index(remainder, "<<"); shift >= 0 {
		shiftAmount = strings.TrimSpace(remainder[shift+2:])
		remainder = strings.TrimSpace(remainder[:shift])
		if _, err := strconv.ParseUint(shiftAmount, 0, 8); err != nil {
			return "", "", "", false
		}
	}
	switch remainder {
	case "UXTB", "UXTH", "UXTW", "UXTX", "SXTB", "SXTH", "SXTW", "SXTX":
		return register, remainder, shiftAmount, true
	default:
		return "", "", "", false
	}
}

func parseVectorRegister(source string) (VectorRegister, bool) {
	source = strings.ToUpper(strings.TrimSpace(source))
	if !strings.HasPrefix(source, "V") {
		return VectorRegister{}, false
	}

	registerEnd := 1
	for registerEnd < len(source) && source[registerEnd] >= '0' && source[registerEnd] <= '9' {
		registerEnd++
	}
	if registerEnd == 1 {
		return VectorRegister{}, false
	}
	registerNumber, err := strconv.Atoi(source[1:registerEnd])
	if err != nil || registerNumber < 0 || registerNumber > 31 {
		return VectorRegister{}, false
	}
	vector := VectorRegister{Register: source[:registerEnd]}
	if registerEnd == len(source) {
		return vector, true
	}
	if source[registerEnd] != '.' {
		return VectorRegister{}, false
	}
	arrangement := source[registerEnd+1:]
	if open := strings.IndexByte(arrangement, '['); open >= 0 {
		if !strings.HasSuffix(arrangement, "]") || open == 0 {
			return VectorRegister{}, false
		}
		vector.Index = arrangement[open+1 : len(arrangement)-1]
		if _, err := strconv.ParseUint(vector.Index, 10, 8); err != nil {
			return VectorRegister{}, false
		}
		arrangement = arrangement[:open]
	}
	if !validVectorArrangement(arrangement, vector.Index != "") {
		return VectorRegister{}, false
	}
	vector.Arrangement = arrangement
	return vector, true
}

func validVectorArrangement(arrangement string, element bool) bool {
	if element {
		return arrangement == "B" || arrangement == "H" || arrangement == "S" || arrangement == "D"
	}
	switch arrangement {
	case "B8", "B16", "H4", "H8", "S2", "S4", "D1", "D2":
		return true
	default:
		return false
	}
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
