package amd64

import (
	"fmt"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// This file emits the `name_abi0` shims through which Plan 9 assembly calls a
// cg12-compiled Go function. Assembly speaks ABI0 -- every argument and result
// in the caller's outgoing area, nothing preserved -- while the function is
// compiled for ABIInternal, so the shim moves the arguments into their
// registers, establishes the two registers ABIInternal code may assume (g in
// R14, zero in X15: ABI0 code is free to have clobbered both), calls, and
// stores the results back where the assembly expects them. It is the job Go's
// linker-generated ABI wrappers do.

type goABI0RegisterMove struct {
	stackOffset int
	register    Reg
	width       int
}

type goABI0StackCopy struct {
	abi0Offset     int
	internalOffset int
	size           int
}

type goABI0Result struct {
	abi0Offset     int
	internalOffset int
	register       Reg
	width          int
	stack          bool
	size           int
}

// amd64GReload is the TLS load that re-establishes g in R14, the access the
// translated runtime assembly uses for its own `get_tls`/`g` macros.
const amd64GReload = "\tmovq %fs:runtime_tlsg@tpoff, %r14\n"

func emitGoABI0AssemblyWrappers(module *ir.Module, references, assemblyFunctions map[string]bool) (string, []goFunctionInfo, error) {
	functions := make(map[string]*ir.Func)
	for _, function := range module.Funcs {
		functions[sanitize(function.Name)] = function
	}
	names := make([]string, 0, len(references))
	for name := range references {
		if functions[name] != nil && !assemblyFunctions[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var output strings.Builder
	metadata := make([]goFunctionInfo, 0, len(names))
	for _, name := range names {
		assembly, info, err := emitGoABI0AssemblyWrapper(name, functions[name])
		if err != nil {
			return "", nil, fmt.Errorf("ABI0 wrapper for %s: %w", name, err)
		}
		output.WriteString(assembly)
		metadata = append(metadata, info)
	}
	return output.String(), metadata, nil
}

func emitGoABI0AssemblyWrapper(name string, function *ir.Func) (string, goFunctionInfo, error) {
	if !function.UsesGoInternalCallConvention() {
		return "", goFunctionInfo{}, fmt.Errorf("target does not use the Go ABI")
	}

	for _, parameter := range function.Params {
		if isResultPointerParameter(parameter.Name) {
			// goc returns results after the first through caller-supplied
			// pointers; mapping those onto ABI0 result slots needs the results'
			// types, which the IR does not carry.
			return "", goFunctionInfo{}, fmt.Errorf("multiple results are not supported in an ABI0 shim")
		}
	}

	assigner := newArgAssigner(true)
	abi0Offset := 0
	var registerInputs []goABI0RegisterMove
	var stackInputs []goABI0StackCopy
	for parameterIndex := 0; parameterIndex < len(function.Params); {
		var aggregate *ir.AggType
		count := 1
		if group, ok := ir.ValueGroupAt(function.ParamGroups, parameterIndex); ok {
			aggregate = group.Type
			count = group.Count
		} else if function.Params[parameterIndex].Agg != nil {
			aggregate = function.Params[parameterIndex].Agg
		}
		parameter := function.Params[parameterIndex]
		parameterIndex += count

		size, alignment := goABI0ValueLayout(parameter.Cls, aggregate)
		abi0Offset = roundUp(abi0Offset, alignment)
		if aggregate != nil {
			parts, onStack, internalOffset := assignGoAggregate(&assigner, aggregate)
			if onStack {
				stackInputs = append(stackInputs, goABI0StackCopy{
					abi0Offset:     abi0Offset,
					internalOffset: internalOffset,
					size:           size,
				})
			} else {
				for _, part := range parts {
					registerInputs = append(registerInputs, goABI0RegisterMove{
						stackOffset: abi0Offset + part.offset,
						register:    part.reg,
						width:       part.sub.Size(),
					})
				}
			}
		} else {
			location := assigner.assign(parameter.Cls)
			if location.onStack {
				stackInputs = append(stackInputs, goABI0StackCopy{
					abi0Offset:     abi0Offset,
					internalOffset: location.stacky,
					size:           size,
				})
			} else {
				registerInputs = append(registerInputs, goABI0RegisterMove{
					stackOffset: abi0Offset,
					register:    location.reg,
					width:       size,
				})
			}
		}
		abi0Offset += size
	}

	// ABI0 results start at the next pointer-aligned offset after the arguments.
	abi0Offset = roundUp(abi0Offset, 8)
	results, _, err := goABI0Results(function, abi0Offset, assigner.nsaa)
	if err != nil {
		return "", goFunctionInfo{}, err
	}
	// The outgoing area is the callee's whole argument frame: stack arguments,
	// stack results, and the register home slots its prologue may spill into.
	outgoing := goArgumentFrameFor(function).size
	// The frame is the saved RBP plus the outgoing area, keeping RSP 16-byte
	// aligned at the call: entry RSP is 8 mod 16, the push makes it 0.
	frame := roundUp(outgoing, 16)

	// The caller's ABI0 arguments sit above the return address and the saved
	// RBP: at entry RSP+8, so at RSP + frame + 16 once the frame is built.
	argumentBase := frame + 16
	wrapperName := name + "_abi0"
	var output strings.Builder
	output.WriteString("\n.text\n")
	fmt.Fprintf(&output, ".global %s\n", wrapperName)
	fmt.Fprintf(&output, ".hidden %s\n", wrapperName)
	fmt.Fprintf(&output, ".type %s, @function\n", wrapperName)
	fmt.Fprintf(&output, "%s:\n", wrapperName)
	output.WriteString("\tpushq %rbp\n")
	output.WriteString("\tmovq %rsp, %rbp\n")
	if frame > 0 {
		fmt.Fprintf(&output, "\tsubq $%d, %%rsp\n", frame)
	}
	frameStartLabel := wrapperName + ".framestart"
	fmt.Fprintf(&output, "%s:\n", frameStartLabel)
	for _, input := range stackInputs {
		emitGoABI0StackCopy(&output, argumentBase+input.abi0Offset, input.internalOffset, input.size)
	}
	for _, input := range registerInputs {
		emitGoABI0RegisterLoad(&output, input.register, input.width, argumentBase+input.stackOffset)
	}
	output.WriteString(amd64GReload)
	output.WriteString("\txorps %xmm15, %xmm15\n")
	fmt.Fprintf(&output, "\tcall %s\n", name)
	for _, result := range results {
		destination := argumentBase + result.abi0Offset
		if result.stack {
			emitGoABI0StackCopy(&output, result.internalOffset, destination, result.size)
		} else {
			emitGoABI0RegisterStore(&output, result.register, result.width, destination)
		}
	}
	output.WriteString("\tmovq %rbp, %rsp\n")
	output.WriteString("\tpopq %rbp\n")
	output.WriteString("\tret\n")
	fmt.Fprintf(&output, ".size %s, .-%s\n", wrapperName, wrapperName)

	// The prologue is three fixed-size instructions: push %rbp (1 byte),
	// mov %rsp,%rbp (3 bytes) and, with a frame, sub $frame,%rsp (4 bytes for an
	// 8-bit immediate, 7 for a 32-bit one).
	frameStart := 4
	switch {
	case frame == 0:
	case frame < 128:
		frameStart += 4
	default:
		frameStart += 7
	}
	return output.String(), goFunctionInfo{
		Name:       wrapperName,
		FrameSize:  frame + 8,
		FrameStart: frameStart,
		FuncFlag:   goFuncFlagAsm,
	}, nil
}

func goABI0Results(function *ir.Func, abi0Offset, internalArgumentEnd int) ([]goABI0Result, int, error) {
	if function.RetAgg == nil {
		if !function.HasRet {
			return nil, internalArgumentEnd, nil
		}
		size, alignment := goABI0ValueLayout(function.Retty, nil)
		resultOffset := roundUp(abi0Offset, alignment)
		return []goABI0Result{{
			abi0Offset: resultOffset,
			register:   retReg(function.Retty),
			width:      size,
		}}, internalArgumentEnd, nil
	}

	size, alignment := function.RetAgg.Layout()
	abi0ResultOffset := roundUp(abi0Offset, alignment)
	resultAssigner := newArgAssigner(true)
	resultAssigner.nsaa = roundUp(internalArgumentEnd, 8)
	parts, onStack, internalOffset := assignGoAggregate(&resultAssigner, function.RetAgg)
	if onStack {
		return []goABI0Result{{
			abi0Offset:     abi0ResultOffset,
			internalOffset: internalOffset,
			stack:          true,
			size:           size,
		}}, resultAssigner.nsaa, nil
	}
	results := make([]goABI0Result, 0, len(parts))
	for _, part := range parts {
		results = append(results, goABI0Result{
			abi0Offset: abi0ResultOffset + part.offset,
			register:   part.reg,
			width:      part.sub.Size(),
		})
	}
	return results, resultAssigner.nsaa, nil
}

func goABI0ValueLayout(class ir.Cls, aggregate *ir.AggType) (int, int) {
	if aggregate != nil {
		return aggregate.Layout()
	}
	size := class.Size()
	return size, size
}

// gnuRegisterName spells a register at an access width for the GNU assembler.
func gnuRegisterName(register Reg, width int) string {
	if register.IsFloat() {
		return fmt.Sprintf("%%xmm%d", int(register-XMM0))
	}
	names := gpNames[register]
	switch width {
	case 1:
		return "%" + names[0]
	case 2:
		return "%" + names[1]
	case 4:
		return "%" + names[2]
	default:
		return "%" + names[3]
	}
}

func emitGoABI0RegisterLoad(output *strings.Builder, register Reg, width, offset int) {
	destination := gnuRegisterName(register, 8)
	switch {
	case register.IsFloat() && width == 4:
		fmt.Fprintf(output, "\tmovss %d(%%rsp), %s\n", offset, destination)
	case register.IsFloat():
		fmt.Fprintf(output, "\tmovsd %d(%%rsp), %s\n", offset, destination)
	case width == 8:
		fmt.Fprintf(output, "\tmovq %d(%%rsp), %s\n", offset, destination)
	case width == 4:
		fmt.Fprintf(output, "\tmovl %d(%%rsp), %s\n", offset, gnuRegisterName(register, 4))
	case width == 2:
		fmt.Fprintf(output, "\tmovzwl %d(%%rsp), %s\n", offset, gnuRegisterName(register, 4))
	default:
		fmt.Fprintf(output, "\tmovzbl %d(%%rsp), %s\n", offset, gnuRegisterName(register, 4))
	}
}

func emitGoABI0RegisterStore(output *strings.Builder, register Reg, width, offset int) {
	source := gnuRegisterName(register, width)
	switch {
	case register.IsFloat() && width == 4:
		fmt.Fprintf(output, "\tmovss %s, %d(%%rsp)\n", source, offset)
	case register.IsFloat():
		fmt.Fprintf(output, "\tmovsd %s, %d(%%rsp)\n", source, offset)
	case width == 8:
		fmt.Fprintf(output, "\tmovq %s, %d(%%rsp)\n", source, offset)
	case width == 4:
		fmt.Fprintf(output, "\tmovl %s, %d(%%rsp)\n", source, offset)
	case width == 2:
		fmt.Fprintf(output, "\tmovw %s, %d(%%rsp)\n", source, offset)
	default:
		fmt.Fprintf(output, "\tmovb %s, %d(%%rsp)\n", source, offset)
	}
}

// emitGoABI0StackCopy copies size bytes between two RSP-relative offsets
// through R12, which is free in a wrapper: ABI0 preserves nothing and the
// ABIInternal callee has not been entered yet.
func emitGoABI0StackCopy(output *strings.Builder, sourceOffset, destinationOffset, size int) {
	for _, width := range []int{8, 4, 2, 1} {
		for size >= width {
			switch width {
			case 8:
				fmt.Fprintf(output, "\tmovq %d(%%rsp), %%r12\n\tmovq %%r12, %d(%%rsp)\n", sourceOffset, destinationOffset)
			case 4:
				fmt.Fprintf(output, "\tmovl %d(%%rsp), %%r12d\n\tmovl %%r12d, %d(%%rsp)\n", sourceOffset, destinationOffset)
			case 2:
				fmt.Fprintf(output, "\tmovw %d(%%rsp), %%r12w\n\tmovw %%r12w, %d(%%rsp)\n", sourceOffset, destinationOffset)
			default:
				fmt.Fprintf(output, "\tmovb %d(%%rsp), %%r12b\n\tmovb %%r12b, %d(%%rsp)\n", sourceOffset, destinationOffset)
			}
			sourceOffset += width
			destinationOffset += width
			size -= width
		}
	}
}

// isResultPointerParameter reports whether a parameter is one of the result
// pointers goc appends for results after the first ("result1", ...).
func isResultPointerParameter(name string) bool {
	digits := strings.TrimPrefix(name, "result")
	if digits == name || digits == "" {
		return false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
