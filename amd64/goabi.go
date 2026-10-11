package amd64

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/ir"
)

// This file holds the parts of Go's frame contract that amd64 has to honour
// beyond plain System V: the ABIInternal register assignment of aggregates, and
// the argument frame a managed function's stack-growth prologue spills its
// register arguments into.
//
// It mirrors arm64/goabi.go. The algorithms are the same -- they are dictated by
// the Go runtime and by internal/abi, not by the instruction set -- and only the
// register tables, and the absence of a hand-reserved stack link (the call
// instruction pushes the return address itself), differ.

// goRegisterSpill is one register argument's home slot. offset is measured from
// RSP at function entry, where [RSP] is the return address, so the caller's
// outgoing argument area -- the callee's argument frame -- begins at offset 8.
type goRegisterSpill struct {
	reg     Reg
	offset  int
	size    int
	float   bool
	pointer bool
}

type goRegisterSpillGroup struct {
	parts     []goABIPart
	reg       Reg
	size      int
	alignment int
	float     bool
	pointer   bool
}

// goArgumentFrame describes the argument frame the caller reserves for a
// function: its stack-passed arguments and stack results, then the home slots
// the stack-growth prologue spills the register arguments into.
//
// pointerWords covers every pointer word the frame can hold and is only correct
// in the entry window, where the prologue has just written the home slots
// before calling morestack. incomingPointerWords covers just the words the
// caller wrote before the call -- the stack-passed arguments -- and is therefore
// the map that holds at every safepoint in the body.
type goArgumentFrame struct {
	spills               []goRegisterSpill
	size                 int
	pointerWords         []int
	incomingPointerWords []int
}

// goABIPart is one flattened part of a Go aggregate together with the register
// ABIInternal assigned it.
type goABIPart struct {
	sub     ir.SubCls
	offset  int
	pointer bool
	reg     Reg
}

// flattenAggregate is ir.FlattenAggregate with a register slot added to each
// part.
func flattenAggregate(aggregate *ir.AggType) ([]goABIPart, bool) {
	neutral, flattenable := ir.FlattenAggregate(aggregate)
	if !flattenable {
		return nil, false
	}
	if len(neutral) == 0 {
		return nil, true
	}
	parts := make([]goABIPart, len(neutral))
	for index, part := range neutral {
		parts[index] = goABIPart{sub: part.Sub, offset: part.Offset, pointer: part.Pointer}
	}
	return parts, true
}

// assignGoAggregate assigns one complete flattened Go value under ABIInternal.
// If any of its parts cannot get a register, the whole value goes on the stack
// and the registers it did not use stay available to later values, which is
// Go's rule (internal/abi, "Function call argument and result passing").
func assignGoAggregate(assigner *argAssigner, aggregate *ir.AggType) (parts []goABIPart, onStack bool, stackOffset int) {
	parts, flattenable := flattenAggregate(aggregate)
	size, alignment := aggregate.Layout()
	if size == 0 || !flattenable {
		return nil, true, assigner.assignStack(size, alignment)
	}

	integerCount := 0
	floatCount := 0
	for _, part := range parts {
		if part.sub.Cls().IsFloat() {
			floatCount++
		} else {
			integerCount++
		}
	}
	if assigner.ngrn+integerCount > len(assigner.intRegs) || assigner.nsrn+floatCount > len(assigner.floatRegs) {
		return nil, true, assigner.assignStack(size, alignment)
	}

	for index := range parts {
		if parts[index].sub.Cls().IsFloat() {
			parts[index].reg = assigner.floatRegs[assigner.nsrn]
			assigner.nsrn++
		} else {
			parts[index].reg = assigner.intRegs[assigner.ngrn]
			assigner.ngrn++
		}
	}
	return parts, false, 0
}

// goArgumentFrameFor lays out f's argument frame. The order the parameters are
// assigned in has to be exactly the order lowerParams assigns them, and the
// home-slot placement exactly what the caller reserved (callStackBytes), since
// all three are computed independently.
func goArgumentFrameFor(function *ir.Func) goArgumentFrame {
	goInternal := function.UsesGoInternalCallConvention()
	assigner := newArgAssigner(goInternal)
	groups := make([]goRegisterSpillGroup, 0, len(function.Params))
	incomingOffsets := make(map[int]bool)
	prologueOffsets := make(map[int]bool)

	if !goInternal && function.RetAgg != nil && classifyAgg(function.RetAgg).memory {
		// The System V result buffer pointer arrives in RDI ahead of the
		// arguments. It points into the caller's frame, so it is a pointer the
		// stack copier has to adjust.
		location := assigner.assign(ir.ClsL)
		groups = append(groups, goRegisterSpillGroup{
			reg:       location.reg,
			size:      8,
			alignment: 8,
			pointer:   true,
		})
	}

	for parameterIndex := 0; parameterIndex < len(function.Params); {
		if group, ok := ir.ValueGroupAt(function.ParamGroups, parameterIndex); goInternal && ok {
			parts, onStack, stackOffset := assignGoAggregate(&assigner, group.Type)
			if onStack {
				for _, offset := range ir.AggregatePointerOffsets(group.Type) {
					incomingOffsets[stackOffset+offset] = true
				}
			} else {
				size, alignment := group.Type.Layout()
				groups = append(groups, goRegisterSpillGroup{
					parts:     parts,
					size:      size,
					alignment: alignment,
				})
			}
			parameterIndex += group.Count
			continue
		}

		if group, ok := ir.ValueGroupAt(function.ParamGroups, parameterIndex); !goInternal && ok {
			sysvGroups, stackPointers := sysvAggregateSpillGroups(&assigner, group.Type)
			groups = append(groups, sysvGroups...)
			for _, offset := range stackPointers {
				incomingOffsets[offset] = true
			}
			parameterIndex += group.Count
			continue
		}

		parameter := function.Params[parameterIndex]
		if parameter.Agg != nil {
			if goInternal {
				parts, onStack, stackOffset := assignGoAggregate(&assigner, parameter.Agg)
				if onStack {
					for _, offset := range ir.AggregatePointerOffsets(parameter.Agg) {
						incomingOffsets[stackOffset+offset] = true
					}
				} else {
					size, alignment := parameter.Agg.Layout()
					groups = append(groups, goRegisterSpillGroup{
						parts:     parts,
						size:      size,
						alignment: alignment,
					})
				}
				parameterIndex++
				continue
			}
			sysvGroups, stackPointers := sysvAggregateSpillGroups(&assigner, parameter.Agg)
			groups = append(groups, sysvGroups...)
			for _, offset := range stackPointers {
				incomingOffsets[offset] = true
			}
			parameterIndex++
			continue
		}

		location := assigner.assign(parameter.Cls)
		if location.onStack {
			if parameter.GCRef {
				incomingOffsets[location.stacky] = true
			}
			parameterIndex++
			continue
		}
		groups = append(groups, goRegisterSpillGroup{
			reg:       location.reg,
			size:      parameter.Cls.Size(),
			alignment: parameter.Cls.Size(),
			float:     parameter.Cls.IsFloat(),
			pointer:   parameter.GCRef,
		})
		parameterIndex++
	}

	resultEnd := roundUp(assigner.nsaa, 8)
	if function.RetAgg != nil && goInternal {
		results := newArgAssigner(true)
		results.nsaa = resultEnd
		_, onStack, stackOffset := assignGoAggregate(&results, function.RetAgg)
		if onStack {
			for _, offset := range ir.AggregatePointerOffsets(function.RetAgg) {
				prologueOffsets[stackOffset+offset] = true
			}
		}
		resultEnd = results.nsaa
	}

	cursor := roundUp(resultEnd, 8)
	var spills []goRegisterSpill
	for _, group := range groups {
		if group.size == 0 {
			continue
		}
		cursor = roundUp(cursor, group.alignment)
		if len(group.parts) == 0 {
			spill := goRegisterSpill{
				reg:     group.reg,
				offset:  8 + cursor,
				size:    group.size,
				float:   group.float,
				pointer: group.pointer,
			}
			spills = append(spills, spill)
			if spill.pointer {
				prologueOffsets[cursor] = true
			}
		} else {
			for _, part := range group.parts {
				spill := goRegisterSpill{
					reg:     part.reg,
					offset:  8 + cursor + part.offset,
					size:    part.sub.Size(),
					float:   part.sub.Cls().IsFloat(),
					pointer: part.pointer,
				}
				spills = append(spills, spill)
				if spill.pointer {
					prologueOffsets[cursor+part.offset] = true
				}
			}
		}
		cursor += group.size
	}
	cursor = roundUp(cursor, 8)
	entryOffsets := make(map[int]bool, len(incomingOffsets)+len(prologueOffsets))
	for offset := range incomingOffsets {
		entryOffsets[offset] = true
	}
	for offset := range prologueOffsets {
		entryOffsets[offset] = true
	}
	return goArgumentFrame{
		spills:               spills,
		size:                 cursor,
		pointerWords:         argumentPointerWords(entryOffsets),
		incomingPointerWords: argumentPointerWords(incomingOffsets),
	}
}

// sysvAggregateSpillGroups assigns one System V by-value aggregate parameter
// and returns the home-slot groups for the eightbytes that arrive in registers,
// or -- for one passed in memory -- the argument-frame offsets of its pointer
// words.
func sysvAggregateSpillGroups(assigner *argAssigner, aggregate *ir.AggType) ([]goRegisterSpillGroup, []int) {
	classification := classifyAgg(aggregate)
	pointerOffsets := ir.AggregatePointerOffsets(aggregate)
	registers, onStack, stackOffset := assigner.assignAgg(classification)
	if onStack {
		stackPointers := make([]int, 0, len(pointerOffsets))
		for _, offset := range pointerOffsets {
			stackPointers = append(stackPointers, stackOffset+offset)
		}
		return nil, stackPointers
	}
	pointerAt := make(map[int]bool, len(pointerOffsets))
	for _, offset := range pointerOffsets {
		pointerAt[offset] = true
	}
	parts := make([]goABIPart, len(registers))
	for index, register := range registers {
		sub := ir.SubL
		if register.float {
			sub = ir.SubD
		}
		parts[index] = goABIPart{
			sub:     sub,
			offset:  index * 8,
			pointer: pointerAt[index*8],
			reg:     register.reg,
		}
	}
	return []goRegisterSpillGroup{{
		parts:     parts,
		size:      len(registers) * 8,
		alignment: 8,
	}}, nil
}

// argumentPointerWords turns a set of argument-frame byte offsets into the
// sorted word indexes a Go stack map is written from, dropping any offset that
// is not word-aligned and so cannot name a pointer word.
func argumentPointerWords(offsets map[int]bool) []int {
	words := make([]int, 0, len(offsets))
	for offset := range offsets {
		if offset%8 == 0 {
			words = append(words, offset/8)
		}
	}
	sort.Ints(words)
	return words
}

type goABISpill struct {
	size      int
	alignment int
}

// homeSlotsEnd lays the register home slots after cursor and returns the end of
// the argument frame, rounded for the outgoing area.
func homeSlotsEnd(cursor int, homes []goABISpill) int {
	cursor = roundUp(cursor, 8)
	for _, home := range homes {
		if home.size == 0 {
			continue
		}
		cursor = roundUp(cursor, home.alignment)
		cursor += home.size
	}
	cursor = roundUp(cursor, 8)
	if cursor == 0 {
		return 0
	}
	return roundUp(cursor, 16)
}

// goCallStackBytes returns the outgoing area an ABIInternal call needs: its
// stack arguments, its stack results (ending at resultEnd), and then a home slot
// for every register argument, which the callee's stack-growth prologue spills
// into.
func goCallStackBytes(f *ir.Func, call *ir.Instr, resultEnd int) int {
	assigner := newArgAssigner(true)
	var homes []goABISpill
	arguments := call.Args[1:]
	for argumentIndex := 0; argumentIndex < len(arguments); {
		if group, ok := ir.ValueGroupAt(call.ArgGroups(), argumentIndex); ok {
			_, onStack, _ := assignGoAggregate(&assigner, group.Type)
			if !onStack {
				size, alignment := group.Type.Layout()
				homes = append(homes, goABISpill{size: size, alignment: alignment})
			}
			argumentIndex += group.Count
			continue
		}
		if aggregate := aggArgAt(call, argumentIndex); aggregate != nil {
			_, onStack, _ := assignGoAggregate(&assigner, aggregate)
			if !onStack {
				size, alignment := aggregate.Layout()
				homes = append(homes, goABISpill{size: size, alignment: alignment})
			}
			argumentIndex++
			continue
		}
		class := f.ClassOf(arguments[argumentIndex])
		location := assigner.assign(class)
		if !location.onStack {
			homes = append(homes, goABISpill{size: class.Size(), alignment: class.Size()})
		}
		argumentIndex++
	}
	return homeSlotsEnd(resultEnd, homes)
}

// platformCallStackBytes returns the outgoing area a managed function reserves
// for a System V call: the System V stack arguments at their usual offsets, then
// home slots for the register arguments in assignment order, so a managed
// callee's stack-growth prologue has somewhere to keep them. It must replay the
// assignment lowerCalls made, including the result-buffer pointer in RDI.
func platformCallStackBytes(f *ir.Func, call *ir.Instr) int {
	assigner := newArgAssigner(false)
	var homes []goABISpill
	if call.RetAgg() != nil && classifyAgg(call.RetAgg()).memory {
		assigner.assign(ir.ClsL)
		homes = append(homes, goABISpill{size: 8, alignment: 8})
	}
	arguments := call.Args[1:]
	for argumentIndex := 0; argumentIndex < len(arguments); argumentIndex++ {
		if group, ok := ir.ValueGroupAt(call.ArgGroups(), argumentIndex); ok {
			registers, onStack, _ := assigner.assignAgg(classifyAgg(group.Type))
			if !onStack && len(registers) > 0 {
				homes = append(homes, goABISpill{size: len(registers) * 8, alignment: 8})
			}
			argumentIndex += group.Count - 1
			continue
		}
		if aggregate := aggArgAt(call, argumentIndex); aggregate != nil {
			registers, onStack, _ := assigner.assignAgg(classifyAgg(aggregate))
			if !onStack && len(registers) > 0 {
				homes = append(homes, goABISpill{size: len(registers) * 8, alignment: 8})
			}
			continue
		}
		class := f.ClassOf(arguments[argumentIndex])
		location := assigner.assign(class)
		if !location.onStack {
			homes = append(homes, goABISpill{size: class.Size(), alignment: class.Size()})
		}
	}
	return homeSlotsEnd(assigner.nsaa, homes)
}

func lowerGoAggregateParam(f *ir.Func, parameter *ir.Temp, assigner *argAssigner) (parameters, reconstruction []ir.Instr) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, parameter.Agg)
	if onStack {
		stackedPar := ir.Instr{
			Op:  ir.OPar,
			Cls: ir.ClsL,
			To:  parameter.Ref(),
			Aux: int64(stackOffset),
		}
		stackedPar.SetRetAgg(parameter.Agg)
		return []ir.Instr{stackedPar}, nil
	}

	slot := f.AllocAggregate(parameter.Agg, &reconstruction)
	for _, part := range parts {
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		address := offsetAddr(f, slot, part.offset, &reconstruction)
		reconstruction = append(reconstruction, ir.Instr{
			Op:   ir.StoreOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			Args: []ir.Ref{pin, address},
		})
	}
	reconstruction = append(reconstruction, ir.Instr{
		Op:   ir.OCopy,
		Cls:  ir.ClsL,
		To:   parameter.Ref(),
		Args: []ir.Ref{slot},
	})
	return nil, reconstruction
}

func lowerGoValueParams(f *ir.Func, parameters []*ir.Temp, aggregate *ir.AggType, assigner *argAssigner) ([]ir.Instr, error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, aggregate)
	if onStack {
		parts, _ = flattenAggregate(aggregate)
	}
	if len(parts) != len(parameters) {
		return nil, fmt.Errorf("amd64: aggregate parameter has %d SSA parts, want %d", len(parameters), len(parts))
	}

	lowered := make([]ir.Instr, 0, len(parts))
	for index, part := range parts {
		parameter := parameters[index]
		partClass := part.sub.Cls()
		pointerClass := part.pointer && parameter.Cls == ir.ClsP && partClass == ir.ClsL
		if parameter.Cls != partClass && !pointerClass {
			return nil, fmt.Errorf("amd64: aggregate parameter part %d has class %s, want %s", index, parameter.Cls, part.sub.Cls())
		}
		if onStack {
			lowered = append(lowered, ir.Instr{
				Op:  ir.OPar,
				Cls: parameter.Cls,
				To:  parameter.Ref(),
				Aux: int64(stackOffset + part.offset),
			})
			continue
		}

		pin := newPinned(f, part.reg, parameter.Cls)
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		lowered = append(lowered, ir.Instr{
			Op:   ir.OPar,
			Cls:  parameter.Cls,
			To:   parameter.Ref(),
			Args: []ir.Ref{pin},
		})
	}
	return lowered, nil
}

// lowerGoAggregateArg lowers one by-value aggregate argument of an ABIInternal
// call. A register-assigned aggregate is loaded part by part into its
// registers; a stack-assigned one becomes a single stacked OArg carrying the
// aggregate type, which the emitter copies byte-exactly into the outgoing area
// (emitStackAggregateArgument).
func lowerGoAggregateArg(f *ir.Func, argument ir.Ref, aggregate *ir.AggType, assigner *argAssigner, out *[]ir.Instr, setup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, aggregate)
	if onStack {
		stackedArg := ir.Instr{
			Op:   ir.OArg,
			Cls:  ir.ClsL,
			To:   ir.R,
			Args: []ir.Ref{argument},
			Aux:  int64(stackOffset),
		}
		stackedArg.SetRetAgg(aggregate)
		setup = append(setup, stackedArg)
		return setup, pins
	}
	for _, part := range parts {
		address := offsetAddr(f, argument, part.offset, out)
		value := f.NewTemp("", part.sub.Cls())
		*out = append(*out, ir.Instr{
			Op:   ir.LoadOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			To:   value,
			Args: []ir.Ref{address},
		})
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		setup = append(setup, ir.Instr{
			Op:   ir.OArg,
			Cls:  part.sub.Cls(),
			To:   pin,
			Args: []ir.Ref{value},
		})
		pins = append(pins, pin)
	}
	return setup, pins
}

func lowerGoValueArg(f *ir.Func, arguments []ir.Ref, aggregate *ir.AggType, assigner *argAssigner, setup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref, error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, aggregate)
	if onStack {
		parts, _ = flattenAggregate(aggregate)
	}
	if len(parts) != len(arguments) {
		return nil, nil, fmt.Errorf("amd64: aggregate argument has %d SSA parts, want %d", len(arguments), len(parts))
	}

	for index, part := range parts {
		argument := arguments[index]
		class := f.ClassOf(argument)
		if class != part.sub.Cls() {
			return nil, nil, fmt.Errorf("amd64: aggregate argument part %d has class %s, want %s", index, class, part.sub.Cls())
		}
		if onStack {
			setup = append(setup, ir.Instr{
				Op:   ir.OArg,
				Cls:  class,
				To:   ir.R,
				Args: []ir.Ref{argument},
				Aux:  int64(stackOffset + part.offset),
			})
			continue
		}

		pin := newPinned(f, part.reg, class)
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		setup = append(setup, ir.Instr{
			Op:   ir.OArg,
			Cls:  class,
			To:   pin,
			Args: []ir.Ref{argument},
		})
		pins = append(pins, pin)
	}
	return setup, pins, nil
}

// goCallResult is how an ABIInternal call's result reaches its destination.
type goCallResult struct {
	callTo      ir.Ref
	callClass   ir.Cls
	definitions []ir.Ref
	post        []ir.Instr
	stackResult ir.Ref
	stackOffset int
	stackEnd    int
}

func lowerGoAggregateResult(f *ir.Func, destination ir.Ref, aggregate *ir.AggType, stackBase int, out *[]ir.Instr) goCallResult {
	resultAssigner := newArgAssigner(true)
	resultAssigner.nsaa = stackBase
	parts, onStack, resultOffset := assignGoAggregate(&resultAssigner, aggregate)
	slot := f.AllocAggregate(aggregate, out)
	var result goCallResult
	result.stackEnd = resultAssigner.nsaa
	if onStack {
		result.post = append(result.post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: destination, Args: []ir.Ref{slot}})
		result.stackResult = slot
		result.stackOffset = resultOffset
		return result
	}

	for index, part := range parts {
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		if index == 0 {
			result.callTo = pin
			result.callClass = part.sub.Cls()
		} else {
			result.definitions = append(result.definitions, pin)
		}
		address := offsetAddr(f, slot, part.offset, &result.post)
		result.post = append(result.post, ir.Instr{
			Op:   ir.StoreOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			Args: []ir.Ref{pin, address},
		})
	}
	result.post = append(result.post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: destination, Args: []ir.Ref{slot}})
	return result
}

func lowerGoValueResult(f *ir.Func, destinations []ir.Ref, aggregate *ir.AggType, stackBase int, out *[]ir.Instr) (goCallResult, error) {
	resultAssigner := newArgAssigner(true)
	resultAssigner.nsaa = stackBase
	parts, onStack, resultOffset := assignGoAggregate(&resultAssigner, aggregate)
	if onStack {
		parts, _ = flattenAggregate(aggregate)
	}
	var result goCallResult
	result.stackEnd = resultAssigner.nsaa
	if len(parts) != len(destinations) {
		return result, fmt.Errorf("amd64: aggregate result has %d SSA parts, want %d", len(destinations), len(parts))
	}

	if onStack {
		slot := f.AllocAggregate(aggregate, out)
		for index, part := range parts {
			destination := destinations[index]
			if f.ClassOf(destination) != part.sub.Cls() {
				return result, fmt.Errorf("amd64: aggregate result part %d has class %s, want %s", index, f.ClassOf(destination), part.sub.Cls())
			}
			address := offsetAddr(f, slot, part.offset, &result.post)
			result.post = append(result.post, ir.Instr{
				Op:   ir.LoadOpForSub(part.sub),
				Cls:  part.sub.Cls(),
				To:   destination,
				Args: []ir.Ref{address},
			})
		}
		result.stackResult = slot
		result.stackOffset = resultOffset
		return result, nil
	}

	for index, part := range parts {
		destination := destinations[index]
		if f.ClassOf(destination) != part.sub.Cls() {
			return result, fmt.Errorf("amd64: aggregate result part %d has class %s, want %s", index, f.ClassOf(destination), part.sub.Cls())
		}
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		if index == 0 {
			result.callTo = pin
			result.callClass = part.sub.Cls()
		} else {
			result.definitions = append(result.definitions, pin)
		}
		result.post = append(result.post, ir.Instr{
			Op:   ir.OCopy,
			Cls:  part.sub.Cls(),
			To:   destination,
			Args: []ir.Ref{pin},
		})
	}
	return result, nil
}

func lowerGoAggregateReturn(f *ir.Func, block *ir.Block, resultBuffer ir.Ref) error {
	resultAssigner := newArgAssigner(true)
	parts, onStack, _ := assignGoAggregate(&resultAssigner, f.RetAgg)
	if onStack {
		if resultBuffer.IsNone() {
			return fmt.Errorf("amd64: missing stack result buffer for Go ABI aggregate")
		}
		size, _ := f.RetAgg.Layout()
		emitMemcpy(f, resultBuffer, block.Jmp.Arg, size, &block.Instrs)
		block.Jmp = ir.Jmp{Kind: ir.JmpRet}
		return nil
	}

	pointer := block.Jmp.Arg
	var resultRegisters []ir.Ref
	for _, part := range parts {
		address := offsetAddr(f, pointer, part.offset, &block.Instrs)
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		block.Instrs = append(block.Instrs, ir.Instr{
			Op:   ir.LoadOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			To:   pin,
			Args: []ir.Ref{address},
		})
		resultRegisters = append(resultRegisters, pin)
	}
	if len(resultRegisters) == 0 {
		block.Jmp = ir.Jmp{Kind: ir.JmpRet}
		return nil
	}
	block.Jmp.Arg = resultRegisters[0]
	block.Jmp.Args = resultRegisters[1:]
	return nil
}

func lowerGoValueReturn(f *ir.Func, block *ir.Block, resultBuffer ir.Ref) error {
	values := append([]ir.Ref{block.Jmp.Arg}, block.Jmp.Args...)
	resultAssigner := newArgAssigner(true)
	parts, onStack, _ := assignGoAggregate(&resultAssigner, f.RetAgg)
	if onStack {
		parts, _ = flattenAggregate(f.RetAgg)
	}
	if len(parts) != len(values) {
		return fmt.Errorf("amd64: aggregate return has %d SSA parts, want %d", len(values), len(parts))
	}

	if onStack {
		if resultBuffer.IsNone() {
			return fmt.Errorf("amd64: missing stack result buffer for Go ABI aggregate")
		}
		for index, part := range parts {
			value := values[index]
			if f.ClassOf(value) != part.sub.Cls() {
				return fmt.Errorf("amd64: aggregate return part %d has class %s, want %s", index, f.ClassOf(value), part.sub.Cls())
			}
			address := offsetAddr(f, resultBuffer, part.offset, &block.Instrs)
			block.Instrs = append(block.Instrs, ir.Instr{
				Op:   ir.StoreOpForSub(part.sub),
				Cls:  part.sub.Cls(),
				Args: []ir.Ref{value, address},
			})
		}
		block.Jmp = ir.Jmp{Kind: ir.JmpRet}
		return nil
	}

	resultRegisters := make([]ir.Ref, 0, len(parts))
	for index, part := range parts {
		value := values[index]
		if f.ClassOf(value) != part.sub.Cls() {
			return fmt.Errorf("amd64: aggregate return part %d has class %s, want %s", index, f.ClassOf(value), part.sub.Cls())
		}
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		block.Instrs = append(block.Instrs, ir.Instr{
			Op:   ir.OCopy,
			Cls:  part.sub.Cls(),
			To:   pin,
			Args: []ir.Ref{value},
		})
		resultRegisters = append(resultRegisters, pin)
	}
	block.Jmp.Arg = resultRegisters[0]
	block.Jmp.Args = resultRegisters[1:]
	return nil
}

// stabilizeClosureContext copies an incoming ABIInternal closure environment
// out of RDX right after the parameters are taken. RDX is fixed only on entry:
// it is also the high half of division and of the widening multiply, so the
// ordinary temporary the body reads has to be a copy the allocator may place
// (and spill, for precise GC) like any other value.
func stabilizeClosureContext(function *ir.Func) {
	if !function.HasClosureContext {
		return
	}

	var incoming ir.Ref
	for _, temporary := range function.Temps {
		if temporary != nil && temporary.ClosureContext {
			incoming = temporary.Ref()
			break
		}
	}
	if incoming.IsNone() {
		return
	}

	incomingTemporary := function.Temp(incoming)
	saved := function.NewTemp("closure.saved", incomingTemporary.Cls)
	savedTemporary := function.Temp(saved)
	savedTemporary.GCRef = incomingTemporary.GCRef
	savedTemporary.GCType = incomingTemporary.GCType

	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			for index, argument := range phi.Args {
				if argument == incoming {
					phi.Args[index] = saved
				}
			}
		}
		for index := range block.Instrs {
			for argumentIndex, argument := range block.Instrs[index].Args {
				if argument == incoming {
					block.Instrs[index].Args[argumentIndex] = saved
				}
			}
			if block.Instrs[index].ClosureContext() == incoming {
				block.Instrs[index].SetClosureContext(saved)
			}
		}
		if block.Jmp.Arg == incoming {
			block.Jmp.Arg = saved
		}
		for index, argument := range block.Jmp.Args {
			if argument == incoming {
				block.Jmp.Args[index] = saved
			}
		}
	}

	copyContext := ir.Instr{
		Op:   ir.OCopy,
		Cls:  incomingTemporary.Cls,
		To:   saved,
		Args: []ir.Ref{incoming},
	}
	insertAt := 0
	for insertAt < len(function.Start.Instrs) && function.Start.Instrs[insertAt].Op == ir.OPar {
		insertAt++
	}
	function.Start.Instrs = append(function.Start.Instrs, ir.Instr{})
	copy(function.Start.Instrs[insertAt+1:], function.Start.Instrs[insertAt:])
	function.Start.Instrs[insertAt] = copyContext
}
