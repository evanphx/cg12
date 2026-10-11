package plan9asm

import (
	"fmt"
	"strconv"
	"strings"
)

// amd64RegisterNames lists every register spelling the Go assembler accepts
// for amd64 (cmd/internal/obj/x86.Register), keyed by name.
var amd64RegisterNames = buildAMD64RegisterNames()

func buildAMD64RegisterNames() map[string]bool {
	names := make(map[string]bool)
	fixed := []string{
		"AL", "CL", "DL", "BL", "SPB", "BPB", "SIB", "DIB",
		"AX", "CX", "DX", "BX", "SP", "BP", "SI", "DI",
		"AH", "CH", "DH", "BH",
		"CS", "SS", "DS", "ES", "FS", "GS",
		"GDTR", "IDTR", "LDTR", "MSW", "TASK", "TLS",
	}
	for _, name := range fixed {
		names[name] = true
	}
	for number := 8; number <= 15; number++ {
		names["R"+strconv.Itoa(number)] = true
		names["R"+strconv.Itoa(number)+"B"] = true
	}
	for number := 0; number <= 31; number++ {
		names["X"+strconv.Itoa(number)] = true
		names["Y"+strconv.Itoa(number)] = true
		names["Z"+strconv.Itoa(number)] = true
	}
	for number := 0; number <= 7; number++ {
		names["F"+strconv.Itoa(number)] = true
		names["M"+strconv.Itoa(number)] = true
		names["K"+strconv.Itoa(number)] = true
		names["DR"+strconv.Itoa(number)] = true
		names["TR"+strconv.Itoa(number)] = true
	}
	for number := 0; number <= 15; number++ {
		names["CR"+strconv.Itoa(number)] = true
	}
	return names
}

// canonicalAMD64Register returns the canonical spelling of an amd64 register
// operand, mapping the assembler's g alias to R14.
func canonicalAMD64Register(source string) (string, bool) {
	if source == "g" {
		return "R14", true
	}
	if amd64RegisterNames[source] {
		return source, true
	}
	return "", false
}

func isAMD64MemoryBase(source string) bool {
	switch source {
	case "SB", "FP", "SP", "PC", "TLS":
		return true
	}
	_, ok := canonicalAMD64Register(source)
	return ok
}

func parseAMD64Operands(source string) ([]Operand, error) {
	if strings.TrimSpace(source) == "" {
		return nil, nil
	}
	parts, err := splitCommaSeparated(source)
	if err != nil {
		return nil, err
	}
	operands := make([]Operand, 0, len(parts))
	for _, part := range parts {
		operand, err := parseAMD64Operand(part)
		if err != nil {
			return nil, err
		}
		operands = append(operands, operand)
	}
	return operands, nil
}

// parseAMD64Operand parses one operand in the Go assembler's amd64 grammar.
// Unlike the AArch64-oriented generic grammar, it never falls back to an
// untyped operand: text that is not a register, immediate, address constant,
// memory reference, or label is an error.
func parseAMD64Operand(source string) (Operand, error) {
	source = strings.TrimSpace(source)
	operand := Operand{Text: source}
	if source == "" {
		return operand, fmt.Errorf("empty operand")
	}
	if strings.HasPrefix(source, "$") {
		value := strings.TrimSpace(source[1:])
		if value == "" {
			return operand, fmt.Errorf("empty immediate %q", source)
		}
		operand.Kind = OperandImmediate
		operand.Immediate = value
		if strings.HasPrefix(value, "\"") {
			return operand, nil
		}
		memory, ok, err := parseAMD64Memory(value)
		if err != nil {
			return operand, fmt.Errorf("operand %q: %w", source, err)
		}
		if ok {
			memory.Kind = OperandAddress
			memory.Text = source
			memory.Immediate = value
			return memory, nil
		}
		return operand, nil
	}
	if register, ok := canonicalAMD64Register(source); ok {
		operand.Kind = OperandRegister
		operand.Register = register
		return operand, nil
	}
	memory, ok, err := parseAMD64Memory(source)
	if err != nil {
		return operand, fmt.Errorf("operand %q: %w", source, err)
	}
	if ok {
		memory.Text = source
		return memory, nil
	}
	if isIdentifier(source) {
		operand.Kind = OperandLabel
		return operand, nil
	}
	if isAMD64AbsoluteAddress(source) {
		// A bare number is an absolute memory address: MOVL $0xf1, 0xf1.
		operand.Kind = OperandMemory
		operand.Offset = source
		return operand, nil
	}
	return operand, fmt.Errorf("unrecognized amd64 operand %q", source)
}

func isAMD64AbsoluteAddress(source string) bool {
	trimmed := strings.TrimPrefix(source, "-")
	if trimmed == "" {
		return false
	}
	_, err := strconv.ParseUint(trimmed, 0, 64)
	return err == nil
}

// parseAMD64Memory recognizes off(base), off(base)(index*scale),
// off(index*scale), sym+off(SB), name+off(FP), and name-off(SP). The boolean
// is false when the text is not a memory reference at all (for example the
// parenthesized constant expression (5*8)).
func parseAMD64Memory(source string) (Operand, bool, error) {
	operand := Operand{Kind: OperandMemory}
	if !strings.HasSuffix(source, ")") {
		return operand, false, nil
	}
	open, err := matchingOpenParen(source, len(source)-1)
	if err != nil {
		return operand, false, nil
	}
	last := strings.TrimSpace(source[open+1 : len(source)-1])
	prefix := source[:open]
	if index, scale, ok := parseAMD64Index(last); ok {
		operand.Index = index
		operand.Scale = scale
		trimmedPrefix := strings.TrimSpace(prefix)
		if strings.HasSuffix(trimmedPrefix, ")") {
			baseOpen, err := matchingOpenParen(trimmedPrefix, len(trimmedPrefix)-1)
			if err == nil {
				base := strings.TrimSpace(trimmedPrefix[baseOpen+1 : len(trimmedPrefix)-1])
				if isAMD64MemoryBase(base) {
					operand.Base = canonicalAMD64Base(base)
					prefix = trimmedPrefix[:baseOpen]
				}
			}
		}
		if operand.Base == "PC" || operand.Base == "FP" {
			return operand, false, fmt.Errorf("%s cannot be indexed", operand.Base)
		}
	} else if isAMD64MemoryBase(last) {
		operand.Base = canonicalAMD64Base(last)
	} else {
		return operand, false, nil
	}
	operand.Offset = strings.TrimSpace(prefix)
	if operand.Base == "SB" {
		if operand.Offset == "" {
			return operand, false, fmt.Errorf("SB reference has no symbol")
		}
		operand.Symbol = parseSymbol(amd64SymbolName(operand.Offset))
	}
	return operand, true, nil
}

func canonicalAMD64Base(source string) string {
	if register, ok := canonicalAMD64Register(source); ok {
		return register
	}
	return source
}

// amd64SymbolName strips a trailing +off or -off displacement from an SB
// reference, leaving the symbol spelling that parseSymbol expects.
func amd64SymbolName(source string) string {
	source = strings.TrimSpace(source)
	depth := 0
	for index := len(source) - 1; index > 0; index-- {
		switch source[index] {
		case ')':
			depth++
		case '(':
			depth--
		case '+', '-':
			if depth == 0 && source[index-1] != '<' {
				return strings.TrimSpace(source[:index])
			}
		}
	}
	return source
}

func parseAMD64Index(source string) (string, string, bool) {
	star := strings.IndexByte(source, '*')
	if star <= 0 {
		return "", "", false
	}
	register, ok := canonicalAMD64Register(strings.TrimSpace(source[:star]))
	if !ok {
		return "", "", false
	}
	scale := strings.TrimSpace(source[star+1:])
	switch scale {
	case "1", "2", "4", "8":
		return register, scale, true
	default:
		return "", "", false
	}
}

func matchingOpenParen(source string, close int) (int, error) {
	depth := 0
	for index := close; index >= 0; index-- {
		switch source[index] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced parentheses in %q", source)
}
