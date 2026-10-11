package plan9asm

import (
	"fmt"
	"sort"
	"strings"
)

// amd64Layout is the ABI0 argument area of one function: the slots it reads
// as arguments and writes as results, grouped into Go values.
type amd64Layout struct {
	inputs  []amd64Value
	outputs []amd64Value
}

// amd64Value is one Go value: a scalar, or the scalars of an aggregate that
// ABIInternal assigns to registers as a unit.
type amd64Value struct {
	slots []abi0Slot
}

func (layout amd64Layout) empty() bool {
	return len(layout.inputs) == 0 && len(layout.outputs) == 0
}

// amd64IntegerArgumentRegisters is Go's amd64 ABIInternal integer argument
// and result sequence (internal/abi.IntArgRegs = 9).
var amd64IntegerArgumentRegisters = []string{"AX", "BX", "CX", "DI", "SI", "R8", "R9", "R10", "R11"}

// amd64FloatArgumentRegisters is the floating-point sequence. Go allows
// X0-X14; cg12 caps it at X0-X12 because it keeps two float scratch
// registers below the X15 zero register (AMD64_PARITY_PLAN §3.3(b)), so a
// signature needing more is refused rather than silently stack-passed.
const amd64FloatArgumentRegisters = 13

// collectLayouts determines each ABI0 function's argument layout, from its Go
// signature when the frontend supplied one and otherwise from the
// name+off(FP) operands its body uses.
func (t *amd64Translator) collectLayouts(file *File) error {
	inferred := make([]map[int]abi0Slot, len(t.infos))
	index := -1
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			index++
			inferred[index] = make(map[int]abi0Slot)
		case *Instruction:
			if index < 0 {
				continue
			}
			info := t.infos[index]
			if statement.Opcode == "JMP" && len(statement.Operands) == 1 {
				if symbol, ok := operandSymbol(statement.Operands[0]); ok {
					info.tailAlias = t.symbol(symbol)
				}
			}
			t.inferSlots(info, statement, inferred[index])
		}
	}
	names := make(map[string]*amd64FunctionInfo)
	for _, info := range t.infos {
		names[info.symbol] = info
	}
	for index, info := range t.infos {
		if info.kind == amd64Static {
			continue
		}
		if signature, ok := t.options.Signatures[info.symbol]; ok {
			info.layout = amd64SignatureLayout(signature)
			continue
		}
		info.layout = t.inferredLayout(info, inferred[index])
	}
	// A function whose whole body is a tail jump to another function in this
	// file reads its arguments through that function, so it shares its layout
	// (the ARM64 path does the same for B aliases).
	for range t.infos {
		changed := false
		for _, info := range t.infos {
			if info.kind == amd64Static || info.kind == amd64ABIInternal || !info.layout.empty() {
				continue
			}
			if _, ok := t.options.Signatures[info.symbol]; ok {
				continue
			}
			target := names[info.tailAlias]
			if target == nil || target.layout.empty() {
				continue
			}
			info.layout = target.layout
			changed = true
		}
		if !changed {
			break
		}
	}
	return nil
}

func amd64SignatureLayout(signature AsmSignature) amd64Layout {
	return amd64Layout{
		inputs:  amd64SignatureValues(signature.Params),
		outputs: amd64SignatureValues(signature.Results),
	}
}

func amd64SignatureValues(slots []AsmSlot) []amd64Value {
	var values []amd64Value
	for index, slot := range slots {
		converted := abi0Slot{offset: slot.Offset, width: slot.Width, float: slot.Float}
		if slot.Group != 0 && index > 0 && slots[index-1].Group == slot.Group {
			last := &values[len(values)-1]
			last.slots = append(last.slots, converted)
			continue
		}
		values = append(values, amd64Value{slots: []abi0Slot{converted}})
	}
	return values
}

// inferSlots records the ABI0 slots one instruction reads or writes through
// the FP pseudo-register.
func (t *amd64Translator) inferSlots(info *amd64FunctionInfo, instruction *Instruction, slots map[int]abi0Slot) {
	if info.kind == amd64Static {
		return
	}
	opcode := instruction.Opcode
	if opcode == "LEAQ" || opcode == "LEAL" {
		return
	}
	width, float := amd64FrameAccess(instruction)
	if width == 0 {
		return
	}
	last := len(instruction.Operands) - 1
	for position, operand := range instruction.Operands {
		if operand.Kind != OperandMemory || operand.Base != "FP" {
			continue
		}
		offset, _, err := t.frameOffset(operand.Offset)
		if err != nil {
			continue
		}
		unaryDestination := last == 0 && (strings.HasPrefix(opcode, "SET") || strings.HasPrefix(opcode, "POP"))
		output := position == last && (last > 0 || unaryDestination) && amd64StoresDestination(opcode)
		slot := slots[int(offset)]
		slot.offset = int(offset)
		slot.width = max(slot.width, width)
		slot.float = slot.float || float
		if output {
			slot.float = slot.float || float
			slots[int(offset)] = slot
			info.outputOffsets[int(offset)] = true
			continue
		}
		slots[int(offset)] = slot
		if strings.HasSuffix(namedFrameField(operand.Offset), "_base") {
			// Go names a slice's three ABI0 words name_base, name_len, and
			// name_cap. Assembly commonly reads only the base, but
			// ABIInternal still assigns all three before the next argument.
			for _, extra := range []int{8, 16} {
				neighbour := slots[int(offset)+extra]
				neighbour.offset = int(offset) + extra
				neighbour.width = max(neighbour.width, 8)
				slots[int(offset)+extra] = neighbour
			}
		}
	}
}

func (t *amd64Translator) inferredLayout(info *amd64FunctionInfo, slots map[int]abi0Slot) amd64Layout {
	functionName := strings.TrimPrefix(info.text.Symbol.Name, "·")
	floatInputs := make(map[int]bool)
	for _, offset := range t.options.FloatInputs[functionName] {
		floatInputs[offset] = true
	}
	floatOutputs := make(map[int]bool)
	for _, offset := range t.options.FloatOutputs[functionName] {
		floatOutputs[offset] = true
	}
	outputs := info.outputOffsets
	offsets := make([]int, 0, len(slots))
	for offset := range slots {
		offsets = append(offsets, offset)
	}
	sort.Ints(offsets)
	var layout amd64Layout
	for _, offset := range offsets {
		slot := slots[offset]
		if outputs[offset] {
			if len(t.options.FloatOutputs[functionName]) != 0 {
				slot.float = floatOutputs[offset]
			}
			layout.outputs = append(layout.outputs, amd64Value{slots: []abi0Slot{slot}})
			continue
		}
		if len(t.options.FloatInputs[functionName]) != 0 {
			slot.float = floatInputs[offset]
		}
		layout.inputs = append(layout.inputs, amd64Value{slots: []abi0Slot{slot}})
	}
	return layout
}

// amd64FrameAccess gives the width in bytes, and whether it is a
// floating-point value, of the memory operand of an instruction that moves a
// value to or from the argument area.
func amd64FrameAccess(instruction *Instruction) (int, bool) {
	opcode := instruction.Opcode
	vector := false
	for _, operand := range instruction.Operands {
		if isAMD64VectorOperand(operand) {
			vector = true
		}
	}
	switch opcode {
	case "MOVSD":
		return 8, true
	case "MOVSS":
		return 4, true
	case "MOVQ", "MOVD":
		return 8, vector
	case "MOVL":
		return 4, vector
	case "MOVW":
		return 2, false
	case "MOVB":
		return 1, false
	case "MOVBLZX", "MOVBQZX", "MOVBWZX", "MOVBLSX", "MOVBQSX", "MOVBWSX":
		return 1, false
	case "MOVWLZX", "MOVWQZX", "MOVWLSX", "MOVWQSX":
		return 2, false
	case "MOVLQZX", "MOVLQSX":
		return 4, false
	}
	if strings.HasPrefix(opcode, "SET") {
		return 1, false
	}
	spec, ok := lookupAMD64(opcode)
	if ok && spec.width != 0 {
		return spec.width / 8, false
	}
	return 0, false
}

func amd64StoresDestination(opcode string) bool {
	if strings.HasPrefix(opcode, "MOV") || strings.HasPrefix(opcode, "SET") {
		return true
	}
	switch opcode {
	case "CVTSL2SD", "CVTSQ2SD", "CVTTSD2SQ", "CVTTSD2SL", "POPQ", "XCHGQ", "XCHGL":
		return true
	}
	return false
}

// amd64Location is where ABIInternal puts one scalar: a register, or a byte
// offset in the caller's stack-argument area (0 is the word just above the
// return address).
type amd64Location struct {
	register string
	stack    int
	onStack  bool
}

// amd64Placement pairs an ABI0 slot with its ABIInternal location.
type amd64Placement struct {
	slot     abi0Slot
	location amd64Location
}

// amd64Assigner hands out ABIInternal locations in order, the way Go's ABI
// assigns arguments: a value goes entirely into registers if the remaining
// ones can hold all of its scalars, and otherwise entirely onto the stack,
// aligned to its alignment, without consuming registers.
type amd64Assigner struct {
	integer int
	float   int
	stack   int
}

func (a *amd64Assigner) assign(value amd64Value) []amd64Placement {
	integerNeeded := 0
	floatNeeded := 0
	for _, slot := range value.slots {
		if slot.float {
			floatNeeded++
		} else {
			integerNeeded++
		}
	}
	placements := make([]amd64Placement, 0, len(value.slots))
	if a.integer+integerNeeded <= len(amd64IntegerArgumentRegisters) && a.float+floatNeeded <= amd64FloatArgumentRegisters {
		for _, slot := range value.slots {
			register := ""
			if slot.float {
				register = fmt.Sprintf("X%d", a.float)
				a.float++
			} else {
				register = amd64IntegerArgumentRegisters[a.integer]
				a.integer++
			}
			placements = append(placements, amd64Placement{slot: slot, location: amd64Location{register: register}})
		}
		return placements
	}
	base := value.slots[0].offset
	alignment := 1
	size := 0
	for _, slot := range value.slots {
		alignment = max(alignment, slot.width)
		size = max(size, slot.offset-base+slot.width)
	}
	a.stack = roundUpInteger(a.stack, alignment)
	for _, slot := range value.slots {
		placements = append(placements, amd64Placement{slot: slot, location: amd64Location{stack: a.stack + slot.offset - base, onStack: true}})
	}
	a.stack += size
	return placements
}

// emitABI0Wrapper writes the ABIInternal entry for an ABI0 function and
// returns its frame size (SP delta, excluding the return address) and the
// byte offset at which that frame is fully established.
//
// The calling contract is the one cg12's frontend uses for assembly functions
// (the ARM64 translator's emitABI0Wrapper implements the same one):
//   - The parameters arrive by ABIInternal: integers in RAX, RBX, RCX, RDI,
//     RSI, R8, R9, R10, R11, floats in X0-X12, anything that does not fit on
//     the stack above the return address.
//   - After the parameters come one pointer per result beyond the first, as
//     further integer arguments continuing the same sequence (or on the
//     stack once the registers run out).
//   - The first result is returned in RAX/RBX/... or X0/X1/..., the others
//     are stored through their pointers.
//
// The entry stores the parameters into an ABI0 argument area at the bottom
// of its own frame, saves the result pointers above that area, calls the
// ABI0 body, delivers the results, and finally re-establishes the
// ABIInternal invariants an ABI0 body may have broken (R14 = g, X15 = 0), as
// Go's own ABI wrappers do. R12 and R13, ABIInternal's scratch registers,
// carry values between memory locations.
func (t *amd64Translator) emitABI0Wrapper(info *amd64FunctionInfo) (int, int, error) {
	var arguments amd64Assigner
	var inputs []amd64Placement
	for _, value := range info.layout.inputs {
		inputs = append(inputs, arguments.assign(value)...)
	}
	var pointers []amd64Location
	for range max(0, len(info.layout.outputs)-1) {
		pointer := arguments.assign(amd64Value{slots: []abi0Slot{{width: 8}}})
		pointers = append(pointers, pointer[0].location)
	}
	var first []amd64Placement
	if len(info.layout.outputs) > 0 {
		var results amd64Assigner
		first = results.assign(info.layout.outputs[0])
		if first[0].location.onStack {
			return 0, 0, fmt.Errorf("ABI0 wrapper for %s: the first result does not fit in result registers", info.symbol)
		}
	}

	area := 0
	if info.argsKnown {
		area = info.args
	}
	for _, values := range [][]amd64Value{info.layout.inputs, info.layout.outputs} {
		for _, value := range values {
			for _, slot := range value.slots {
				area = max(area, slot.offset+slot.width)
			}
		}
	}
	area = roundUpInteger(area, 8)
	frame := area + 8*len(pointers)

	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", info.symbol)
	fmt.Fprintf(&t.output, ".type %s, @function\n", info.symbol)
	fmt.Fprintf(&t.output, "%s:\n", info.symbol)
	t.output.WriteString("\tpushq %rbp\n")
	t.output.WriteString("\tmovq %rsp, %rbp\n")
	frameStart := 4
	if frame != 0 {
		t.emitAdjustSP(int64(frame))
		frameStart += amd64AdjustSPSize(int64(frame))
	}
	for _, input := range inputs {
		destination := amd64Displacement(int64(input.slot.offset), "%rsp", "")
		if err := t.emitWrapperCopy(input.slot, input.location, destination, true); err != nil {
			return 0, 0, err
		}
	}
	for index, pointer := range pointers {
		destination := amd64Displacement(int64(area+8*index), "%rsp", "")
		if err := t.emitWrapperCopy(abi0Slot{width: 8}, pointer, destination, true); err != nil {
			return 0, 0, err
		}
	}
	fmt.Fprintf(&t.output, "\tcall %s\n", info.body)
	for _, result := range first {
		source := amd64Displacement(int64(result.slot.offset), "%rsp", "")
		if err := t.emitWrapperCopy(result.slot, result.location, source, false); err != nil {
			return 0, 0, err
		}
	}
	for index, value := range info.layout.outputs[min(1, len(info.layout.outputs)):] {
		fmt.Fprintf(&t.output, "\tmovq %s, %%r12\n", amd64Displacement(int64(area+8*index), "%rsp", ""))
		base := value.slots[0].offset
		for _, slot := range value.slots {
			source := amd64Displacement(int64(slot.offset), "%rsp", "")
			destination := amd64Displacement(int64(slot.offset-base), "%r12", "")
			if err := emitAMD64MemoryCopy(&t.output, slot.width, source, destination); err != nil {
				return 0, 0, err
			}
		}
	}
	if frame != 0 {
		t.emitAdjustSP(int64(-frame))
	}
	t.output.WriteString("\tpopq %rbp\n")
	emitAMD64RestoreABIInternal(&t.output)
	t.output.WriteString("\tret\n")
	return frame + 8, frameStart, nil
}

// emitAMD64RestoreABIInternal reloads the g register from TLS and zeroes X15,
// the two fixed registers ABIInternal code relies on and ABI0 code may use
// freely.
func emitAMD64RestoreABIInternal(output *strings.Builder) {
	fmt.Fprintf(output, "\tmovq %%fs:%s@tpoff, %%r14\n", AMD64TLSSymbol)
	output.WriteString("\txorps %xmm15, %xmm15\n")
}

// emitWrapperCopy moves one scalar between its ABIInternal location and an
// ABI0 frame slot: into the slot when store is set, out of it otherwise.
// Stack-assigned arguments are read relative to the wrapper's frame pointer
// (the caller's stack arguments start at 16(%rbp)).
func (t *amd64Translator) emitWrapperCopy(slot abi0Slot, location amd64Location, memory string, store bool) error {
	if location.onStack {
		if !store {
			return fmt.Errorf("stack-assigned results are not supported")
		}
		source := amd64Displacement(int64(16+location.stack), "%rbp", "")
		return emitAMD64MemoryCopy(&t.output, slot.width, source, memory)
	}
	if slot.float {
		register, _ := amd64VectorRegister(location.register)
		mnemonic := map[int]string{4: "movss", 8: "movsd"}[slot.width]
		if mnemonic == "" {
			return fmt.Errorf("unsupported floating-point ABI0 slot width %d", slot.width)
		}
		if store {
			fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, register, memory)
		} else {
			fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, memory, register)
		}
		return nil
	}
	if store {
		width := map[int]int{1: 8, 2: 16, 4: 32, 8: 64}[slot.width]
		if width == 0 {
			return fmt.Errorf("unsupported ABI0 slot width %d", slot.width)
		}
		register, err := amd64GPR(location.register, width)
		if err != nil {
			return err
		}
		mnemonic := map[int]string{8: "movb", 16: "movw", 32: "movl", 64: "movq"}[width]
		fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, register, memory)
		return nil
	}
	// A result narrower than its register is zero-extended into it.
	switch slot.width {
	case 1:
		register, _ := amd64GPR(location.register, 32)
		fmt.Fprintf(&t.output, "\tmovzbl %s, %s\n", memory, register)
	case 2:
		register, _ := amd64GPR(location.register, 32)
		fmt.Fprintf(&t.output, "\tmovzwl %s, %s\n", memory, register)
	case 4:
		register, _ := amd64GPR(location.register, 32)
		fmt.Fprintf(&t.output, "\tmovl %s, %s\n", memory, register)
	case 8:
		register, _ := amd64GPR(location.register, 64)
		fmt.Fprintf(&t.output, "\tmovq %s, %s\n", memory, register)
	default:
		return fmt.Errorf("unsupported ABI0 slot width %d", slot.width)
	}
	return nil
}

// emitAMD64MemoryCopy copies width bytes between two memory operands through
// R13.
func emitAMD64MemoryCopy(output *strings.Builder, width int, source, destination string) error {
	switch width {
	case 1:
		fmt.Fprintf(output, "\tmovb %s, %%r13b\n\tmovb %%r13b, %s\n", source, destination)
	case 2:
		fmt.Fprintf(output, "\tmovw %s, %%r13w\n\tmovw %%r13w, %s\n", source, destination)
	case 4:
		fmt.Fprintf(output, "\tmovl %s, %%r13d\n\tmovl %%r13d, %s\n", source, destination)
	case 8:
		fmt.Fprintf(output, "\tmovq %s, %%r13\n\tmovq %%r13, %s\n", source, destination)
	default:
		return fmt.Errorf("unsupported ABI0 slot width %d", width)
	}
	return nil
}

// emitRawVforkSyscallAMD64 writes the ABIInternal entry of
// syscall.rawVforkSyscall(trap, a1, a2, a3 uintptr) (r1, err uintptr).
//
// The ABI0 body pops its return address into R12 before SYSCALL(vfork) and
// pushes it back afterwards, because the child runs on the parent's stack
// until it execs and may overwrite anything below the caller's SP. A
// generated wrapper would keep its own return address and result area there,
// so the parent would return through a frame the child has clobbered.
// Instead this entry does the same work directly in ABIInternal registers:
// trap in AX, a1-a3 in BX, CX, DI, and (cg12's contract for a second result)
// a pointer to err in SI; r1 returns in AX and err is stored through the
// pointer, which R13 holds across the system call. Nothing is kept on the
// stack across SYSCALL, and R14 and X15 are left alone.
func (t *amd64Translator) emitRawVforkSyscallAMD64(info *amd64FunctionInfo) {
	okLabel := fmt.Sprintf(".L%s_rawVforkSyscall_ok", t.fileTag)
	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", info.symbol)
	fmt.Fprintf(&t.output, ".type %s, @function\n", info.symbol)
	fmt.Fprintf(&t.output, "%s:\n", info.symbol)
	t.output.WriteString("\tmovq %rsi, %r13\n")
	t.output.WriteString("\tmovq %rdi, %rdx\n")
	t.output.WriteString("\tmovq %rcx, %rsi\n")
	t.output.WriteString("\tmovq %rbx, %rdi\n")
	t.output.WriteString("\tmovq $0, %r10\n")
	t.output.WriteString("\tmovq $0, %r8\n")
	t.output.WriteString("\tmovq $0, %r9\n")
	t.output.WriteString("\tpopq %r12\n")
	t.output.WriteString("\tsyscall\n")
	t.output.WriteString("\tpushq %r12\n")
	t.output.WriteString("\tcmpq $-4095, %rax\n")
	fmt.Fprintf(&t.output, "\tjbe %s\n", okLabel)
	t.output.WriteString("\tnegq %rax\n")
	t.output.WriteString("\tmovq %rax, (%r13)\n")
	t.output.WriteString("\tmovq $-1, %rax\n")
	t.output.WriteString("\tret\n")
	fmt.Fprintf(&t.output, "%s:\n", okLabel)
	t.output.WriteString("\tmovq $0, (%r13)\n")
	t.output.WriteString("\tret\n")
}

// abiInternalBodySymbol names the body of an <ABIInternal> function whose
// plain name is a result adapter.
func abiInternalBodySymbol(name string) string {
	return name + "_abiinternal"
}

// emitABIInternalAdapter writes the plain-name entry of an <ABIInternal>
// function with more than one result. The body returns every result in
// registers, as Go's ABIInternal does (Syscall6: r1, r2, errno in AX, BX,
// CX), but cg12 calls every function with the contract emitABI0Wrapper
// documents: after the parameters, one pointer per extra result continues the
// integer argument sequence, and only the first result comes back in a
// register. The adapter saves those pointers, calls the body with the
// parameters untouched (copying any stack-assigned ones down so the body
// finds them above its return address), and stores each extra result through
// its pointer. R14 and X15 pass through unchanged. It returns the frame size
// (SP delta, excluding the return address) and where that frame is reached.
func (t *amd64Translator) emitABIInternalAdapter(info *amd64FunctionInfo) (int, int, error) {
	var arguments amd64Assigner
	for _, value := range info.layout.inputs {
		arguments.assign(value)
	}
	inputStack := roundUpInteger(arguments.stack, 8)
	var pointers []amd64Location
	for range len(info.layout.outputs) - 1 {
		pointer := arguments.assign(amd64Value{slots: []abi0Slot{{width: 8}}})
		pointers = append(pointers, pointer[0].location)
	}
	var results amd64Assigner
	var extra [][]amd64Placement
	for index, value := range info.layout.outputs {
		placements := results.assign(value)
		if placements[0].location.onStack {
			return 0, 0, fmt.Errorf("ABIInternal adapter for %s: result %d does not fit in result registers", info.symbol, index)
		}
		if index > 0 {
			extra = append(extra, placements)
		}
	}
	saved := inputStack
	frame := inputStack + 8*len(pointers)

	t.output.WriteString("\n.text\n")
	fmt.Fprintf(&t.output, ".global %s\n", info.symbol)
	fmt.Fprintf(&t.output, ".type %s, @function\n", info.symbol)
	fmt.Fprintf(&t.output, "%s:\n", info.symbol)
	t.output.WriteString("\tpushq %rbp\n")
	t.output.WriteString("\tmovq %rsp, %rbp\n")
	frameStart := 4
	if frame != 0 {
		t.emitAdjustSP(int64(frame))
		frameStart += amd64AdjustSPSize(int64(frame))
	}
	for offset := 0; offset < inputStack; offset += 8 {
		source := amd64Displacement(int64(16+offset), "%rbp", "")
		destination := amd64Displacement(int64(offset), "%rsp", "")
		if err := emitAMD64MemoryCopy(&t.output, 8, source, destination); err != nil {
			return 0, 0, err
		}
	}
	for index, pointer := range pointers {
		destination := amd64Displacement(int64(saved+8*index), "%rsp", "")
		if err := t.emitWrapperCopy(abi0Slot{width: 8}, pointer, destination, true); err != nil {
			return 0, 0, err
		}
	}
	fmt.Fprintf(&t.output, "\tcall %s\n", info.body)
	for index, placements := range extra {
		fmt.Fprintf(&t.output, "\tmovq %s, %%r12\n", amd64Displacement(int64(saved+8*index), "%rsp", ""))
		base := placements[0].slot.offset
		for _, placement := range placements {
			destination := amd64Displacement(int64(placement.slot.offset-base), "%r12", "")
			if err := t.emitWrapperCopy(placement.slot, placement.location, destination, true); err != nil {
				return 0, 0, err
			}
		}
	}
	if frame != 0 {
		t.emitAdjustSP(int64(-frame))
	}
	t.output.WriteString("\tpopq %rbp\n")
	t.output.WriteString("\tret\n")
	return frame + 8, frameStart, nil
}
