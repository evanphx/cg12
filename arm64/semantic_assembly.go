package arm64

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/evanphx/cg12/ir"
	plan9sem "github.com/evanphx/cg12/plan9asm/sem"
)

// lowerSemanticAssembly turns bindable Plan 9 assembly functions into ordinary
// cg12 functions. Pseudo-register operands have already been dissolved by the
// semantic package, so the inline machine sequence only contains architectural
// registers, local labels, and explicitly bound IR values.
type semanticAssemblyPolicy struct {
	CallConv     ir.CallConvention
	ManagedFrame bool
}

func lowerSemanticAssembly(unit *plan9sem.Unit, policy semanticAssemblyPolicy) ([]*ir.Func, error) {
	module := ir.NewModule()
	for _, source := range unit.Funcs {
		if source.Kind != plan9sem.Bindable {
			return nil, fmt.Errorf("%s is raw assembly: %s", source.Sym.Name, source.Classification)
		}
		if _, err := lowerSemanticFunction(module, source, policy); err != nil {
			return nil, fmt.Errorf("%s: %w", source.Sym.Name, err)
		}
	}
	return module.Funcs, nil
}

func lowerSemanticFunction(module *ir.Module, source *plan9sem.Func, policy semanticAssemblyPolicy) (*ir.Func, error) {
	var function *ir.Func
	if len(source.Signature.Results) > 0 {
		function = module.NewFunc(source.Sym.Name, semanticSlotClass(source.Signature.Results[0]))
	} else {
		function = module.NewFuncVoid(source.Sym.Name)
	}
	function.Export()
	function.CallConv = policy.CallConv
	function.ManagedFrame = policy.ManagedFrame
	function.NoSplit = semanticFlag(source.Flags, "NOSPLIT")

	parameters := make([]ir.Ref, len(source.Signature.Params))
	for index, slot := range source.Signature.Params {
		class := semanticSlotClass(slot)
		parameter := function.Param(slot.Name, class)
		if slot.GCRef {
			function.MarkGCRef(parameter)
		}
		parameters[index] = parameter
	}
	resultPointers := make([]ir.Ref, len(source.Signature.Results))
	for index := 1; index < len(source.Signature.Results); index++ {
		name := source.Signature.Results[index].Name + ".result"
		pointer := function.ParamRef(name)
		parameters = append(parameters, pointer)
		resultPointers[index] = pointer
	}

	if target, ok := semanticTailTarget(source); ok {
		lowerSemanticTailCall(function, target, parameters, policy.CallConv)
		return function, nil
	}

	renderer := semanticFunctionRenderer{
		function:       function,
		source:         source,
		parameters:     parameters,
		resultPointers: resultPointers,
		symbols:        make(map[semanticSymbolKey]int),
	}
	template, specs, err := renderer.render()
	if err != nil {
		return nil, err
	}
	block := function.Entry()
	results := block.Asm(template, specs)
	assembly := &block.Instrs[len(block.Instrs)-1]
	assembly.Asm.ExactClobbers = true
	assembly.Asm.Clobbers = semanticClobbers(source)
	if len(source.Signature.Results) > 0 {
		if len(results) != 1 {
			return nil, fmt.Errorf("assembly result was not bound")
		}
		block.Ret(results[0])
	} else {
		block.RetVoid()
	}
	return function, nil
}

func semanticClobbers(function *plan9sem.Func) []string {
	registers := make(map[string]bool)
	for _, statement := range function.Body {
		instruction, ok := statement.(plan9sem.Inst)
		if !ok {
			continue
		}
		for _, operand := range instruction.Operands {
			for _, register := range append([]string{operand.Register, operand.Base, operand.Index}, operand.Registers...) {
				if strings.HasPrefix(register, "x") {
					registers[register] = true
				}
			}
		}
	}
	clobbers := make([]string, 0, len(registers)+2)
	for register := range registers {
		clobbers = append(clobbers, register)
	}
	sort.Slice(clobbers, func(left, right int) bool {
		leftNumber, _ := strconv.Atoi(strings.TrimPrefix(clobbers[left], "x"))
		rightNumber, _ := strconv.Atoi(strings.TrimPrefix(clobbers[right], "x"))
		return leftNumber < rightNumber
	})
	return append(clobbers, "cc", "memory")
}

func lowerSemanticTailCall(function *ir.Func, target plan9sem.SymRef, parameters []ir.Ref, convention ir.CallConvention) {
	block := function.Entry()
	callee := function.Sym(target.Name, 0)
	if function.HasRet {
		result := block.CallWithConvention(function.Retty, convention, callee, parameters...)
		block.Instrs[len(block.Instrs)-1].Tail = true
		block.Ret(result)
		return
	}
	block.CallVoidWithConvention(convention, callee, parameters...)
	block.Instrs[len(block.Instrs)-1].Tail = true
	block.RetVoid()
}

func semanticTailTarget(function *plan9sem.Func) (plan9sem.SymRef, bool) {
	if len(function.Body) != 1 {
		return plan9sem.SymRef{}, false
	}
	instruction, ok := function.Body[0].(plan9sem.Inst)
	if !ok || instruction.Op.Family != "branch" || len(instruction.Operands) != 1 {
		return plan9sem.SymRef{}, false
	}
	target := instruction.Operands[0]
	if target.Kind != plan9sem.FunctionOperand {
		return plan9sem.SymRef{}, false
	}
	return target.Symbol, true
}

func semanticSlotClass(slot plan9sem.Slot) ir.Cls {
	if slot.Float {
		if slot.Width <= 4 {
			return ir.ClsS
		}
		return ir.ClsD
	}
	if slot.GCRef {
		return ir.ClsP
	}
	if slot.Width <= 4 {
		return ir.ClsW
	}
	return ir.ClsL
}

func semanticFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

type semanticSymbolKey struct {
	name   string
	offset int64
}

type semanticFunctionRenderer struct {
	function       *ir.Func
	source         *plan9sem.Func
	parameters     []ir.Ref
	resultPointers []ir.Ref
	specs          []ir.AsmSpec
	symbols        map[semanticSymbolKey]int
	output         strings.Builder
}

func (r *semanticFunctionRenderer) render() (string, []ir.AsmSpec, error) {
	if len(r.source.Signature.Results) > 0 {
		r.specs = append(r.specs, ir.AsmSpec{
			Kind: ir.AsmRegOut,
			Cls:  semanticSlotClass(r.source.Signature.Results[0]),
		})
	}
	for _, parameter := range r.parameters {
		r.specs = append(r.specs, ir.AsmSpec{Kind: ir.AsmRegIn, Ref: parameter})
	}

	for _, statement := range r.source.Body {
		switch statement := statement.(type) {
		case plan9sem.Label:
			fmt.Fprintf(&r.output, "%s:\n", statement.Name)
		case plan9sem.Align:
			fmt.Fprintf(&r.output, ".balign %d\n", statement.Alignment)
		case plan9sem.Directive:
			// Go pointer metadata is reconstructed from the IR signature and frame.
			// It is deliberately not forwarded as assembler text.
			continue
		case plan9sem.Inst:
			line, err := r.instruction(statement)
			if err != nil {
				return "", nil, fmt.Errorf("line %d: %w", statement.Pos.Line, err)
			}
			if line != "" {
				r.output.WriteByte('\t')
				r.output.WriteString(line)
				r.output.WriteByte('\n')
			}
		default:
			return "", nil, fmt.Errorf("unsupported semantic statement %T", statement)
		}
	}
	r.output.WriteString("semantic_return:\n")
	return r.output.String(), r.specs, nil
}

func (r *semanticFunctionRenderer) instruction(instruction plan9sem.Inst) (string, error) {
	switch instruction.Op.Family {
	case "move":
		return r.move(instruction)
	case "add", "and", "or", "xor", "bit-clear":
		return r.alu(instruction)
	case "mvn", "neg":
		return r.unary(instruction.Op.Family, instruction)
	case "cmp", "cmn":
		return r.compare(instruction)
	case "cset":
		return r.conditionalSet(instruction)
	case "branch":
		return r.branch("b", instruction)
	case "b":
		return r.conditionalBranch(instruction)
	case "cb":
		return r.compareBranch(instruction)
	case "return":
		return "b semantic_return", nil
	case "ldar", "ldaxr":
		return r.atomicLoad(instruction)
	case "stlr":
		return r.atomicStore(instruction)
	case "stlxr":
		return r.atomicStoreExclusive(instruction)
	case "swpal", "casal", "ldaddal", "ldclral", "ldoral":
		return r.lseAtomic(instruction)
	case "mrs":
		return r.systemRegisterRead(instruction)
	case "msr":
		return r.systemRegisterWrite(instruction)
	case "ubfx", "sbfx":
		return r.bitfieldExtract(instruction)
	case "dmb", "dsb", "isb":
		return r.barrier(instruction)
	case "svc":
		return r.supervisorCall(instruction)
	default:
		return "", fmt.Errorf("unsupported operation %s (%s)", instruction.Op.Family, instruction.Op.Variant)
	}
}

func (r *semanticFunctionRenderer) systemRegisterRead(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 || instruction.Operands[1].Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("mrs requires a system register and destination")
	}
	register := semanticLabel(instruction.Operands[0])
	if register == "" {
		return "", fmt.Errorf("mrs system register is invalid")
	}
	return fmt.Sprintf("mrs %s, %s",
		semanticRegister(instruction.Operands[1].Register, 8), strings.ToLower(register)), nil
}

func (r *semanticFunctionRenderer) systemRegisterWrite(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 {
		return "", fmt.Errorf("msr requires a source and system register")
	}
	register := semanticLabel(instruction.Operands[1])
	if register == "" {
		return "", fmt.Errorf("msr system register is invalid")
	}
	source, err := r.generalOperand(instruction.Operands[0], 8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("msr %s, %s", strings.ToLower(register), source), nil
}

func (r *semanticFunctionRenderer) bitfieldExtract(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 4 {
		return "", fmt.Errorf("%s requires an offset, source, width, and destination", instruction.Op.Family)
	}
	bitOffset := instruction.Operands[0]
	source := instruction.Operands[1]
	bitWidth := instruction.Operands[2]
	destination := instruction.Operands[3]
	if bitOffset.Kind != plan9sem.ImmediateOperand || bitWidth.Kind != plan9sem.ImmediateOperand ||
		source.Kind != plan9sem.RegisterOperand || destination.Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("%s operands are invalid", instruction.Op.Family)
	}
	return fmt.Sprintf("%s %s, %s, #%d, #%d", instruction.Op.Family,
		semanticRegister(destination.Register, instruction.Op.Width),
		semanticRegister(source.Register, instruction.Op.Width), bitOffset.Immediate, bitWidth.Immediate), nil
}

func (r *semanticFunctionRenderer) barrier(instruction plan9sem.Inst) (string, error) {
	if instruction.Op.Family == "isb" && len(instruction.Operands) == 0 {
		return "isb sy", nil
	}
	if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != plan9sem.ImmediateOperand {
		return "", fmt.Errorf("%s requires one immediate option", instruction.Op.Family)
	}
	options := map[int64]string{
		2:  "oshst",
		3:  "osh",
		6:  "nshst",
		7:  "nsh",
		10: "ishst",
		11: "ish",
		14: "st",
		15: "sy",
	}
	option := options[instruction.Operands[0].Immediate]
	if option == "" {
		return "", fmt.Errorf("%s option %d is unsupported", instruction.Op.Family, instruction.Operands[0].Immediate)
	}
	return instruction.Op.Family + " " + option, nil
}

func (r *semanticFunctionRenderer) supervisorCall(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) == 0 {
		return "svc #0", nil
	}
	if len(instruction.Operands) != 1 || instruction.Operands[0].Kind != plan9sem.ImmediateOperand {
		return "", fmt.Errorf("svc accepts at most one immediate")
	}
	return fmt.Sprintf("svc #%d", instruction.Operands[0].Immediate), nil
}

func (r *semanticFunctionRenderer) move(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 {
		return "", fmt.Errorf("move requires two operands")
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]

	if source.Kind == plan9sem.ArgumentOperand && destination.Kind == plan9sem.RegisterOperand {
		parameter, err := r.parameterOperand(source)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("mov %s, %s", semanticRegister(destination.Register, instruction.Op.Width), parameter), nil
	}
	if source.Kind == plan9sem.RegisterOperand && destination.Kind == plan9sem.ResultOperand {
		result, err := r.resultOperand(destination)
		if err != nil {
			return "", err
		}
		width := resultWidth(r.source, destination.Slot)
		if destination.Slot == 0 {
			return fmt.Sprintf("mov %s, %s", result, semanticRegister(source.Register, width)), nil
		}
		return fmt.Sprintf("%s %s, [%s]", semanticStoreMnemonic(width),
			semanticRegister(source.Register, width), result), nil
	}
	if source.Kind == plan9sem.SymbolMemoryOperand && destination.Kind == plan9sem.RegisterOperand {
		address := r.symbolOperand(source.Symbol, source.Offset)
		mnemonic := semanticLoadMnemonic(instruction.Op.Width, instruction.Op.Variant)
		return fmt.Sprintf("%s %s, [%s]", mnemonic, semanticRegister(destination.Register, instruction.Op.Width), address), nil
	}
	if source.Kind == plan9sem.RegisterOperand && destination.Kind == plan9sem.RegisterOperand {
		return fmt.Sprintf("mov %s, %s",
			semanticRegister(destination.Register, instruction.Op.Width),
			semanticRegister(source.Register, instruction.Op.Width)), nil
	}
	if source.Kind == plan9sem.ImmediateOperand && destination.Kind == plan9sem.RegisterOperand {
		return fmt.Sprintf("mov %s, #%d",
			semanticRegister(destination.Register, instruction.Op.Width), source.Immediate), nil
	}
	return "", fmt.Errorf("unsupported move operands %v, %v", source.Kind, destination.Kind)
}

func (r *semanticFunctionRenderer) alu(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 && len(instruction.Operands) != 3 {
		return "", fmt.Errorf("%s requires two or three operands", instruction.Op.Family)
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[len(instruction.Operands)-1]
	left := destination
	if len(instruction.Operands) == 3 {
		left = instruction.Operands[1]
	}
	if destination.Kind != plan9sem.RegisterOperand || left.Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("%s requires register destinations", instruction.Op.Family)
	}
	right, err := r.generalOperand(source, instruction.Op.Width)
	if err != nil {
		return "", err
	}
	mnemonic := map[string]string{
		"add":       "add",
		"and":       "and",
		"or":        "orr",
		"xor":       "eor",
		"bit-clear": "bic",
	}[instruction.Op.Family]
	return fmt.Sprintf("%s %s, %s, %s", mnemonic,
		semanticRegister(destination.Register, instruction.Op.Width),
		semanticRegister(left.Register, instruction.Op.Width), right), nil
}

func (r *semanticFunctionRenderer) unary(mnemonic string, instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 {
		return "", fmt.Errorf("%s requires two operands", mnemonic)
	}
	source := instruction.Operands[0]
	destination := instruction.Operands[1]
	if source.Kind != plan9sem.RegisterOperand || destination.Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("%s requires registers", mnemonic)
	}
	return fmt.Sprintf("%s %s, %s", mnemonic,
		semanticRegister(destination.Register, instruction.Op.Width),
		semanticRegister(source.Register, instruction.Op.Width)), nil
}

func (r *semanticFunctionRenderer) compare(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 || instruction.Operands[1].Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("compare requires a source and register")
	}
	right, err := r.generalOperand(instruction.Operands[0], instruction.Op.Width)
	if err != nil {
		return "", err
	}
	left := semanticRegister(instruction.Operands[1].Register, instruction.Op.Width)
	return fmt.Sprintf("%s %s, %s", instruction.Op.Family, left, right), nil
}

func (r *semanticFunctionRenderer) conditionalSet(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 || instruction.Operands[1].Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("cset requires a condition and destination")
	}
	condition := semanticLabel(instruction.Operands[0])
	if condition == "" {
		return "", fmt.Errorf("cset condition is invalid")
	}
	return fmt.Sprintf("cset %s, %s", semanticRegister(instruction.Operands[1].Register, instruction.Op.Width), strings.ToLower(condition)), nil
}

func (r *semanticFunctionRenderer) branch(mnemonic string, instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 1 {
		return "", fmt.Errorf("branch requires one target")
	}
	target := semanticLabel(instruction.Operands[0])
	if target == "" {
		return "", fmt.Errorf("branch target is invalid")
	}
	return mnemonic + " " + target, nil
}

func (r *semanticFunctionRenderer) conditionalBranch(instruction plan9sem.Inst) (string, error) {
	condition := strings.TrimPrefix(instruction.Op.Variant, "b")
	return r.branch("b."+condition, instruction)
}

func (r *semanticFunctionRenderer) compareBranch(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 || instruction.Operands[0].Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("compare-and-branch requires a register and target")
	}
	target := semanticLabel(instruction.Operands[1])
	if target == "" {
		return "", fmt.Errorf("compare-and-branch target is invalid")
	}
	mnemonic := strings.TrimSuffix(instruction.Op.Variant, "w")
	return fmt.Sprintf("%s %s, %s", mnemonic,
		semanticRegister(instruction.Operands[0].Register, instruction.Op.Width), target), nil
}

func (r *semanticFunctionRenderer) atomicLoad(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 || instruction.Operands[1].Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("atomic load requires an address and destination")
	}
	address, err := r.memoryOperand(instruction.Operands[0])
	if err != nil {
		return "", err
	}
	mnemonic := semanticAtomicMnemonic(instruction.Op.Variant)
	return fmt.Sprintf("%s %s, %s", mnemonic,
		semanticRegister(instruction.Operands[1].Register, instruction.Op.Width), address), nil
}

func (r *semanticFunctionRenderer) atomicStore(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 2 || instruction.Operands[0].Kind != plan9sem.RegisterOperand {
		return "", fmt.Errorf("atomic store requires a value and address")
	}
	address, err := r.memoryOperand(instruction.Operands[1])
	if err != nil {
		return "", err
	}
	mnemonic := semanticAtomicMnemonic(instruction.Op.Variant)
	return fmt.Sprintf("%s %s, %s", mnemonic,
		semanticRegister(instruction.Operands[0].Register, instruction.Op.Width), address), nil
}

func (r *semanticFunctionRenderer) atomicStoreExclusive(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 3 {
		return "", fmt.Errorf("exclusive store requires a value, address, and status")
	}
	address, err := r.memoryOperand(instruction.Operands[1])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s, %s, %s", semanticAtomicMnemonic(instruction.Op.Variant),
		semanticRegister(instruction.Operands[2].Register, 4),
		semanticRegister(instruction.Operands[0].Register, instruction.Op.Width), address), nil
}

func (r *semanticFunctionRenderer) lseAtomic(instruction plan9sem.Inst) (string, error) {
	if len(instruction.Operands) != 3 {
		return "", fmt.Errorf("LSE atomic requires a source, address, and destination")
	}
	address, err := r.memoryOperand(instruction.Operands[1])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s, %s, %s", semanticAtomicMnemonic(instruction.Op.Variant),
		semanticRegister(instruction.Operands[0].Register, instruction.Op.Width),
		semanticRegister(instruction.Operands[2].Register, instruction.Op.Width), address), nil
}

func (r *semanticFunctionRenderer) generalOperand(operand plan9sem.Operand, width int) (string, error) {
	switch operand.Kind {
	case plan9sem.RegisterOperand:
		return semanticRegister(operand.Register, width), nil
	case plan9sem.ImmediateOperand:
		return "#" + strconv.FormatInt(operand.Immediate, 10), nil
	default:
		return "", fmt.Errorf("unsupported arithmetic operand %v", operand.Kind)
	}
}

func (r *semanticFunctionRenderer) memoryOperand(operand plan9sem.Operand) (string, error) {
	if operand.Kind != plan9sem.MemoryOperand || operand.Index != "" || operand.Offset != 0 {
		return "", fmt.Errorf("unsupported atomic memory operand %v", operand.Kind)
	}
	return "[" + semanticRegister(operand.Base, 8) + "]", nil
}

func (r *semanticFunctionRenderer) parameterOperand(operand plan9sem.Operand) (string, error) {
	if operand.Slot < 0 || operand.Slot >= len(r.parameters) {
		return "", fmt.Errorf("parameter slot %d is out of range", operand.Slot)
	}
	index := r.resultCount() + operand.Slot
	return semanticPlaceholder(index, r.source.Signature.Params[operand.Slot].Width), nil
}

func (r *semanticFunctionRenderer) resultOperand(operand plan9sem.Operand) (string, error) {
	if operand.Slot < 0 || operand.Slot >= len(r.source.Signature.Results) {
		return "", fmt.Errorf("result slot %d is out of range", operand.Slot)
	}
	if operand.Slot == 0 {
		return semanticPlaceholder(0, r.source.Signature.Results[0].Width), nil
	}
	pointer := r.resultPointers[operand.Slot]
	for index, parameter := range r.parameters {
		if parameter == pointer {
			return semanticPlaceholder(r.resultCount()+index, 8), nil
		}
	}
	return "", fmt.Errorf("result slot %d has no result pointer", operand.Slot)
}

func (r *semanticFunctionRenderer) symbolOperand(symbol plan9sem.SymRef, offset int64) string {
	key := semanticSymbolKey{name: symbol.Name, offset: offset}
	if index, ok := r.symbols[key]; ok {
		return semanticPlaceholder(index, 8)
	}
	index := len(r.specs)
	r.symbols[key] = index
	r.specs = append(r.specs, ir.AsmSpec{
		Kind: ir.AsmRegIn,
		Ref:  r.function.Sym(symbol.Name, offset),
	})
	return semanticPlaceholder(index, 8)
}

func (r *semanticFunctionRenderer) resultCount() int {
	if len(r.source.Signature.Results) > 0 {
		return 1
	}
	return 0
}

func semanticStoreMnemonic(width int) string {
	switch width {
	case 1:
		return "strb"
	case 2:
		return "strh"
	default:
		return "str"
	}
}

func semanticRegister(register string, width int) string {
	if register == "sp" {
		return register
	}
	if register == "zr" {
		if width <= 4 {
			return "wzr"
		}
		return "xzr"
	}
	if register == "g" {
		register = "x28"
	}
	if strings.HasPrefix(register, "x") {
		if width <= 4 {
			return "w" + strings.TrimPrefix(register, "x")
		}
		return register
	}
	return register
}

func semanticPlaceholder(index, width int) string {
	if width <= 4 {
		return "%w" + strconv.Itoa(index)
	}
	return "%x" + strconv.Itoa(index)
}

func semanticLabel(operand plan9sem.Operand) string {
	switch operand.Kind {
	case plan9sem.LabelOperand:
		return operand.Label
	case plan9sem.FunctionOperand:
		return operand.Symbol.Name
	}
	return ""
}

func semanticLoadMnemonic(width int, variant string) string {
	switch width {
	case 1:
		if variant == "sign-extend" {
			return "ldrsb"
		}
		return "ldrb"
	case 2:
		if variant == "sign-extend" {
			return "ldrsh"
		}
		return "ldrh"
	case 4:
		if variant == "sign-extend" {
			return "ldrsw"
		}
		return "ldr"
	default:
		return "ldr"
	}
}

func semanticAtomicMnemonic(variant string) string {
	mnemonic := strings.ToLower(variant)
	if strings.HasSuffix(mnemonic, "w") || strings.HasSuffix(mnemonic, "d") {
		mnemonic = mnemonic[:len(mnemonic)-1]
	}
	if strings.HasPrefix(mnemonic, "ldor") {
		mnemonic = "ldset" + strings.TrimPrefix(mnemonic, "ldor")
	}
	return mnemonic
}

func resultWidth(function *plan9sem.Func, slot int) int {
	if slot >= 0 && slot < len(function.Signature.Results) {
		return function.Signature.Results[slot].Width
	}
	return 8
}
