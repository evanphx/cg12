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

type goArgumentFrame struct {
	spills       []goRegisterSpill
	size         int
	pointerWords []int
}

// goRegisterSpills returns the ABIInternal entry spills used by the standard
// stack-growth prologue. Register arguments have home slots after all stack
// arguments and stack results. morestack copies those slots with the caller's
// frame, and the retry path reloads the argument registers from them.
func goRegisterSpills(function *ir.Func) []goRegisterSpill {
	return goArgumentFrameFor(function).spills
}

func goArgumentFrameFor(function *ir.Func) goArgumentFrame {
	assigner := newArgAssigner(true)
	groups := make([]goRegisterSpillGroup, 0, len(function.Params))
	pointerOffsets := make(map[int]bool)
	for parameterIndex := 0; parameterIndex < len(function.Params); {
		if group, ok := valueGroupAt(function.ParamGroups, parameterIndex); ok {
			parts, onStack, stackOffset := assignGoAggregate(&assigner, group.Type)
			if onStack {
				for _, offset := range goAggregatePointerOffsets(group.Type) {
					pointerOffsets[stackOffset+offset] = true
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

		parameter := function.Params[parameterIndex]
		if parameter.Agg != nil {
			parts, onStack, stackOffset := assignGoAggregate(&assigner, parameter.Agg)
			if onStack {
				for _, offset := range goAggregatePointerOffsets(parameter.Agg) {
					pointerOffsets[stackOffset+offset] = true
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
				pointerOffsets[location.stacky] = true
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
	if function.RetAgg != nil {
		results := newArgAssigner(true)
		results.nsaa = resultEnd
		_, onStack, stackOffset := assignGoAggregate(&results, function.RetAgg)
		if onStack {
			for _, offset := range goAggregatePointerOffsets(function.RetAgg) {
				pointerOffsets[stackOffset+offset] = true
			}
		}
		resultEnd = results.nsaa
	}

	cursor := roundUp(resultEnd, 8)
	var spills []goRegisterSpill
	for _, group := range groups {
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
				pointerOffsets[cursor] = true
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
					pointerOffsets[cursor+part.offset] = true
				}
			}
		}
		cursor += group.size
	}
	cursor = roundUp(cursor, 8)
	pointerWords := make([]int, 0, len(pointerOffsets))
	for offset := range pointerOffsets {
		if offset%8 == 0 {
			pointerWords = append(pointerWords, offset/8)
		}
	}
	sort.Ints(pointerWords)
	return goArgumentFrame{spills: spills, size: cursor, pointerWords: pointerWords}
}

func valueGroupAt(groups []ir.ValueGroup, index int) (ir.ValueGroup, bool) {
	for _, group := range groups {
		if group.Index == index {
			return group, true
		}
	}
	return ir.ValueGroup{}, false
}

func goAggregatePointerOffsets(aggregate *ir.AggType) []int {
	var offsets []int
	var walk func(*ir.AggType, int)
	walk = func(current *ir.AggType, base int) {
		if current == nil || current.Opaque || current.Union {
			return
		}
		offset := 0
		for _, field := range current.Fields {
			size, alignment := goFieldSizeAlign(field)
			offset = roundUp(offset, alignment)
			count := field.Count
			if count <= 0 {
				count = 1
			}
			for index := 0; index < count; index++ {
				fieldOffset := base + offset + index*size
				if field.Type != nil {
					walk(field.Type, fieldOffset)
				} else if field.Pointer {
					offsets = append(offsets, fieldOffset)
				}
			}
			offset += size * count
		}
	}
	walk(aggregate, 0)
	return offsets
}

func maxOutgoingCallSize(function *ir.Func) int {
	maximum := 0
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall || instruction.Tail {
				continue
			}
			maximum = max(maximum, int(instruction.Aux))
		}
	}
	return maximum
}

// goABIPart is one recursively flattened base value of a Go aggregate. Go's
// ABIInternal assigns each part to its own integer or floating-point register.
type goABIPart struct {
	sub     ir.SubCls
	offset  int
	pointer bool
	reg     Reg
}

func flattenGoAggregate(aggregate *ir.AggType) ([]goABIPart, bool) {
	if aggregate == nil || aggregate.Opaque || aggregate.Union {
		return nil, false
	}

	var parts []goABIPart
	var walk func(*ir.AggType, int) bool
	walk = func(current *ir.AggType, base int) bool {
		offset := 0
		for _, field := range current.Fields {
			size, alignment := goFieldSizeAlign(field)
			offset = roundUp(offset, alignment)
			count := field.Count
			if count <= 0 {
				count = 1
			}
			if count > 1 {
				// ABIInternal never register-assigns a non-trivial array, even
				// when all of its elements would otherwise fit.
				return false
			}
			if field.Type != nil {
				if !walk(field.Type, base+offset) {
					return false
				}
			} else {
				parts = append(parts, goABIPart{
					sub:     field.Sub,
					offset:  base + offset,
					pointer: field.Pointer,
				})
			}
			offset += size
		}
		return true
	}

	if !walk(aggregate, 0) {
		return nil, false
	}
	return parts, true
}

func goFieldSizeAlign(field ir.Field) (int, int) {
	if field.Type != nil {
		return field.Type.Layout()
	}
	size := field.Sub.Size()
	return size, size
}

// assignGoAggregate assigns one complete Go value. If any of its parts cannot
// fit, the register indexes are rolled back and the whole value is placed on
// the stack, as required by ABIInternal.
func assignGoAggregate(assigner *argAssigner, aggregate *ir.AggType) (parts []goABIPart, onStack bool, stackOffset int) {
	parts, flattenable := flattenGoAggregate(aggregate)
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

func aggregateAlloc(f *ir.Func, aggregate *ir.AggType, out *[]ir.Instr) ir.Ref {
	size, alignment := aggregate.Layout()
	if size == 0 {
		size = 1
	}
	var operation ir.Op
	switch {
	case alignment > 8:
		operation = ir.OAlloc16
	case alignment > 4:
		operation = ir.OAlloc8
	default:
		operation = ir.OAlloc4
	}
	slot := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: operation, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(size))}})
	markAggregatePointerWords(f, slot, aggregate)
	return slot
}

func markAggregatePointerWords(f *ir.Func, slot ir.Ref, aggregate *ir.AggType) {
	parts, ok := flattenGoAggregate(aggregate)
	if !ok {
		return
	}
	for _, part := range parts {
		if !part.pointer || part.offset%8 != 0 {
			continue
		}
		if f.StackPointerWords[slot.ID] == nil {
			f.StackPointerWords[slot.ID] = make(map[int]bool)
		}
		f.StackPointerWords[slot.ID][part.offset] = true
	}
}

func loadOpForSub(sub ir.SubCls) ir.Op {
	switch sub {
	case ir.SubB:
		return ir.OLoadsb
	case ir.SubUB:
		return ir.OLoadub
	case ir.SubH:
		return ir.OLoadsh
	case ir.SubUH:
		return ir.OLoaduh
	case ir.SubW:
		return ir.OLoadsw
	case ir.SubL:
		return ir.OLoadl
	case ir.SubS:
		return ir.OLoads
	case ir.SubD:
		return ir.OLoadd
	case ir.SubQ:
		return ir.OLoadq
	default:
		panic("arm64: unsupported Go ABI aggregate field")
	}
}

func storeOpForSub(sub ir.SubCls) ir.Op {
	switch sub {
	case ir.SubB, ir.SubUB:
		return ir.OStoreb
	case ir.SubH, ir.SubUH:
		return ir.OStoreh
	case ir.SubW:
		return ir.OStorew
	case ir.SubL:
		return ir.OStorel
	case ir.SubS:
		return ir.OStores
	case ir.SubD:
		return ir.OStored
	case ir.SubQ:
		return ir.OStoreq
	default:
		panic("arm64: unsupported Go ABI aggregate field")
	}
}

func lowerGoAggregateParam(f *ir.Func, parameter *ir.Temp, assigner *argAssigner) (parameters, reconstruction []ir.Instr, err error) {
	parts, onStack, stackOffset := assignGoAggregate(assigner, parameter.Agg)
	if onStack {
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: parameter.Ref(), Aux: int64(stackOffset)}}, nil, nil
	}

	slot := aggregateAlloc(f, parameter.Agg, &reconstruction)
	for _, part := range parts {
		pin := newPinned(f, part.reg, part.sub.Cls())
		if part.pointer {
			f.Temp(pin).GCRef = true
		}
		address := offsetAddr(f, slot, part.offset, &reconstruction)
		reconstruction = append(reconstruction, ir.Instr{
			Op:   storeOpForSub(part.sub),
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
		parts, _ = flattenGoAggregate(aggregate)
	}
	if len(parts) != len(parameters) {
		return nil, fmt.Errorf("arm64: aggregate parameter has %d SSA parts, want %d", len(parameters), len(parts))
	}

	lowered := make([]ir.Instr, 0, len(parts))
	for index, part := range parts {
		parameter := parameters[index]
		if parameter.Cls != part.sub.Cls() {
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
			Op:   loadOpForSub(part.sub),
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
		parts, _ = flattenGoAggregate(aggregate)
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
		slot := aggregateAlloc(f, aggregate, out)
		post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: destination, Args: []ir.Ref{slot}})
		return ir.R, 0, nil, setup, pins, post, slot, resultOffset, resultAssigner.nsaa, nil
	}

	slot := aggregateAlloc(f, aggregate, out)
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
			Op:   storeOpForSub(part.sub),
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
		parts, _ = flattenGoAggregate(aggregate)
	}
	if len(parts) != len(destinations) {
		err = fmt.Errorf("arm64: aggregate result has %d SSA parts, want %d", len(destinations), len(parts))
		return
	}

	if onStack {
		slot := aggregateAlloc(f, aggregate, out)
		for index, part := range parts {
			destination := destinations[index]
			if f.ClassOf(destination) != part.sub.Cls() {
				err = fmt.Errorf("arm64: aggregate result part %d has class %s, want %s", index, f.ClassOf(destination), part.sub.Cls())
				return
			}
			address := offsetAddr(f, slot, part.offset, &post)
			post = append(post, ir.Instr{
				Op:   loadOpForSub(part.sub),
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
		address := offsetAddr(f, pointer, part.offset, &block.Instrs)
		value := f.NewTemp("", part.sub.Cls())
		block.Instrs = append(block.Instrs, ir.Instr{
			Op:   loadOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			To:   value,
			Args: []ir.Ref{address},
		})
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
	if len(resultRegisters) == 0 {
		return fmt.Errorf("arm64: zero-sized Go ABI aggregate results are not yet supported")
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
		parts, _ = flattenGoAggregate(f.RetAgg)
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
				Op:   storeOpForSub(part.sub),
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
		if group, ok := valueGroupAt(call.ArgGroups, argumentIndex); ok {
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
