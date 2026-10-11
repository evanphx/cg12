package plan9asm

import (
	"fmt"
	"strconv"
	"strings"
)

// amd64FunctionKind says how a TEXT declaration is exposed to callers.
type amd64FunctionKind uint8

const (
	// amd64Static is a file-local function (foo<>): one local body label.
	amd64Static amd64FunctionKind = iota
	// amd64ABIInternal is declared <ABIInternal>: the body is the plain name.
	amd64ABIInternal
	// amd64ABI0Alias is an ABI0 function with no arguments or results: the
	// body is name_abi0 and the plain name labels the same address.
	amd64ABI0Alias
	// amd64ABI0Wrapped is an ABI0 function with arguments or results: the
	// body is name_abi0 and the plain name is a generated ABIInternal entry.
	amd64ABI0Wrapped
	// amd64ABI0Vfork is syscall.rawVforkSyscall: the body is name_abi0 and
	// the plain name is a hand-built ABIInternal entry that never touches the
	// stack across the system call (see emitRawVforkSyscallAMD64).
	amd64ABI0Vfork
	// amd64ABIInternalAdapted is an <ABIInternal> function with more than one
	// result: the body is name_abiinternal and the plain name is an adapter
	// that returns the extra results through caller-supplied pointers.
	amd64ABIInternalAdapted
)

// amd64FunctionInfo is what the analysis pass learns about one TEXT before
// any code is emitted.
type amd64FunctionInfo struct {
	text      *Text
	symbol    string
	body      string
	kind      amd64FunctionKind
	frame     int
	args      int
	argsKnown bool
	hasCall   bool
	bpsize    int
	autoffset int
	labels    map[string]string
	pcLabels  map[int]string
	layout    amd64Layout
	calls     []string
	indirect  bool
	addresses []string
	spWrite   bool
	tailAlias string
	// outputOffsets are the inferred ABI0 slots the body writes (results).
	outputOffsets map[int]bool
}

type amd64Translator struct {
	options           AMD64Options
	fileTag           string
	output            strings.Builder
	references        map[string]bool
	abi0References    map[string]bool
	globalDefinitions map[string]bool
	abiInternal       map[string]bool
	abi0Defined       map[string]bool
	adaptedBodies     map[string]string
	data              map[string][]arm64DataValue
	floatConstants    map[string]string
	functions         []AMD64Function
	infos             []*amd64FunctionInfo
	progIndex         map[Statement]int

	current      *amd64FunctionInfo
	currentIndex int
	bodyFunction int
	deltasp      int
}

// analyze walks the file once to learn every function's frame, labels,
// PC-relative branch targets, calls, and ABI0 argument layout, so that the
// emitting pass can name forward references correctly.
func (t *amd64Translator) analyze(file *File) error {
	t.progIndex = make(map[Statement]int)
	t.globalDefinitions = make(map[string]bool)
	t.currentIndex = -1
	var info *amd64FunctionInfo
	prog := 0
	type pcBranch struct {
		info   *amd64FunctionInfo
		from   int
		offset int64
	}
	var branches []pcBranch
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			var err error
			info, err = t.newFunctionInfo(statement)
			if err != nil {
				return fmt.Errorf("line %d: %w", statement.Position().Line, err)
			}
			t.infos = append(t.infos, info)
			prog = 0
			t.progIndex[statement] = prog
		case *Label:
			if info == nil {
				return fmt.Errorf("line %d: label %s outside a function", statement.Position().Line, statement.Name)
			}
			info.labels[statement.Name] = fmt.Sprintf(".L%s_%d_%s", t.fileTag, len(t.infos)-1, sanitizeSymbol(statement.Name))
		case *Directive:
			switch statement.Name {
			case "PCDATA", "FUNCDATA", "PCALIGN":
				if info != nil {
					prog++
					t.progIndex[statement] = prog
				}
			case "GLOBL":
				if len(statement.Operands) > 0 {
					if symbol, ok := operandSymbol(statement.Operands[0]); ok && !symbol.Static {
						t.globalDefinitions[t.symbol(symbol)] = true
					}
				}
			}
		case *Instruction:
			if info == nil {
				return fmt.Errorf("line %d: instruction %s outside a function", statement.Position().Line, statement.Opcode)
			}
			prog++
			t.progIndex[statement] = prog
			if statement.Opcode == "CALL" {
				info.hasCall = true
			}
			for _, operand := range statement.Operands {
				if operand.Kind == OperandMemory && operand.Base == "PC" && isAMD64Branch(statement.Opcode) {
					offset, err := t.resolveInteger(operand.Offset)
					if err != nil {
						return fmt.Errorf("line %d: %w", statement.Position().Line, err)
					}
					branches = append(branches, pcBranch{info: info, from: prog, offset: offset})
				}
			}
		}
	}
	for _, branch := range branches {
		target := branch.from + int(branch.offset)
		if target < 0 {
			return fmt.Errorf("PC-relative branch before the start of %s", branch.info.symbol)
		}
		index := t.infoIndex(branch.info)
		branch.info.pcLabels[target] = fmt.Sprintf(".L%s_%d_pc_%d", t.fileTag, index, target)
	}
	for _, info := range t.infos {
		info.bpsize = 0
		info.autoffset = info.frame
		if !textHasFlag(info.text, "NOFRAME") && !(info.frame == 0 && !info.hasCall) {
			info.bpsize = 8
			info.autoffset += 8
		}
	}
	if err := t.collectLayouts(file); err != nil {
		return err
	}
	for _, info := range t.infos {
		t.classify(info)
	}
	return t.collectEdges(file)
}

func (t *amd64Translator) infoIndex(target *amd64FunctionInfo) int {
	for index, info := range t.infos {
		if info == target {
			return index
		}
	}
	return -1
}

func (t *amd64Translator) newFunctionInfo(text *Text) (*amd64FunctionInfo, error) {
	frame, err := parseSignedTextSize("frame", text.Frame)
	if err != nil {
		return nil, err
	}
	if frame < 0 {
		// obj6 treats a negative frame ($-8) as zero.
		frame = 0
	}
	if frame%8 != 0 {
		return nil, fmt.Errorf("unaligned stack size %d", frame)
	}
	info := &amd64FunctionInfo{
		text:          text,
		frame:         frame,
		labels:        make(map[string]string),
		pcLabels:      make(map[int]string),
		outputOffsets: make(map[int]bool),
	}
	if text.Args != "" {
		args, err := parseTextSize("arguments", text.Args)
		if err != nil {
			return nil, err
		}
		info.args = args
		info.argsKnown = true
	}
	info.symbol = t.symbol(text.Symbol)
	switch {
	case text.Symbol.Static:
		info.kind = amd64Static
		info.body = info.symbol
	case text.Symbol.ABI == "ABIInternal":
		info.kind = amd64ABIInternal
		info.body = info.symbol
		t.abiInternal[info.symbol] = true
	case text.Symbol.ABI == "" || text.Symbol.ABI == "ABI0":
		info.kind = amd64ABI0Wrapped
		info.body = abi0Symbol(info.symbol)
		t.abi0Defined[info.symbol] = true
	default:
		return nil, fmt.Errorf("unsupported ABI selector <%s> on %s", text.Symbol.ABI, text.Symbol.Raw)
	}
	return info, nil
}

// classify settles how an ABI0 function's plain name is provided.
func (t *amd64Translator) classify(info *amd64FunctionInfo) {
	if info.kind == amd64ABIInternal && len(info.layout.outputs) > 1 {
		info.kind = amd64ABIInternalAdapted
		info.body = abiInternalBodySymbol(info.symbol)
		t.adaptedBodies[info.symbol] = info.body
		return
	}
	if info.kind != amd64ABI0Wrapped {
		return
	}
	if info.symbol == "syscall_rawVforkSyscall" && t.options.PackagePath == "syscall" {
		info.kind = amd64ABI0Vfork
		return
	}
	hasArguments := len(info.layout.inputs) != 0 || len(info.layout.outputs) != 0 || info.argsKnown && info.args != 0
	if !hasArguments {
		info.kind = amd64ABI0Alias
	}
}

func parseSignedTextSize(kind, source string) (int, error) {
	if source == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(source, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid TEXT %s $%s", kind, source)
	}
	return int(parsed), nil
}

// collectEdges records, per function, the symbols it branches to and the
// symbols it materializes as addresses, spelled the way the emitted code
// spells them.
func (t *amd64Translator) collectEdges(file *File) error {
	index := -1
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			index++
		case *Instruction:
			if index < 0 {
				continue
			}
			info := t.infos[index]
			if isAMD64Branch(statement.Opcode) && len(statement.Operands) == 1 {
				operand := statement.Operands[0]
				if symbol, ok := operandSymbol(operand); ok {
					if statement.Opcode == "CALL" || statement.Opcode == "JMP" {
						info.calls = append(info.calls, t.branchSymbolName(symbol))
					}
					continue
				}
				if operand.Kind == OperandRegister || operand.Kind == OperandMemory && operand.Base != "PC" {
					info.indirect = true
				}
				continue
			}
			for _, operand := range statement.Operands {
				if operand.Kind != OperandAddress && !(operand.Kind == OperandMemory && statement.Opcode == "LEAQ") {
					continue
				}
				if operand.Base != "SB" {
					continue
				}
				info.addresses = append(info.addresses, t.addressSymbolName(operand.Symbol))
			}
		}
	}
	return nil
}

// branchSymbolName spells a direct branch target: ABIInternal functions and
// file-local functions by name, everything else through its ABI0 entry.
func (t *amd64Translator) branchSymbolName(symbol Symbol) string {
	name := t.symbol(symbol)
	if body, ok := t.adaptedBodies[name]; ok && (symbol.ABI == "ABIInternal" || symbol.ABI == "") {
		// Assembly speaks real ABIInternal, results in registers, so it
		// calls the body rather than the pointer-result adapter.
		return body
	}
	if symbol.Static || symbol.ABI == "ABIInternal" || symbol.ABI == "" && t.abiInternal[name] {
		return name
	}
	return abi0Symbol(name)
}

// addressSymbolName spells a materialized symbol address. Go resolves an
// unqualified function symbol in assembly to its ABI0 entry, which for an
// ABI0 function translated from this file is name_abi0. Other symbols (data,
// or functions compiled by cg12) keep their plain names, as on ARM64.
func (t *amd64Translator) addressSymbolName(symbol Symbol) string {
	name := t.symbol(symbol)
	if symbol.Static || symbol.ABI != "" {
		return name
	}
	if t.abi0Defined[name] {
		return abi0Symbol(name)
	}
	return name
}

func (t *amd64Translator) symbol(symbol Symbol) string {
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

func (t *amd64Translator) resolveInteger(source string) (int64, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return 0, nil
	}
	return ResolveIntegerExpression(source, t.options.Defines)
}

func (t *amd64Translator) translate(statement Statement) error {
	switch statement := statement.(type) {
	case *Preprocessor:
		if strings.HasPrefix(statement.Text, "#include ") {
			return nil
		}
		return fmt.Errorf("unsupported preprocessor directive %q", statement.Text)
	case *Text:
		return t.translateText(statement)
	case *Label:
		label := t.current.labels[statement.Name]
		fmt.Fprintf(&t.output, "%s:\n", label)
		return nil
	case *Directive:
		if err := t.emitPCLabel(statement); err != nil {
			return err
		}
		return t.translateDirective(statement)
	case *Instruction:
		if err := t.emitPCLabel(statement); err != nil {
			return err
		}
		return t.translateInstruction(statement)
	default:
		return fmt.Errorf("unsupported statement %T", statement)
	}
}

func (t *amd64Translator) emitPCLabel(statement Statement) error {
	index, ok := t.progIndex[statement]
	if !ok || t.current == nil {
		return nil
	}
	if label := t.current.pcLabels[index]; label != "" {
		fmt.Fprintf(&t.output, "%s:\n", label)
	}
	return nil
}

func (t *amd64Translator) translateText(text *Text) error {
	if err := t.finishFunction(); err != nil {
		return err
	}
	t.currentIndex++
	info := t.infos[t.currentIndex]
	t.current = info
	t.deltasp = 0
	flags := append([]string(nil), text.Flags...)

	switch info.kind {
	case amd64ABI0Wrapped:
		frame, frameStart, err := t.emitABI0Wrapper(info)
		if err != nil {
			return err
		}
		t.functions = append(t.functions, AMD64Function{
			Name:       info.symbol,
			Frame:      frame,
			FrameStart: frameStart,
			Args:       info.args,
			Flags:      append([]string(nil), flags...),
			Calls:      []string{info.body},
		})
	case amd64ABIInternalAdapted:
		frame, frameStart, err := t.emitABIInternalAdapter(info)
		if err != nil {
			return err
		}
		t.functions = append(t.functions, AMD64Function{
			Name:       info.symbol,
			Frame:      frame,
			FrameStart: frameStart,
			Args:       info.args,
			Flags:      append([]string(nil), flags...),
			Calls:      []string{info.body},
		})
	case amd64ABI0Vfork:
		t.emitRawVforkSyscallAMD64(info)
		t.functions = append(t.functions, AMD64Function{
			Name:  info.symbol,
			Args:  info.args,
			Flags: append([]string(nil), flags...),
		})
	}

	t.bodyFunction = len(t.functions)
	t.functions = append(t.functions, AMD64Function{
		Name:         info.body,
		Frame:        info.autoffset,
		FrameStart:   amd64PrologueSize(info),
		Args:         info.args,
		Flags:        flags,
		Calls:        uniqueSortedSymbols(info.calls),
		Indirect:     info.indirect,
		AddressTaken: uniqueSortedSymbols(info.addresses),
	})

	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", info.body)
	if info.kind != amd64ABIInternal {
		// Runtime metadata and ABI wrappers live in the separately emitted cg12
		// object, so implementation symbols must remain link-visible. Hidden
		// visibility keeps file-local and ABI0 entry points out of the public ABI.
		fmt.Fprintf(&t.output, ".hidden %s\n", info.body)
	}
	fmt.Fprintf(&t.output, ".type %s, @function\n", info.body)
	fmt.Fprintf(&t.output, "%s:\n", info.body)
	if info.kind == amd64ABI0Alias {
		fmt.Fprintf(&t.output, ".global %s\n", info.symbol)
		fmt.Fprintf(&t.output, ".type %s, @function\n", info.symbol)
		fmt.Fprintf(&t.output, "%s:\n", info.symbol)
	}
	if err := t.emitPCLabel(text); err != nil {
		return err
	}
	// Prologue, as cmd/internal/obj/x86's preprocess builds it. No stack-split
	// check is emitted: like the ARM64 translator, the translated body runs on
	// whatever stack it is entered on, and the NOSPLIT flag is reported in the
	// function metadata for the backend's nosplit budget.
	if info.bpsize > 0 {
		t.output.WriteString("\tpushq %rbp\n")
		t.output.WriteString("\tmovq %rsp, %rbp\n")
		t.deltasp += info.bpsize
	}
	if info.frame != 0 {
		t.emitAdjustSP(int64(info.frame))
		t.deltasp += info.frame
	}
	return nil
}

// amd64PrologueSize is the byte offset of the first body instruction, where
// SP has dropped by the full frame (autoffset): push %rbp (1 byte), mov
// %rsp,%rbp (3 bytes), and the frame subtraction (4 bytes for an 8-bit
// immediate, 7 otherwise).
func amd64PrologueSize(info *amd64FunctionInfo) int {
	size := 0
	if info.bpsize > 0 {
		size += 4
	}
	if info.frame != 0 {
		size += amd64AdjustSPSize(int64(info.frame))
	}
	return size
}

// emitAdjustSP lowers ADJSP $v the way asm6 does: SUBQ $v, SP, except that
// $0x80 and negative values other than -0x80 become ADDQ $-v, SP so the
// immediate stays as short as possible.
func (t *amd64Translator) emitAdjustSP(value int64) {
	switch {
	case value == 0:
	case value == 0x80 || value < 0 && value != -0x80:
		fmt.Fprintf(&t.output, "\taddq $%d, %%rsp\n", -value)
	default:
		fmt.Fprintf(&t.output, "\tsubq $%d, %%rsp\n", value)
	}
}

func amd64AdjustSPSize(value int64) int {
	if value == 0x80 || value < 0 && value != -0x80 {
		value = -value
	}
	if value >= -128 && value <= 127 {
		return 4
	}
	return 7
}

// finishFunction records what only the end of a function reveals.
func (t *amd64Translator) finishFunction() error {
	if t.current == nil {
		return nil
	}
	if t.current.spWrite {
		body := &t.functions[t.bodyFunction]
		body.Flags = append(body.Flags, "SPWRITE")
	}
	t.current = nil
	return nil
}

// emitEpilogue tears the frame down before a RET, as obj6 does.
func (t *amd64Translator) emitEpilogue() error {
	info := t.current
	if t.deltasp != info.autoffset {
		return fmt.Errorf("%s: unbalanced PUSH/POP (SP delta %d at RET, frame %d)", info.symbol, t.deltasp, info.autoffset)
	}
	if info.autoffset == 0 {
		return nil
	}
	if info.frame != 0 {
		t.emitAdjustSP(int64(-info.frame))
	}
	if info.bpsize > 0 {
		t.output.WriteString("\tpopq %rbp\n")
	}
	return nil
}

func isAMD64Branch(opcode string) bool {
	if opcode == "JMP" || opcode == "CALL" {
		return true
	}
	_, ok := amd64JumpMnemonics[opcode]
	return ok
}
