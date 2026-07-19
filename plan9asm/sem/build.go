package sem

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/evanphx/cg12/plan9asm"
)

type Options struct {
	PackagePath  string
	Filename     string
	Defines      map[string]int64
	FloatInputs  map[string][]int
	FloatOutputs map[string][]int
	Signatures   map[string]Signature
}

type builder struct {
	options    Options
	unit       Unit
	current    *Func
	references map[string]SymRef
	dataValues map[string][]DataItem
	fileTag    string
}

func Build(file *plan9asm.File, options Options) (*Unit, error) {
	b := &builder{
		options:    options,
		references: make(map[string]SymRef),
		dataValues: make(map[string][]DataItem),
		fileTag:    sanitize(options.PackagePath + "_" + strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))),
	}
	for _, statement := range file.Statements {
		directive, ok := statement.(*plan9asm.Directive)
		if !ok || directive.Name != "DATA" {
			continue
		}
		if err := b.recordData(directive); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", options.Filename, statement.Position().Line, err)
		}
	}
	for _, statement := range file.Statements {
		if err := b.statement(statement); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", options.Filename, statement.Position().Line, err)
		}
	}
	b.finishFunction()
	b.propagateAliasSignatures()
	for _, reference := range b.references {
		b.unit.References = append(b.unit.References, reference)
	}
	sort.Slice(b.unit.References, func(i, j int) bool {
		return b.unit.References[i].Name < b.unit.References[j].Name
	})
	return &b.unit, nil
}

func (b *builder) propagateAliasSignatures() {
	functions := make(map[string]*Func, len(b.unit.Funcs))
	for _, function := range b.unit.Funcs {
		functions[function.Sym.Name] = function
	}
	for range b.unit.Funcs {
		changed := false
		for _, function := range b.unit.Funcs {
			if len(function.Signature.Params) != 0 || len(function.Signature.Results) != 0 || len(function.Body) != 1 {
				continue
			}
			instruction, ok := function.Body[0].(Inst)
			if !ok || instruction.Op.Family != "branch" || len(instruction.Operands) != 1 {
				continue
			}
			operand := instruction.Operands[0]
			if operand.Kind != FunctionOperand {
				continue
			}
			target := functions[operand.Symbol.Name]
			if target == nil || len(target.Signature.Params) == 0 && len(target.Signature.Results) == 0 {
				continue
			}
			function.Signature = cloneSignature(target.Signature)
			changed = true
		}
		if !changed {
			return
		}
	}
}

func (b *builder) statement(statement plan9asm.Statement) error {
	switch statement := statement.(type) {
	case *plan9asm.Preprocessor:
		if strings.HasPrefix(statement.Text, "#include ") {
			return nil
		}
		return fmt.Errorf("unsupported preprocessor directive %q", statement.Text)
	case *plan9asm.Text:
		return b.startFunction(statement)
	case *plan9asm.Label:
		return b.appendStatement(Label{Pos: statement.Position(), Name: sanitize(statement.Name)})
	case *plan9asm.Instruction:
		return b.instruction(statement)
	case *plan9asm.Directive:
		return b.directive(statement)
	default:
		return fmt.Errorf("unsupported statement %T", statement)
	}
}

func (b *builder) startFunction(text *plan9asm.Text) error {
	b.finishFunction()
	frameSize, err := b.integer(text.Frame)
	if err != nil {
		return fmt.Errorf("TEXT frame: %w", err)
	}
	argumentSize := int64(0)
	if strings.TrimSpace(text.Args) != "" {
		argumentSize, err = b.integer(text.Args)
		if err != nil {
			return fmt.Errorf("TEXT argument size: %w", err)
		}
	}
	symbol := b.symbol(text.Symbol)
	function := &Func{
		Sym:       symbol,
		DeclABI:   declaredABI(text.Symbol.ABI),
		Flags:     append([]string(nil), text.Flags...),
		FrameSize: int(frameSize),
		ArgsSize:  int(argumentSize),
		Kind:      Bindable,
	}
	if signature, ok := b.options.Signatures[symbol.Name]; ok {
		function.Signature = cloneSignature(signature)
		function.signatureDeclared = true
	}
	if rawReason := intrinsicRawReason(symbol.Name); rawReason != "" {
		function.Kind = Raw
		function.Classification = rawReason
	}
	b.current = function
	return nil
}

func (b *builder) finishFunction() {
	if b.current == nil {
		return
	}
	if b.current.DeclABI == ABIInternal && len(b.current.Signature.Params) == 0 && len(b.current.Signature.Results) == 0 && b.current.Kind == Bindable {
		b.current.Kind = Raw
		b.current.Classification = "ABIInternal register intent requires a declared Go signature"
	}
	b.unit.Funcs = append(b.unit.Funcs, b.current)
	b.current = nil
}

func (b *builder) appendStatement(statement Statement) error {
	if b.current == nil {
		return fmt.Errorf("statement outside TEXT")
	}
	b.current.Body = append(b.current.Body, statement)
	return nil
}

func (b *builder) instruction(instruction *plan9asm.Instruction) error {
	if b.current == nil {
		return fmt.Errorf("instruction outside TEXT")
	}
	operation, err := normalizeOperation(instruction.Opcode)
	if err != nil {
		return err
	}
	semantic := Inst{
		Pos:    instruction.Position(),
		Op:     operation,
		Suffix: strings.ToLower(instruction.Suffix),
	}
	for index, sourceOperand := range instruction.Operands {
		operand, err := b.operand(instruction, index, sourceOperand)
		if err != nil {
			return err
		}
		semantic.Operands = append(semantic.Operands, operand)
	}
	b.classifyInstruction(instruction, semantic)
	b.current.Body = append(b.current.Body, semantic)
	return nil
}

func (b *builder) directive(directive *plan9asm.Directive) error {
	switch directive.Name {
	case "PCALIGN":
		if len(directive.Operands) != 1 {
			return fmt.Errorf("PCALIGN requires one alignment")
		}
		value, err := b.operandInteger(directive.Operands[0])
		if err != nil {
			return err
		}
		return b.appendStatement(Align{Pos: directive.Position(), Alignment: int(value)})
	case "FUNCDATA", "PCDATA", "NO_LOCAL_POINTERS":
		if b.current == nil {
			return fmt.Errorf("%s outside TEXT", directive.Name)
		}
		if directive.Name == "NO_LOCAL_POINTERS" {
			b.current.NoLocalPointers = true
		}
		return b.appendStatement(Directive{Pos: directive.Position(), Name: strings.ToLower(directive.Name)})
	case "GLOBL":
		return b.global(directive)
	case "DATA":
		return nil
	default:
		return fmt.Errorf("unsupported directive %s", directive.Name)
	}
}

func (b *builder) recordData(directive *plan9asm.Directive) error {
	if len(directive.Operands) != 2 || directive.Operands[1].Kind != plan9asm.OperandImmediate {
		return fmt.Errorf("DATA requires an address/width and immediate value")
	}
	symbol, offset, width, err := parseDataAddress(directive.Operands[0].Text)
	if err != nil {
		return err
	}
	resolved := b.symbol(symbol)
	item := DataItem{Offset: offset, Width: width}
	source := strings.TrimSpace(directive.Operands[1].Immediate)
	if strings.HasPrefix(source, "\"") {
		value, err := strconv.Unquote(source)
		if err != nil {
			return fmt.Errorf("invalid DATA string %q", directive.Operands[1].Text)
		}
		if len(value) > width {
			return fmt.Errorf("DATA string length %d does not fit in %d bytes", len(value), width)
		}
		item.Bytes = []byte(value)
		b.dataValues[resolved.Name] = append(b.dataValues[resolved.Name], item)
		return nil
	}
	if strings.HasSuffix(source, "(SB)") {
		if width != 4 && width != 8 {
			return fmt.Errorf("DATA relocation width %d is unsupported", width)
		}
		targetSource := strings.TrimSpace(strings.TrimSuffix(source, "(SB)"))
		targetName, addend := splitSymbolAddend(targetSource)
		target := b.symbol(parseDataSymbol(targetName))
		item.Symbol = &target
		item.Addend = addend
		b.reference(target)
		b.dataValues[resolved.Name] = append(b.dataValues[resolved.Name], item)
		return nil
	}
	value, err := strconv.ParseUint(source, 0, 64)
	if err != nil {
		return fmt.Errorf("invalid DATA value %q", directive.Operands[1].Text)
	}
	if width != 1 && width != 2 && width != 4 && width != 8 {
		return fmt.Errorf("numeric DATA width %d is unsupported", width)
	}
	if width < 8 && value >= uint64(1)<<(width*8) {
		return fmt.Errorf("DATA value %#x does not fit in %d bytes", value, width)
	}
	item.Value = int64(value)
	b.dataValues[resolved.Name] = append(b.dataValues[resolved.Name], item)
	return nil
}

func (b *builder) global(directive *plan9asm.Directive) error {
	if len(directive.Operands) < 2 || len(directive.Operands) > 3 {
		return fmt.Errorf("GLOBL requires a symbol, optional flags, and size")
	}
	symbol, symbolOffset, err := dataOperandSymbol(directive.Operands[0])
	if err != nil {
		return err
	}
	if symbolOffset != 0 {
		return fmt.Errorf("GLOBL symbol %q has nonzero offset", directive.Operands[0].Text)
	}
	sizeOperand := directive.Operands[len(directive.Operands)-1]
	if sizeOperand.Kind != plan9asm.OperandImmediate {
		return fmt.Errorf("invalid GLOBL size %q", sizeOperand.Text)
	}
	size, err := b.integer(sizeOperand.Immediate)
	if err != nil || size < 0 {
		return fmt.Errorf("invalid GLOBL size %q", sizeOperand.Text)
	}
	flags := ""
	if len(directive.Operands) == 3 {
		flags = directive.Operands[1].Text
	}
	resolved := b.symbol(symbol)
	data := &Data{
		Sym:        resolved,
		Size:       int(size),
		ReadOnly:   hasFlag(flags, "RODATA"),
		NoPointers: hasFlag(flags, "NOPTR"),
		Thread:     hasFlag(flags, "TLSBSS"),
		Duplicate:  hasFlag(flags, "DUPOK"),
		Items:      append([]DataItem(nil), b.dataValues[resolved.Name]...),
	}
	sort.Slice(data.Items, func(left, right int) bool {
		return data.Items[left].Offset < data.Items[right].Offset
	})
	written := 0
	for _, item := range data.Items {
		if item.Offset < written || item.Offset+item.Width > data.Size {
			return fmt.Errorf("DATA range %d/%d is invalid for %s size %d", item.Offset, item.Width, resolved.Name, data.Size)
		}
		written = item.Offset + item.Width
	}
	b.unit.Data = append(b.unit.Data, data)
	return nil
}

func parseDataAddress(source string) (plan9asm.Symbol, int, int, error) {
	source = strings.TrimSpace(source)
	slash := strings.LastIndexByte(source, '/')
	if slash < 0 {
		return plan9asm.Symbol{}, 0, 0, fmt.Errorf("DATA address %q has no width", source)
	}
	width, err := strconv.ParseUint(strings.TrimSpace(source[slash+1:]), 0, 32)
	if err != nil || width == 0 {
		return plan9asm.Symbol{}, 0, 0, fmt.Errorf("invalid DATA width in %q", source)
	}
	address := strings.TrimSpace(source[:slash])
	if !strings.HasSuffix(address, "(SB)") {
		return plan9asm.Symbol{}, 0, 0, fmt.Errorf("DATA address %q is not relative to SB", source)
	}
	name, offset := splitSymbolAddend(strings.TrimSpace(strings.TrimSuffix(address, "(SB)")))
	if name == "" || offset < 0 {
		return plan9asm.Symbol{}, 0, 0, fmt.Errorf("invalid DATA address %q", source)
	}
	return parseDataSymbol(name), int(offset), int(width), nil
}

func dataOperandSymbol(operand plan9asm.Operand) (plan9asm.Symbol, int64, error) {
	switch operand.Kind {
	case plan9asm.OperandSymbol:
		return operand.Symbol, 0, nil
	case plan9asm.OperandMemory:
		if operand.Base == "SB" {
			name, offset := splitSymbolAddend(operand.Offset)
			if name != "" {
				return parseDataSymbol(name), offset, nil
			}
		}
	}
	return plan9asm.Symbol{}, 0, fmt.Errorf("invalid GLOBL symbol %q", operand.Text)
}

func parseDataSymbol(source string) plan9asm.Symbol {
	source = strings.TrimSpace(source)
	symbol := plan9asm.Symbol{Raw: source}
	if strings.HasSuffix(source, "<>") {
		symbol.Static = true
		source = strings.TrimSpace(strings.TrimSuffix(source, "<>"))
	} else if end := strings.LastIndexByte(source, '>'); end == len(source)-1 {
		if start := strings.LastIndexByte(source, '<'); start >= 0 {
			symbol.ABI = source[start+1 : end]
			source = strings.TrimSpace(source[:start])
		}
	}
	symbol.Name = source
	return symbol
}

func splitSymbolAddend(source string) (string, int64) {
	for index := len(source) - 1; index > 0; index-- {
		if source[index] != '+' && source[index] != '-' {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(source[index:]), 0, 64)
		if err == nil {
			return strings.TrimSpace(source[:index]), value
		}
	}
	return strings.TrimSpace(source), 0
}

func hasFlag(flags, want string) bool {
	for _, flag := range strings.FieldsFunc(flags, func(character rune) bool {
		return character == '|' || character == '+'
	}) {
		if strings.TrimSpace(flag) == want {
			return true
		}
	}
	return false
}

func (b *builder) operand(instruction *plan9asm.Instruction, index int, source plan9asm.Operand) (Operand, error) {
	width := instructionWidth(instruction.Opcode)
	switch source.Kind {
	case plan9asm.OperandRegister:
		return Operand{Kind: RegisterOperand, Register: registerName(source.Register), Width: width}, nil
	case plan9asm.OperandImmediate:
		if strings.Contains(source.Immediate, "(") && strings.HasSuffix(source.Immediate, ")") {
			return b.addressOperand(source.Immediate, width)
		}
		value, err := b.integer(source.Immediate)
		if err != nil {
			if isFloatOperation(instruction.Opcode) {
				floatingPoint, floatErr := strconv.ParseFloat(strings.TrimSpace(source.Immediate), 64)
				if floatErr == nil {
					return Operand{Kind: FloatImmediateOperand, Float: floatingPoint, Width: width}, nil
				}
			}
			return Operand{}, err
		}
		return Operand{Kind: ImmediateOperand, Immediate: value, Width: width}, nil
	case plan9asm.OperandMemory:
		if source.Base == "SB" && isBranchOperation(instruction.Opcode) {
			symbol, offset, err := b.symbolAddress(source.Offset)
			if err != nil {
				return Operand{}, err
			}
			if offset != 0 {
				return Operand{}, fmt.Errorf("function target %q has offset %d", source.Text, offset)
			}
			b.reference(symbol)
			return Operand{Kind: FunctionOperand, Symbol: symbol, Width: width}, nil
		}
		return b.memoryOperand(instruction, index, source, width)
	case plan9asm.OperandSymbol:
		symbol := b.symbol(source.Symbol)
		b.reference(symbol)
		kind := FunctionOperand
		if instruction.Opcode != "B" && instruction.Opcode != "JMP" && instruction.Opcode != "BL" && instruction.Opcode != "CALL" {
			kind = SymbolAddressOperand
		}
		return Operand{Kind: kind, Symbol: symbol, Width: width}, nil
	case plan9asm.OperandRegisterPair:
		registers := make([]string, len(source.Registers))
		for index, register := range source.Registers {
			registers[index] = registerName(register)
		}
		return Operand{Kind: RegisterListOperand, Registers: registers, Width: width}, nil
	case plan9asm.OperandShiftedRegister:
		amount, err := b.integer(source.ShiftAmount)
		if err != nil {
			return Operand{}, err
		}
		return Operand{Kind: ShiftedOperand, Register: registerName(source.Register), Immediate: amount, Shift: source.Shift, Width: width}, nil
	case plan9asm.OperandExtendedRegister:
		amount := int64(0)
		var err error
		if source.ShiftAmount != "" {
			amount, err = b.integer(source.ShiftAmount)
			if err != nil {
				return Operand{}, err
			}
		}
		return Operand{Kind: ExtendedOperand, Register: registerName(source.Register), Immediate: amount, Extend: strings.ToLower(source.Extend), Width: width}, nil
	case plan9asm.OperandVectorRegister:
		lane := -1
		if source.Vector.Index != "" {
			value, err := b.integer(source.Vector.Index)
			if err != nil {
				return Operand{}, err
			}
			lane = int(value)
		}
		return Operand{Kind: VectorOperand, Register: registerName(source.Vector.Register), Arrangement: normalizeArrangement(source.Vector.Arrangement), Lane: lane, Width: width}, nil
	case plan9asm.OperandVectorList:
		registers := make([]string, len(source.Vectors))
		arrangement := ""
		for index, vector := range source.Vectors {
			registers[index] = registerName(vector.Register)
			if arrangement == "" {
				arrangement = normalizeArrangement(vector.Arrangement)
			}
		}
		return Operand{Kind: RegisterListOperand, Registers: registers, Arrangement: arrangement, Width: width}, nil
	case plan9asm.OperandRaw:
		return Operand{Kind: LabelOperand, Label: sanitize(source.Text), Width: width}, nil
	default:
		return Operand{}, fmt.Errorf("unsupported operand %q", source.Text)
	}
}

func (b *builder) memoryOperand(instruction *plan9asm.Instruction, index int, source plan9asm.Operand, width int) (Operand, error) {
	switch source.Base {
	case "FP":
		offset, name, err := frameOffset(source.Offset)
		if err != nil {
			return Operand{}, err
		}
		if offset < 0 {
			b.markRaw("direct access to caller frame")
			return Operand{Kind: CallerFrameOperand, Offset: int64(offset), Width: width}, nil
		}
		if b.current.Kind == Raw {
			return Operand{Kind: CallerFrameOperand, Offset: int64(offset), Width: width}, nil
		}
		if !isMoveOperation(instruction.Opcode) {
			b.markRaw("non-move access to caller frame")
		}
		if index == len(instruction.Operands)-1 {
			slot, err := b.bindSlot(&b.current.Signature.Results, "result", name, offset, width, isFloatOperation(instruction.Opcode))
			if err != nil {
				return Operand{}, err
			}
			return Operand{Kind: ResultOperand, Slot: slot, Width: width}, nil
		}
		slot, err := b.bindSlot(&b.current.Signature.Params, "parameter", name, offset, width, isFloatOperation(instruction.Opcode))
		if err != nil {
			return Operand{}, err
		}
		return Operand{Kind: ArgumentOperand, Slot: slot, Width: width, Signed: isSignedMove(instruction.Opcode)}, nil
	case "SP":
		offset, err := b.localOffset(source.Offset)
		if err != nil {
			return Operand{}, fmt.Errorf("local offset %q: %w", source.Offset, err)
		}
		return Operand{Kind: LocalOperand, Offset: offset, Width: width, Mode: addressMode(instruction.Suffix)}, nil
	case "SB":
		symbol, offset, err := b.symbolAddress(source.Offset)
		if err != nil {
			return Operand{}, err
		}
		b.reference(symbol)
		return Operand{
			Kind:   SymbolMemoryOperand,
			Symbol: symbol,
			Offset: offset,
			Width:  width,
			Mode:   addressMode(instruction.Suffix),
		}, nil
	default:
		offset, err := b.integer(zeroExpression(source.Offset))
		if err != nil {
			return Operand{}, fmt.Errorf("memory offset %q in %q: %w", source.Offset, source.Text, err)
		}
		return Operand{
			Kind:   MemoryOperand,
			Base:   registerName(source.Base),
			Index:  registerName(source.Index),
			Offset: offset,
			Width:  width,
			Mode:   addressMode(instruction.Suffix),
		}, nil
	}
}

func (b *builder) addressOperand(source string, width int) (Operand, error) {
	source = strings.TrimSpace(source)
	open := strings.LastIndexByte(source, '(')
	if open < 0 || !strings.HasSuffix(source, ")") {
		return Operand{}, fmt.Errorf("invalid address operand %q", source)
	}
	prefix := strings.TrimSpace(source[:open])
	base := strings.ToUpper(strings.TrimSpace(source[open+1 : len(source)-1]))
	switch base {
	case "FP":
		offset, name, err := frameOffset(prefix)
		if err != nil {
			return Operand{}, err
		}
		if offset < 0 {
			b.markRaw("takes address in caller frame")
			return Operand{Kind: CallerFrameAddressOperand, Offset: int64(offset), Width: 8}, nil
		}
		if b.current.Kind == Raw {
			return Operand{Kind: CallerFrameAddressOperand, Offset: int64(offset), Width: 8}, nil
		}
		if slot := slotAtOffset(b.current.Signature.Results, offset); slot >= 0 {
			return Operand{Kind: ResultAddressOperand, Slot: slot, Width: 8}, nil
		}
		slot, err := b.bindSlot(&b.current.Signature.Params, "parameter", name, offset, 0, false)
		if err != nil {
			return Operand{}, err
		}
		return Operand{Kind: ArgumentAddressOperand, Slot: slot, Width: 8}, nil
	case "SP":
		offset, err := b.localOffset(prefix)
		if err != nil {
			return Operand{}, err
		}
		return Operand{Kind: LocalAddressOperand, Offset: offset, Width: 8}, nil
	case "SB":
		symbol, offset, err := b.symbolAddress(prefix)
		if err != nil {
			return Operand{}, err
		}
		b.reference(symbol)
		return Operand{Kind: SymbolAddressOperand, Symbol: symbol, Offset: offset, Width: 8}, nil
	default:
		offset, err := b.integer(zeroExpression(prefix))
		if err != nil {
			return Operand{}, err
		}
		return Operand{Kind: RegisterAddressOperand, Base: registerName(base), Offset: offset, Width: 8}, nil
	}
}

func (b *builder) classifyInstruction(source *plan9asm.Instruction, semantic Inst) {
	if source.Opcode == "BL" || source.Opcode == "CALL" {
		if len(semantic.Operands) != 1 || semantic.Operands[0].Kind != FunctionOperand {
			b.markRaw("indirect call")
		}
	}
	if (source.Opcode == "B" || source.Opcode == "JMP") && len(semantic.Operands) == 1 {
		operand := semantic.Operands[0]
		if operand.Kind == RegisterOperand || operand.Kind == MemoryOperand {
			b.markRaw("indirect branch")
		}
	}
	if source.Opcode == "RET" && len(semantic.Operands) != 0 {
		b.markRaw("non-standard return register")
	}
	if len(source.Operands) > 0 {
		destination := source.Operands[len(source.Operands)-1]
		if destination.Kind == plan9asm.OperandRegister && (destination.Register == "RSP" || destination.Register == "SP") {
			b.markRaw("writes hardware stack pointer")
		}
	}
}

func (b *builder) markRaw(reason string) {
	if b.current.Kind == Raw {
		return
	}
	b.current.Kind = Raw
	b.current.Classification = reason
}

func (b *builder) integer(source string) (int64, error) {
	expression := zeroExpression(source)
	value, err := plan9asm.ResolveIntegerExpression(expression, b.options.Defines)
	if err != nil {
		return 0, fmt.Errorf("integer expression %q: %w", expression, err)
	}
	return value, nil
}

func (b *builder) operandInteger(operand plan9asm.Operand) (int64, error) {
	if operand.Kind != plan9asm.OperandImmediate {
		return 0, fmt.Errorf("%q is not an immediate", operand.Text)
	}
	return b.integer(operand.Immediate)
}

func (b *builder) localOffset(source string) (int64, error) {
	if offset, _, err := frameOffset(source); err == nil {
		return int64(offset), nil
	}
	return b.integer(zeroExpression(source))
}

func (b *builder) symbol(symbol plan9asm.Symbol) SymRef {
	name := symbol.Name
	if strings.HasPrefix(name, "·") {
		name = b.options.PackagePath + name
	}
	name = sanitize(name)
	if symbol.Static {
		name = ".L" + b.fileTag + "_" + name
	}
	return SymRef{Name: name, Static: symbol.Static}
}

func (b *builder) reference(symbol SymRef) {
	if symbol.Static {
		return
	}
	b.references[symbol.Name] = symbol
}

func (b *builder) symbolAddress(source string) (SymRef, int64, error) {
	source = strings.TrimSpace(source)
	name := source
	offset := int64(0)
	for index := len(source) - 1; index > 0; index-- {
		if source[index] != '+' && source[index] != '-' {
			continue
		}
		valueSource := strings.TrimSpace(source[index+1:])
		value, err := b.integer(valueSource)
		if err != nil {
			continue
		}
		if source[index] == '-' {
			value = -value
		}
		name = strings.TrimSpace(source[:index])
		offset = value
		break
	}
	if name == "" {
		return SymRef{}, 0, fmt.Errorf("symbol address %q has no symbol", source)
	}
	if strings.Contains(name, "+") || strings.Contains(name, "-") {
		return SymRef{}, 0, fmt.Errorf("unresolved symbol offset in %q", source)
	}
	return b.symbol(parseDataSymbol(name)), offset, nil
}

func declaredABI(name string) ABIKind {
	switch name {
	case "ABIInternal":
		return ABIInternal
	case "ABI0":
		return ABI0
	default:
		return ABIDefault
	}
}

func ensureSlot(slots *[]Slot, name string, offset, width int, floatingPoint bool) int {
	for index := range *slots {
		if (*slots)[index].Offset == offset {
			if width > (*slots)[index].Width {
				(*slots)[index].Width = width
			}
			(*slots)[index].Float = (*slots)[index].Float || floatingPoint
			return index
		}
	}
	group := 0
	if strings.HasSuffix(name, "_base") || strings.HasSuffix(name, "_len") || strings.HasSuffix(name, "_cap") {
		group = offset/8 + 1
	}
	*slots = append(*slots, Slot{Name: name, Offset: offset, Width: width, Float: floatingPoint, Group: group})
	return len(*slots) - 1
}

func slotAtOffset(slots []Slot, offset int) int {
	for index, slot := range slots {
		if slot.Offset == offset {
			return index
		}
	}
	return -1
}

func (b *builder) bindSlot(slots *[]Slot, role, name string, offset, width int, floatingPoint bool) (int, error) {
	for index, slot := range *slots {
		if slot.Offset != offset {
			continue
		}
		if b.current.signatureDeclared {
			if width > slot.Width && role != "result" {
				return 0, fmt.Errorf("%s slot %s+%d reads %d bytes, declaration provides %d", role, name, offset, width, slot.Width)
			}
			if width > 0 && floatingPoint && !slot.Float {
				return 0, fmt.Errorf("%s slot %s+%d floating-point class disagrees with declaration", role, name, offset)
			}
		}
		if !b.current.signatureDeclared && width > (*slots)[index].Width {
			(*slots)[index].Width = width
		}
		return index, nil
	}
	if b.current.signatureDeclared {
		return 0, fmt.Errorf("%s slot %s+%d is absent from the Go declaration", role, name, offset)
	}
	return ensureSlot(slots, name, offset, width, floatingPoint), nil
}

func cloneSignature(signature Signature) Signature {
	return Signature{
		Params:  append([]Slot(nil), signature.Params...),
		Results: append([]Slot(nil), signature.Results...),
	}
}

func frameOffset(source string) (int, string, error) {
	source = strings.TrimSpace(source)
	if value, err := strconv.ParseInt(source, 0, 32); err == nil {
		return int(value), "", nil
	}
	for index := len(source) - 1; index > 0; index-- {
		if source[index] != '+' && source[index] != '-' {
			continue
		}
		value, err := strconv.ParseInt(source[index:], 0, 32)
		if err == nil {
			return int(value), strings.TrimSpace(source[:index]), nil
		}
	}
	return 0, "", fmt.Errorf("invalid named frame offset %q", source)
}

func zeroExpression(source string) string {
	if strings.TrimSpace(source) == "" {
		return "0"
	}
	return source
}

func addressMode(suffix string) AddressMode {
	switch suffix {
	case "P":
		return AddressPostIndex
	case "W":
		return AddressPreIndex
	default:
		return AddressOffset
	}
}

func registerName(name string) string {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	switch name {
	case "RSP", "SP":
		return "sp"
	case "ZR":
		return "zr"
	case "LR":
		return "x30"
	case "FP":
		return "x29"
	case "G":
		return "g"
	}
	if strings.HasPrefix(name, "R") {
		return "x" + strings.TrimPrefix(name, "R")
	}
	if strings.HasPrefix(name, "F") || strings.HasPrefix(name, "V") {
		return "v" + strings.TrimPrefix(name, name[:1])
	}
	return strings.ToLower(name)
}

func normalizeArrangement(arrangement string) string {
	if arrangement == "" {
		return ""
	}
	element := strings.ToLower(arrangement[:1])
	count := strings.TrimPrefix(arrangement, arrangement[:1])
	return count + element
}

func sanitize(name string) string {
	var output strings.Builder
	for _, character := range name {
		if character == '_' || unicode.IsLetter(character) || unicode.IsDigit(character) {
			if character < unicode.MaxASCII {
				output.WriteRune(character)
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

func intrinsicRawReason(name string) string {
	rawNames := []string{
		"runtime_gogo",
		"runtime_mcall",
		"runtime_systemstack",
		"runtime_systemstack_switch",
		"runtime_morestack",
		"runtime_morestack_noctxt",
		"runtime_asyncPreempt",
		"runtime_sigtramp",
		"runtime_rt0_go",
		"_rt0_arm64_linux",
		"reflect_makeFuncStub",
		"reflect_methodValueCall",
	}
	for _, raw := range rawNames {
		if name == raw || strings.HasPrefix(name, raw+"_") {
			return "runtime machine-state contract"
		}
	}
	return ""
}

func instructionWidth(opcode string) int {
	switch {
	case strings.HasSuffix(opcode, "B") && !strings.HasPrefix(opcode, "B"):
		return 1
	case strings.HasSuffix(opcode, "H"):
		return 2
	case strings.HasSuffix(opcode, "W"), opcode == "MOVWU", opcode == "FMOVS":
		return 4
	case strings.HasPrefix(opcode, "V"), strings.HasPrefix(opcode, "SHA"), strings.HasPrefix(opcode, "AES"):
		return 16
	default:
		return 8
	}
}

func isMoveOperation(opcode string) bool {
	return strings.HasPrefix(opcode, "MOV") || strings.HasPrefix(opcode, "FMOV")
}

func isBranchOperation(opcode string) bool {
	switch opcode {
	case "B", "JMP", "BL", "CALL":
		return true
	}
	return false
}

func isFloatOperation(opcode string) bool {
	return strings.HasPrefix(opcode, "F")
}

func isSignedMove(opcode string) bool {
	return opcode == "MOVB" || opcode == "MOVH" || opcode == "MOVW"
}

func normalizeOperation(opcode string) (Operation, error) {
	width := instructionWidth(opcode)
	switch {
	case strings.HasPrefix(opcode, "MOV"):
		return Operation{Family: "move", Width: width, Variant: moveVariant(opcode)}, nil
	case strings.HasPrefix(opcode, "FMOV"):
		return Operation{Family: "float-move", Width: width}, nil
	case strings.HasPrefix(opcode, "VLD"):
		return Operation{Family: "vector-load", Width: width, Variant: strings.ToLower(strings.TrimPrefix(opcode, "V"))}, nil
	case strings.HasPrefix(opcode, "VST"):
		return Operation{Family: "vector-store", Width: width, Variant: strings.ToLower(strings.TrimPrefix(opcode, "V"))}, nil
	case strings.HasPrefix(opcode, "V"):
		return Operation{Family: "vector-" + strings.ToLower(strings.TrimPrefix(opcode, "V")), Width: width}, nil
	case strings.HasPrefix(opcode, "SHA"), strings.HasPrefix(opcode, "AES"), strings.HasPrefix(opcode, "CRC"):
		return Operation{Family: "crypto", Width: width, Variant: strings.ToLower(opcode)}, nil
	}
	families := map[string]string{
		"ADD": "add", "ADDW": "add", "ADDS": "add-set-flags", "ADDSW": "add-set-flags",
		"ADC": "add-carry", "ADCW": "add-carry", "ADCS": "add-carry-set-flags", "ADCSW": "add-carry-set-flags",
		"SUB": "sub", "SUBW": "sub", "SUBS": "sub-set-flags", "SUBSW": "sub-set-flags",
		"SBC": "sub-carry", "SBCW": "sub-carry", "SBCS": "sub-carry-set-flags", "SBCSW": "sub-carry-set-flags",
		"AND": "and", "ANDW": "and", "ANDS": "and-set-flags", "ANDSW": "and-set-flags",
		"BIC": "bit-clear", "BICW": "bit-clear", "BICS": "bit-clear-set-flags", "BICSW": "bit-clear-set-flags",
		"ORR": "or", "ORRW": "or", "EOR": "xor", "EORW": "xor",
		"LSL": "shift-left", "LSLW": "shift-left", "LSR": "shift-right", "LSRW": "shift-right",
		"ASR": "shift-right-signed", "ASRW": "shift-right-signed",
		"B": "branch", "JMP": "branch", "BL": "call", "CALL": "call", "RET": "return",
		"NOP": "nop", "NOOP": "nop", "WORD": "raw-word", "UNDEF": "undefined",
	}
	if family := families[opcode]; family != "" {
		return Operation{Family: family, Width: width}, nil
	}
	// The remaining names already denote architectural operation families rather
	// than Plan 9 pseudo operations. Lowering handles their operand order and
	// suffix explicitly.
	knownPrefixes := []string{
		"B", "CB", "TB", "CMP", "CMN", "TST", "CCMP", "CSEL", "CINC", "CNEG", "CSET",
		"MUL", "UMULH", "UDIV", "NEG", "RBIT", "CLZ", "ROR", "REV", "UBFX", "SBFX",
		"DMB", "DSB", "ISB", "MRS", "MSR", "DC", "PRFM", "SVC", "BRK",
		"LDAR", "LDAXR", "STLR", "STLXR", "SWPAL", "CASAL", "LDADDAL", "LDCLRAL", "LDORAL", "MVN",
		"LDP", "STP", "FLDP", "FSTP", "FADD", "FSUB", "FMUL", "FNMUL", "FMADD", "FMSUB", "FNMSUB", "FDIV", "FMAX", "FMIN", "FABS", "FRINT", "FCMP", "FCSEL", "FCVT", "SCVTF",
	}
	for _, prefix := range knownPrefixes {
		if strings.HasPrefix(opcode, prefix) {
			return Operation{Family: strings.ToLower(prefix), Width: width, Variant: strings.ToLower(opcode)}, nil
		}
	}
	return Operation{}, fmt.Errorf("unsupported semantic ARM64 instruction %s", opcode)
}

func moveVariant(opcode string) string {
	switch opcode {
	case "MOVB", "MOVH", "MOVW":
		return "sign-extend"
	case "MOVBU", "MOVHU", "MOVWU":
		return "zero-extend"
	default:
		return "bits"
	}
}
