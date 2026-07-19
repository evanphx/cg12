package arm64

import (
	"fmt"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
)

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

	assigner := newArgAssigner(true)
	abi0Offset := 0
	var registerInputs []goABI0RegisterMove
	var stackInputs []goABI0StackCopy
	for _, parameter := range function.Params {
		size, alignment := goABI0ValueLayout(parameter.Cls, parameter.Agg)
		abi0Offset = roundUp(abi0Offset, alignment)
		if parameter.Agg != nil {
			parts, onStack, internalOffset := assignGoAggregate(&assigner, parameter.Agg)
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

	results, internalResultEnd, err := goABI0Results(function, abi0Offset, assigner.nsaa)
	if err != nil {
		return "", goFunctionInfo{}, err
	}
	outgoingEnd := max(assigner.nsaa, internalResultEnd)
	frameSize := roundUp(8+outgoingEnd, 16)
	if frameSize < 16 {
		frameSize = 16
	}

	wrapperName := name + "_abi0"
	var output strings.Builder
	output.WriteString("\n.text\n")
	fmt.Fprintf(&output, ".global %s\n", wrapperName)
	fmt.Fprintf(&output, ".hidden %s\n", wrapperName)
	fmt.Fprintf(&output, ".type %s, %%function\n", wrapperName)
	fmt.Fprintf(&output, "%s:\n", wrapperName)
	fmt.Fprintf(&output, "\tsub sp, sp, #%d\n", frameSize)
	output.WriteString("\tstr x30, [sp]\n")
	output.WriteString("\tstur x29, [sp, #-8]\n")
	output.WriteString("\tsub x29, sp, #8\n")
	for _, input := range registerInputs {
		emitGoABI0RegisterLoad(&output, input.register, input.width, frameSize+8+input.stackOffset)
	}
	for _, input := range stackInputs {
		emitGoABI0StackCopy(&output, frameSize+8+input.abi0Offset, 8+input.internalOffset, input.size)
	}
	fmt.Fprintf(&output, "\tbl %s\n", name)
	for _, result := range results {
		destination := frameSize + 8 + result.abi0Offset
		if result.stack {
			emitGoABI0StackCopy(&output, 8+result.internalOffset, destination, result.size)
		} else {
			emitGoABI0RegisterStore(&output, result.register, result.width, destination)
		}
	}
	output.WriteString("\tldur x29, [sp, #-8]\n")
	output.WriteString("\tldr x30, [sp]\n")
	fmt.Fprintf(&output, "\tadd sp, sp, #%d\n", frameSize)
	output.WriteString("\tret\n")
	fmt.Fprintf(&output, ".size %s, .-%s\n", wrapperName, wrapperName)

	return output.String(), goFunctionInfo{
		name:       wrapperName,
		frameSize:  frameSize,
		frameStart: 4,
		funcFlag:   goFuncFlagAsm,
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

func emitGoABI0RegisterLoad(output *strings.Builder, register Reg, width, offset int) {
	mnemonic := goABI0LoadMnemonic(register, width)
	fmt.Fprintf(output, "\t%s %s, [sp, #%d]\n", mnemonic, register.Name(width), offset)
}

func emitGoABI0RegisterStore(output *strings.Builder, register Reg, width, offset int) {
	mnemonic := goABI0StoreMnemonic(register, width)
	fmt.Fprintf(output, "\t%s %s, [sp, #%d]\n", mnemonic, register.Name(width), offset)
}

func goABI0LoadMnemonic(register Reg, width int) string {
	if register.IsFloat() || width >= 4 {
		return "ldr"
	}
	if width == 2 {
		return "ldrh"
	}
	return "ldrb"
}

func goABI0StoreMnemonic(register Reg, width int) string {
	if register.IsFloat() || width >= 4 {
		return "str"
	}
	if width == 2 {
		return "strh"
	}
	return "strb"
}

func emitGoABI0StackCopy(output *strings.Builder, sourceOffset, destinationOffset, size int) {
	for size >= 8 {
		fmt.Fprintf(output, "\tldr x16, [sp, #%d]\n", sourceOffset)
		fmt.Fprintf(output, "\tstr x16, [sp, #%d]\n", destinationOffset)
		sourceOffset += 8
		destinationOffset += 8
		size -= 8
	}
	if size >= 4 {
		fmt.Fprintf(output, "\tldr w16, [sp, #%d]\n", sourceOffset)
		fmt.Fprintf(output, "\tstr w16, [sp, #%d]\n", destinationOffset)
		sourceOffset += 4
		destinationOffset += 4
		size -= 4
	}
	if size >= 2 {
		fmt.Fprintf(output, "\tldrh w16, [sp, #%d]\n", sourceOffset)
		fmt.Fprintf(output, "\tstrh w16, [sp, #%d]\n", destinationOffset)
		sourceOffset += 2
		destinationOffset += 2
		size -= 2
	}
	if size == 1 {
		fmt.Fprintf(output, "\tldrb w16, [sp, #%d]\n", sourceOffset)
		fmt.Fprintf(output, "\tstrb w16, [sp, #%d]\n", destinationOffset)
	}
}
