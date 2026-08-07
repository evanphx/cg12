package arm64

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/ir"
)

const goStackLinkSize = 8

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
// The two pointer maps differ in when they are valid, which is why they are two
// maps. pointerWords covers every pointer word the frame can hold and is only
// correct in the entry window, where the prologue has just written the home
// slots before calling morestack. incomingPointerWords covers just the words the
// caller wrote before the call -- the stack-passed arguments -- and is therefore
// the map that holds for the whole call, at every safepoint in the body.
type goArgumentFrame struct {
	spills               []goRegisterSpill
	size                 int
	pointerWords         []int
	incomingPointerWords []int
}

// goRegisterSpills returns the managed-frame entry spills used by the standard
// stack-growth prologue. Register arguments have home slots after all stack
// arguments (and ABIInternal stack results). morestack copies those slots with
// the caller's frame, and the retry path reloads the argument registers.
func goRegisterSpills(function *ir.Func) []goRegisterSpill {
	return goArgumentFrameFor(function).spills
}

func goArgumentFrameFor(function *ir.Func) goArgumentFrame {
	goInternal := function.UsesGoInternalCallConvention()
	assigner := newArgAssigner(goInternal)
	groups := make([]goRegisterSpillGroup, 0, len(function.Params))
	// incomingOffsets are the stack-passed argument words, written by the caller
	// before the call and so valid for its whole duration. prologueOffsets are the
	// words only the callee ever writes -- the stack result area and the register
	// home slots -- which hold nothing meaningful outside the entry window.
	incomingOffsets := make(map[int]bool)
	prologueOffsets := make(map[int]bool)
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
			if classifyAgg(group.Type).kind != aggMemory {
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
			parts, flattenable := flattenAggregate(group.Type)
			if !flattenable || len(parts) != group.Count {
				parts = make([]goABIPart, group.Count)
			}
			for partIndex := 0; partIndex < group.Count; partIndex++ {
				parameter := function.Params[parameterIndex+partIndex]
				location := assigner.assign(parameter.Cls)
				pointer := parameter.GCRef || parts[partIndex].pointer
				if location.onStack {
					if pointer {
						incomingOffsets[location.stacky] = true
					}
					continue
				}
				groups = append(groups, goRegisterSpillGroup{
					reg:       location.reg,
					size:      parameter.Cls.Size(),
					alignment: parameter.Cls.Size(),
					float:     parameter.Cls.IsFloat(),
					pointer:   pointer,
				})
			}
			parameterIndex += group.Count
			continue
		}

		parameter := function.Params[parameterIndex]
		if parameter.Agg != nil {
			if !goInternal {
				aapcsGroups, stackPointers := aapcsAggregateSpillGroups(&assigner, parameter.Agg)
				groups = append(groups, aapcsGroups...)
				for _, offset := range stackPointers {
					incomingOffsets[offset] = true
				}
				parameterIndex++
				continue
			}
			parts, onStack, stackOffset := assignGoAggregate(&assigner, parameter.Agg)
			if onStack {
				for _, offset := range ir.AggregatePointerOffsets(parameter.Agg) {
					incomingOffsets[stackOffset+offset] = true
				}
				parameterIndex++
				continue
			}
			size, alignment := parameter.Agg.Layout()
			groups = append(groups, goRegisterSpillGroup{
				parts:     parts,
				size:      size,
				alignment: alignment,
			})
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
	if function.HasClosureContext {
		for _, temporary := range function.Temps {
			if !temporary.Fixed || temporary.Reg != 26 {
				continue
			}
			groups = append(groups, goRegisterSpillGroup{
				reg:       Reg(temporary.Reg),
				size:      8,
				alignment: 8,
				pointer:   true,
			})
			break
		}
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
	if function.RetAgg != nil && !goInternal && classifyAgg(function.RetAgg).kind == aggMemory {
		groups = append(groups, goRegisterSpillGroup{
			reg:       X8,
			size:      8,
			alignment: 8,
			pointer:   true,
		})
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
				offset:  goStackLinkSize + cursor,
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
					offset:  goStackLinkSize + cursor + part.offset,
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

func aapcsAggregateSpillGroups(assigner *argAssigner, aggregate *ir.AggType) ([]goRegisterSpillGroup, []int) {
	classification := classifyAgg(aggregate)
	if classification.size == 0 {
		return nil, nil
	}
	pointerOffsets := ir.AggregatePointerOffsets(aggregate)
	pointerAt := make(map[int]bool, len(pointerOffsets))
	for _, offset := range pointerOffsets {
		pointerAt[offset] = true
	}

	switch classification.kind {
	case aggMemory:
		location := assigner.assign(ir.ClsL)
		if location.onStack {
			return nil, []int{location.stacky}
		}
		return []goRegisterSpillGroup{{
			reg:       location.reg,
			size:      8,
			alignment: 8,
			pointer:   true,
		}}, nil
	case aggGP:
		registers, onStack, stackOffset := assigner.assignGP(classification.nregs, classification.size)
		if onStack {
			stackPointers := make([]int, 0, len(pointerOffsets))
			for _, offset := range pointerOffsets {
				stackPointers = append(stackPointers, stackOffset+offset)
			}
			return nil, stackPointers
		}
		parts := make([]goABIPart, len(registers))
		for index, register := range registers {
			parts[index] = goABIPart{
				sub:     ir.SubL,
				offset:  index * 8,
				pointer: pointerAt[index*8],
				reg:     register,
			}
		}
		return []goRegisterSpillGroup{{
			parts:     parts,
			size:      len(registers) * 8,
			alignment: 8,
		}}, nil
	case aggHFA:
		registers, onStack, _ := assigner.assignHFA(classification.nregs, classification.size)
		if onStack {
			return nil, nil
		}
		parts := make([]goABIPart, len(registers))
		for index, register := range registers {
			parts[index] = goABIPart{
				sub:    semanticSubClass(classification.elem),
				offset: index * classification.elem.Size(),
				reg:    register,
			}
		}
		return []goRegisterSpillGroup{{
			parts:     parts,
			size:      classification.size,
			alignment: classification.elem.Size(),
		}}, nil
	}
	return nil, nil
}

func semanticSubClass(class ir.Cls) ir.SubCls {
	switch class {
	case ir.ClsS:
		return ir.SubS
	case ir.ClsD:
		return ir.SubD
	case ir.ClsQ:
		return ir.SubQ
	default:
		return ir.SubL
	}
}

// maxOutgoingCallSize returns the stacked-argument area the frame must reserve
// below x29 for the calls that write into a fixed outgoing area. A managed frame
// keeps SP stable across every call, so all of its calls count; an unmanaged
// frame moves SP around each AAPCS64 call instead (see dynamicAAPCSFrame in
// callSequence), so only its ABIInternal calls -- which always use a fixed area
// so the runtime can walk the frame -- need room reserved here. Resolving that
// per call must agree with the lowering and the emitter, so it uses the same
// object-wide convention index they do.
func maxOutgoingCallSize(function *ir.Func, conventions calleeConventions) int {
	maximum := 0
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall || instruction.Tail {
				continue
			}
			if !function.UsesManagedFrame() && !conventions.goInternalCall(function, instruction) {
				continue
			}
			maximum = max(maximum, int(instruction.Aux))
		}
	}
	return maximum
}

// goABIPart is one flattened part of a Go aggregate (ir.AggPart) together with
// the register AAPCS64 or ABIInternal assigned it.
type goABIPart struct {
	sub     ir.SubCls
	offset  int
	pointer bool
	reg     Reg
}

// flattenAggregate is ir.FlattenAggregate with an arm64 register slot added to
// each part. The flattening itself is neutral; only the assignment of a part to
// a register is architectural, so only that lives here.
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

// assignGoAggregate assigns one complete flattened Go value. If any of its
// parts cannot fit, the whole value is placed on the stack. ABIInternal leaves
// unused registers available to later values, while AAPCS64 exhausts the
// corresponding register bank before assigning subsequent values.
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
	if assigner.ngrn+integerCount > assigner.intRegs || assigner.nsrn+floatCount > assigner.floatRegs {
		if !assigner.goABI {
			if integerCount > 0 {
				assigner.ngrn = assigner.intRegs
			}
			if floatCount > 0 {
				assigner.nsrn = assigner.floatRegs
			}
		}
		return nil, true, assigner.assignStack(size, alignment)
	}

	for index := range parts {
		if parts[index].sub.Cls().IsFloat() {
			parts[index].reg = vReg(assigner.nsrn)
			assigner.nsrn++
		} else {
			parts[index].reg = X0 + Reg(assigner.ngrn)
			assigner.ngrn++
		}
	}
	return parts, false, 0
}

func (a *argAssigner) assignStack(size, alignment int) int {
	if alignment <= 0 {
		alignment = 1
	}
	offset := roundUp(a.nsaa, alignment)
	a.nsaa = offset + size
	return offset
}

func lowerGoAggregateParam(f *ir.Func, parameter *ir.Temp, assigner *argAssigner) (parameters, reconstruction []ir.Instr, err error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, parameter.Agg)
	if onStack {
		return []ir.Instr{{
			Op:     ir.OPar,
			Cls:    ir.ClsL,
			To:     parameter.Ref(),
			Aux:    int64(stackOffset),
			RetAgg: parameter.Agg,
		}}, nil, nil
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
	return nil, reconstruction, nil
}

func lowerGoValueParams(f *ir.Func, parameters []*ir.Temp, aggregate *ir.AggType, assigner *argAssigner) ([]ir.Instr, error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, aggregate)
	if onStack {
		parts, _ = flattenAggregate(aggregate)
	}
	if len(parts) != len(parameters) {
		return nil, fmt.Errorf("arm64: aggregate parameter has %d SSA parts, want %d", len(parameters), len(parts))
	}

	lowered := make([]ir.Instr, 0, len(parts))
	for index, part := range parts {
		parameter := parameters[index]
		partClass := part.sub.Cls()
		pointerClass := part.pointer && parameter.Cls == ir.ClsP && partClass == ir.ClsL
		if parameter.Cls != partClass && !pointerClass {
			return nil, fmt.Errorf("arm64: aggregate parameter part %d has class %s, want %s", index, parameter.Cls, part.sub.Cls())
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

func lowerGoAggregateArg(f *ir.Func, argument ir.Ref, aggregate *ir.AggType, assigner *argAssigner, out *[]ir.Instr, setup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref, error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, aggregate)
	if onStack {
		setup = append(setup, ir.Instr{
			Op:     ir.OArg,
			Cls:    ir.ClsL,
			To:     ir.R,
			Args:   []ir.Ref{argument},
			Aux:    int64(stackOffset),
			RetAgg: aggregate,
		})
		return setup, pins, nil
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
	return setup, pins, nil
}

func lowerGoValueArg(f *ir.Func, arguments []ir.Ref, aggregate *ir.AggType, assigner *argAssigner, setup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref, error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, aggregate)
	if onStack {
		parts, _ = flattenAggregate(aggregate)
	}
	if len(parts) != len(arguments) {
		return nil, nil, fmt.Errorf("arm64: aggregate argument has %d SSA parts, want %d", len(arguments), len(parts))
	}

	for index, part := range parts {
		argument := arguments[index]
		class := f.ClassOf(argument)
		if class != part.sub.Cls() {
			return nil, nil, fmt.Errorf("arm64: aggregate argument part %d has class %s, want %s", index, class, part.sub.Cls())
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

func lowerGoAggregateResult(f *ir.Func, destination ir.Ref, aggregate *ir.AggType, stackBase int, out *[]ir.Instr, setup []ir.Instr, pins []ir.Ref) (callTo ir.Ref, callClass ir.Cls, definitions []ir.Ref, newSetup []ir.Instr, newPins []ir.Ref, post []ir.Instr, stackResult ir.Ref, stackOffset int, stackEnd int, err error) {
	resultAssigner := newArgAssigner(true)
	resultAssigner.nsaa = stackBase
	parts, onStack, resultOffset := assignGoAggregate(&resultAssigner, aggregate)
	if onStack {
		slot := f.AllocAggregate(aggregate, out)
		post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: destination, Args: []ir.Ref{slot}})
		return ir.R, 0, nil, setup, pins, post, slot, resultOffset, resultAssigner.nsaa, nil
	}

	slot := f.AllocAggregate(aggregate, out)
	for index, part := range parts {
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		if index == 0 {
			callTo = pin
			callClass = part.sub.Cls()
		} else {
			definitions = append(definitions, pin)
		}
		address := offsetAddr(f, slot, part.offset, &post)
		post = append(post, ir.Instr{
			Op:   ir.StoreOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			Args: []ir.Ref{pin, address},
		})
	}
	post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: destination, Args: []ir.Ref{slot}})
	return callTo, callClass, definitions, setup, pins, post, ir.R, 0, resultAssigner.nsaa, nil
}

func lowerGoValueResult(f *ir.Func, destinations []ir.Ref, aggregate *ir.AggType, stackBase int, out *[]ir.Instr, setup []ir.Instr, pins []ir.Ref) (callTo ir.Ref, callClass ir.Cls, definitions []ir.Ref, newSetup []ir.Instr, newPins []ir.Ref, post []ir.Instr, stackResult ir.Ref, stackOffset int, stackEnd int, err error) {
	resultAssigner := newArgAssigner(true)
	resultAssigner.nsaa = stackBase
	parts, onStack, resultOffset := assignGoAggregate(&resultAssigner, aggregate)
	if onStack {
		parts, _ = flattenAggregate(aggregate)
	}
	if len(parts) != len(destinations) {
		err = fmt.Errorf("arm64: aggregate result has %d SSA parts, want %d", len(destinations), len(parts))
		return
	}

	if onStack {
		slot := f.AllocAggregate(aggregate, out)
		for index, part := range parts {
			destination := destinations[index]
			if f.ClassOf(destination) != part.sub.Cls() {
				err = fmt.Errorf("arm64: aggregate result part %d has class %s, want %s", index, f.ClassOf(destination), part.sub.Cls())
				return
			}
			address := offsetAddr(f, slot, part.offset, &post)
			post = append(post, ir.Instr{
				Op:   ir.LoadOpForSub(part.sub),
				Cls:  part.sub.Cls(),
				To:   destination,
				Args: []ir.Ref{address},
			})
		}
		return ir.R, 0, nil, setup, pins, post, slot, resultOffset, resultAssigner.nsaa, nil
	}

	for index, part := range parts {
		destination := destinations[index]
		if f.ClassOf(destination) != part.sub.Cls() {
			err = fmt.Errorf("arm64: aggregate result part %d has class %s, want %s", index, f.ClassOf(destination), part.sub.Cls())
			return
		}
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		if index == 0 {
			callTo = pin
			callClass = part.sub.Cls()
		} else {
			definitions = append(definitions, pin)
		}
		post = append(post, ir.Instr{
			Op:   ir.OCopy,
			Cls:  part.sub.Cls(),
			To:   destination,
			Args: []ir.Ref{pin},
		})
	}
	return callTo, callClass, definitions, setup, pins, post, ir.R, 0, resultAssigner.nsaa, nil
}

func lowerGoAggregateReturn(f *ir.Func, block *ir.Block, resultBuffer ir.Ref) error {
	resultAssigner := newArgAssigner(true)
	parts, onStack, _ := assignGoAggregate(&resultAssigner, f.RetAgg)
	if onStack {
		if resultBuffer.IsNone() {
			return fmt.Errorf("arm64: missing stack result buffer for Go ABI aggregate")
		}
		size, _ := f.RetAgg.Layout()
		emitMemcpy(f, resultBuffer, block.Jmp.Arg, size, &block.Instrs)
		block.Jmp = ir.Jmp{Kind: ir.JmpRet}
		return nil
	}

	pointer := block.Jmp.Arg
	var resultRegisters []ir.Ref
	for _, part := range parts {
		address := returnFieldAddress(f, pointer, part.offset, &block.Instrs)
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
		return fmt.Errorf("arm64: zero-sized Go ABI aggregate results are not yet supported")
	}
	block.Jmp.Arg = resultRegisters[0]
	block.Jmp.Args = resultRegisters[1:]
	return nil
}

func returnFieldAddress(f *ir.Func, base ir.Ref, offset int, out *[]ir.Instr) ir.Ref {
	if offset == 0 {
		return base
	}
	address := newPinned(f, scratch0, ir.ClsL)
	*out = append(*out, ir.Instr{
		Op:   ir.OAdd,
		Cls:  ir.ClsL,
		To:   address,
		Args: []ir.Ref{base, f.Long(int64(offset))},
	})
	return address
}

func lowerGoValueReturn(f *ir.Func, block *ir.Block, resultBuffer ir.Ref) error {
	values := append([]ir.Ref{block.Jmp.Arg}, block.Jmp.Args...)
	resultAssigner := newArgAssigner(true)
	parts, onStack, _ := assignGoAggregate(&resultAssigner, f.RetAgg)
	if onStack {
		parts, _ = flattenAggregate(f.RetAgg)
	}
	if len(parts) != len(values) {
		return fmt.Errorf("arm64: aggregate return has %d SSA parts, want %d", len(values), len(parts))
	}

	if onStack {
		if resultBuffer.IsNone() {
			return fmt.Errorf("arm64: missing stack result buffer for Go ABI aggregate")
		}
		for index, part := range parts {
			value := values[index]
			if f.ClassOf(value) != part.sub.Cls() {
				return fmt.Errorf("arm64: aggregate return part %d has class %s, want %s", index, f.ClassOf(value), part.sub.Cls())
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
			return fmt.Errorf("arm64: aggregate return part %d has class %s, want %s", index, f.ClassOf(value), part.sub.Cls())
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

type goABISpill struct {
	size      int
	alignment int
}

func goCallStackBytes(f *ir.Func, call *ir.Instr, resultEnd int) int {
	assigner := newArgAssigner(true)
	var spills []goABISpill
	arguments := call.Args[1:]
	for argumentIndex := 0; argumentIndex < len(arguments); {
		if group, ok := ir.ValueGroupAt(call.ArgGroups(), argumentIndex); ok {
			_, onStack, _ := assignGoAggregate(&assigner, group.Type)
			if !onStack {
				size, alignment := group.Type.Layout()
				spills = append(spills, goABISpill{size: size, alignment: alignment})
			}
			argumentIndex += group.Count
			continue
		}

		argument := arguments[argumentIndex]
		aggregate := aggArgAt(call, argumentIndex)
		if aggregate != nil {
			_, onStack, _ := assignGoAggregate(&assigner, aggregate)
			if !onStack {
				size, alignment := aggregate.Layout()
				spills = append(spills, goABISpill{size: size, alignment: alignment})
			}
			argumentIndex++
			continue
		}

		class := f.ClassOf(argument)
		location := assigner.assign(class)
		if !location.onStack {
			spills = append(spills, goABISpill{size: class.Size(), alignment: class.Size()})
		}
		argumentIndex++
	}

	cursor := roundUp(resultEnd, 8)
	for _, spill := range spills {
		cursor = roundUp(cursor, spill.alignment)
		cursor += spill.size
	}
	if call.ClosureCall {
		cursor = roundUp(cursor, 8)
		cursor += 8
	}
	cursor = roundUp(cursor, 8)
	if cursor == 0 {
		return 0
	}
	return roundUp(goStackLinkSize+cursor, 16)
}

// aapcsCallStackBytes returns the temporary call area a managed AAPCS64 caller
// must reserve. Stack arguments retain their ordinary AAPCS64 offsets at the
// bottom of the area. Register arguments are given Go stack-growth home slots
// above them so a callee can preserve them across morestack.
func aapcsCallStackBytes(f *ir.Func, call *ir.Instr) int {
	assigner := newArgAssigner(false)
	var homes []goABISpill
	arguments := call.Args[1:]
	for argumentIndex := 0; argumentIndex < len(arguments); {
		if group, ok := ir.ValueGroupAt(call.ArgGroups(), argumentIndex); ok {
			for partIndex := 0; partIndex < group.Count; partIndex++ {
				class := f.ClassOf(arguments[argumentIndex+partIndex])
				location := assigner.assign(class)
				if !location.onStack {
					homes = append(homes, goABISpill{size: class.Size(), alignment: class.Size()})
				}
			}
			argumentIndex += group.Count
			continue
		}

		aggregate := aggArgAt(call, argumentIndex)
		if aggregate != nil {
			classification := classifyAgg(aggregate)
			switch classification.kind {
			case aggMemory:
				location := assigner.assign(ir.ClsL)
				if !location.onStack {
					homes = append(homes, goABISpill{size: 8, alignment: 8})
				}
			case aggGP:
				registers, onStack, _ := assigner.assignGP(classification.nregs, classification.size)
				if !onStack && len(registers) > 0 {
					homes = append(homes, goABISpill{size: len(registers) * 8, alignment: 8})
				}
			case aggHFA:
				registers, onStack, _ := assigner.assignHFA(classification.nregs, classification.size)
				if !onStack && len(registers) > 0 {
					homes = append(homes, goABISpill{
						size:      classification.size,
						alignment: classification.elem.Size(),
					})
				}
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

	if call.RetAgg != nil && classifyAgg(call.RetAgg).kind == aggMemory {
		homes = append(homes, goABISpill{size: 8, alignment: 8})
	}

	cursor := roundUp(assigner.nsaa, 8)
	for _, home := range homes {
		if home.size == 0 {
			continue
		}
		cursor = roundUp(cursor, home.alignment)
		cursor += home.size
	}
	if call.ClosureCall {
		cursor = roundUp(cursor, 8)
		cursor += 8
	}
	cursor = roundUp(cursor, 8)
	if cursor == 0 {
		return 0
	}
	return roundUp(goStackLinkSize+cursor, 16)
}
