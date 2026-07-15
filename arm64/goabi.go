package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

const goStackLinkSize = 8

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

type goABISpill struct {
	size      int
	alignment int
}

func goCallStackBytes(f *ir.Func, call *ir.Instr, resultEnd int) int {
	assigner := newArgAssigner(true)
	var spills []goABISpill
	for argumentIndex, argument := range call.Args[1:] {
		aggregate := aggArgAt(call, argumentIndex)
		if aggregate != nil {
			_, onStack, _ := assignGoAggregate(&assigner, aggregate)
			if !onStack {
				size, alignment := aggregate.Layout()
				spills = append(spills, goABISpill{size: size, alignment: alignment})
			}
			continue
		}

		class := f.ClassOf(argument)
		location := assigner.assign(class)
		if !location.onStack {
			spills = append(spills, goABISpill{size: class.Size(), alignment: class.Size()})
		}
	}

	cursor := roundUp(resultEnd, 8)
	for _, spill := range spills {
		cursor = roundUp(cursor, spill.alignment)
		cursor += spill.size
	}
	cursor = roundUp(cursor, 8)
	if cursor == 0 {
		return 0
	}
	return roundUp(goStackLinkSize+cursor, 16)
}
