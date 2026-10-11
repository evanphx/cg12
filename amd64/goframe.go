package amd64

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// This file is the managed-frame half of the emitter: the stack-growth
// prologue a Go function opens with, and the facts the Go runtime reads back
// out of the frame -- its size, where its pointers are at each safepoint, and
// where its deferreturn call is.
//
// The frame is the one every amd64 function already has, and it happens to be
// exactly the shape Go's x86 runtime expects:
//
//	[RBP+16 ...]   the caller's outgoing area: this function's argument frame
//	[RBP+8]        return address
//	[RBP]          saved RBP                                  <- frame.varp
//	[RBP-8 ...]    spills, then allocations                   (locals)
//	[RSP ...]      the outgoing area for this function's calls
//
// runtime.stkframe derives fp = SP + spdelta + 8, varp = fp - 16 = RBP and
// argp = fp = RBP + 16 from the PCSP table alone, so none of arm64's private
// frame-record PCDATA is needed: its runtime hooks (cg12AAPCSOutgoingSize) read
// -1 for a function that does not carry the table, which is the x86 path.

// goMetadataArch is amd64's instantiation of the shared Go metadata emitter.
// PC deltas count bytes, absolute data references use the x86-64 ABS
// relocations, and the saved RBP is the one frame word above the first local.
var goMetadataArch = gometa.Arch{
	PCQuantum:      1,
	Reloc32:        obj.R_X86_64_32,
	Reloc64:        obj.R_X86_64_64,
	FrameBaseBytes: 8,
}

type goFunctionInfo = gometa.FunctionInfo
type goStackMapPoint = gometa.StackMapPoint

const (
	goFuncFlagTopFrame = gometa.FuncFlagTopFrame
	goFuncFlagSPWrite  = gometa.FuncFlagSPWrite
	goFuncFlagAsm      = gometa.FuncFlagAsm
)

// g's stack-guard words, at their offsets in runtime.g (stack.lo, stack.hi,
// stackguard0, stackguard1).
const (
	goStackGuard0Offset = 16
	goStackGuard1Offset = 24
)

// goStackPrologue emits a managed function's stack-growth check. It runs before
// the frame exists, so SP is still the caller's and [RSP] is the return
// address -- the state runtime.morestack reads f's PC and SP out of.
//
// The check is against the SP the function is about to have, so every frame
// size is covered by one comparison: R12 = RSP - (frame + 8), the 8 being the
// saved RBP. stackguard0 doubles as the preemption request (stackPreempt is
// larger than any stack address), which takes the slow path as Go's own
// prologue does.
//
// The slow path spills the register arguments to their home slots in the
// caller's outgoing area, calls morestack, reloads them and checks again. A
// closure keeps its context in RDX across the call: runtime.morestack saves DX
// in g.sched.ctxt and gogo restores it, so a closure calls morestack itself
// rather than morestack_noctxt, which zeroes DX.
func (m *mc) goStackPrologue() {
	const retry = "__cg12_go_prologue"
	const enough = "__cg12_go_stack_ok"

	guardOffset := int32(goStackGuard0Offset)
	morestack := "runtime_morestack_noctxt"
	if m.f.SystemStack {
		guardOffset = goStackGuard1Offset
		morestack = "runtime_morestackc"
	} else if m.f.HasClosureContext {
		morestack = "runtime_morestack"
	}

	scratch := m.gs0()
	m.prog.Label(retry)
	m.emit(x64.Lea(true, scratch.mreg(), x64.At(RSP.mreg(), int32(-(m.frame+8)))))
	m.emit(x64.CmpMem(true, scratch.mreg(), x64.At(regG.mreg(), guardOffset)))
	m.prog.Jcc(x64.A, enough)
	spills := goArgumentFrameFor(m.f).spills
	for _, spill := range spills {
		m.emitGoRegisterSpill(spill, false)
	}
	m.emit(x64.CallRel(0))
	m.recordReloc(m.prog.Len()-4, morestack, obj.R_X86_64_PLT32, -4)
	for _, spill := range spills {
		m.emitGoRegisterSpill(spill, true)
	}
	m.prog.Jmp(retry)
	m.prog.Label(enough)
}

// emitGoRegisterSpill stores (or, with load, reloads) one register argument to
// its home slot, addressed from the entry RSP.
func (m *mc) emitGoRegisterSpill(spill goRegisterSpill, load bool) {
	at := x64.At(RSP.mreg(), int32(spill.offset))
	reg := spill.reg.mreg()
	switch {
	case spill.float && spill.size == 8 && load:
		m.emit(x64.MovsdLoad(reg, at))
	case spill.float && spill.size == 8:
		m.emit(x64.MovsdStore(reg, at))
	case spill.float && load:
		m.emit(x64.MovssLoad(reg, at))
	case spill.float:
		m.emit(x64.MovssStore(reg, at))
	case load && spill.size == 8:
		m.emit(x64.Load(true, reg, at))
	case load && spill.size == 4:
		m.emit(x64.Load(false, reg, at))
	case load && spill.size == 2:
		m.emit(x64.MovzxLoadWord(false, reg, at))
	case load:
		m.emit(x64.MovzxLoadByte(false, reg, at))
	default:
		m.emit(x64.Store(spill.size*8, reg, at))
	}
}

// zeroGoPointerSlots clears every frame word a stack map can describe as a
// pointer, so no map ever hands the collector a word this activation has not
// written -- the previous occupant of the stack's bytes.
func (m *mc) zeroGoPointerSlots() {
	if !m.f.UsesManagedFrame() {
		return
	}
	offsets := m.goPointerFrameOffsets()
	if len(offsets) == 0 {
		return
	}
	zero := m.gs0()
	m.emit(x64.XorReg(false, zero.mreg(), zero.mreg()))
	for _, distance := range offsets {
		m.emit(x64.Store(64, zero.mreg(), x64.At(RBP.mreg(), int32(-distance))))
	}
}

// goPointerFrameOffsets returns, as distances below RBP, every word-aligned
// frame word that may hold a pointer: the pointer words of each stack
// allocation, and the spill slot of each pointer temporary.
func (m *mc) goPointerFrameOffsets() []int {
	seen := make(map[int]bool)
	for instruction, distance := range m.allocOff {
		for wordOffset := range m.f.StackPointerWords[instruction.To.ID] {
			below := distance - wordOffset
			if below > 0 && below%8 == 0 {
				seen[below] = true
			}
		}
	}
	for _, temporary := range m.f.Temps {
		if temporary == nil {
			continue
		}
		pointer := temporary.GCRef || temporary.Cls == ir.ClsP
		if pointer && temporary.Reg == ir.NoReg && temporary.Slot >= 0 {
			if _, remat := m.alloc.remat[int(temporary.ID)]; remat {
				continue
			}
			seen[m.spillBase+8+temporary.Slot] = true
		}
	}
	offsets := make([]int, 0, len(seen))
	for offset := range seen {
		offsets = append(offsets, offset)
	}
	sort.Ints(offsets)
	return offsets
}

// goLocalWords is the number of words the locals stack map covers: everything
// between RBP and the outgoing area.
func (m *mc) goLocalWords() int {
	return (m.frame - m.outgoing) / 8
}

// goLocalWordIndex converts a distance below RBP to the locals bitmap index the
// runtime reads it at: the bitmap covers [varp - n*8, varp), bit i at
// varp - n*8 + i*8.
func (m *mc) goLocalWordIndex(distance int) (int, bool) {
	if distance <= 0 || distance%8 != 0 {
		return 0, false
	}
	index := m.goLocalWords() - distance/8
	if index < 0 {
		return 0, false
	}
	return index, true
}

func (m *mc) goLocalPointerWords() []int {
	var words []int
	for _, distance := range m.goPointerFrameOffsets() {
		if index, ok := m.goLocalWordIndex(distance); ok {
			words = append(words, index)
		}
	}
	sort.Ints(words)
	return words
}

func (m *mc) goStackMapPoints() []goStackMapPoint {
	points := make([]goStackMapPoint, 0, len(m.safepoints))
	for _, safepoint := range m.safepoints {
		seen := make(map[int]bool)
		for _, root := range safepoint.roots {
			if root.kind != rootFrame {
				continue
			}
			if index, ok := m.goLocalWordIndex(int(-root.val)); ok {
				seen[index] = true
			}
		}
		pointerWords := make([]int, 0, len(seen))
		for word := range seen {
			pointerWords = append(pointerWords, word)
		}
		sort.Ints(pointerWords)
		points = append(points, goStackMapPoint{
			PC:           int(safepoint.startPC),
			PointerWords: pointerWords,
		})
	}
	return points
}

// recordGoSafepoint captures a managed frame's roots at a call: the pointer
// words of every allocation still addressed, and the slot of every pointer
// temporary live across the call. Everything live across a call is in a slot
// under the Go profile, so there are no register roots to report.
func (m *mc) recordGoSafepoint(in *ir.Instr, startPC uint64) {
	roots := m.alloc.safeRoots[in]
	undefined := m.alloc.undefinedAllocs[in]
	locs := make([]rootLoc, 0, len(roots))
	for _, id := range roots {
		t := m.f.Temps[id]
		distance, isAllocation := m.stackAllocDistance[uint32(id)]
		if isAllocation && !undefined[id] {
			for wordOffset := range m.f.StackPointerWords[uint32(id)] {
				locs = append(locs, rootLoc{kind: rootFrame, val: int32(-(distance - wordOffset))})
			}
		}
		if t.Reg != ir.NoReg {
			// An allocation is reported for its pointer words wherever an address
			// into it may still be used -- including when the address escaped and
			// the temporary that held it is long dead. Its register is then
			// nothing the runtime needs.
			if isAllocation {
				continue
			}
			m.fail(fmt.Errorf("amd64: %s: root %%%s is in a register across a managed call", m.f.Name, t.Name))
			continue
		}
		if _, remat := m.alloc.remat[id]; remat {
			// Recomputed from RBP at each use: no stack word holds it.
			continue
		}
		if t.Slot < 0 {
			continue
		}
		locs = append(locs, rootLoc{kind: rootFrame, val: m.slotAddr(t.Slot), typ: t.GCType})
	}
	m.safepoints = append(m.safepoints, safepoint{pc: uint64(m.prog.Len()), startPC: startPC, roots: locs})
}

// deferReturnOffset finds the runtime.deferreturn call a function with defers
// resumes at after a recovered panic.
func (m *mc) deferReturnOffset() (uint32, error) {
	registersDefer := false
	var deferReturn *ir.Instr
	for _, block := range m.f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			symbol, ok := directCallSymbol(m.f, instruction)
			if !ok {
				continue
			}
			switch symbol {
			case "runtime_deferproc", "runtime_deferprocStack", "runtime_deferprocat":
				registersDefer = true
			case "runtime_deferreturn":
				if deferReturn == nil {
					deferReturn = instruction
				}
			}
		}
	}
	if deferReturn != nil {
		pcs, emitted := m.instrPC[deferReturn]
		if !emitted || pcs[0] == 0 {
			return 0, fmt.Errorf("runtime.deferreturn has no valid emitted call-site PC")
		}
		return uint32(pcs[0]), nil
	}
	if registersDefer {
		return 0, fmt.Errorf("runtime defer registration has no defer-return continuation")
	}
	return 0, nil
}

func directCallSymbol(function *ir.Func, instruction *ir.Instr) (string, bool) {
	if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
		return "", false
	}
	callee := instruction.Args[0]
	if callee.Kind != ir.RefConst {
		return "", false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return "", false
	}
	return sanitize(constant.Sym), true
}

// goFunctionInfoFor gathers the Go runtime metadata for one emitted function.
// FrameSize is the PCSP value once the frame is built: the saved RBP plus the
// bytes subtracted from RSP.
func goFunctionInfoFor(f *ir.Func, name string, code *machineCode) (goFunctionInfo, error) {
	m := code.m
	deferReturn, err := m.deferReturnOffset()
	if err != nil {
		return goFunctionInfo{}, err
	}
	argumentFrame := goArgumentFrame{}
	if f.UsesManagedFrame() {
		argumentFrame = goArgumentFrameFor(f)
	}
	return goFunctionInfo{
		Name:                          name,
		FrameSize:                     m.frame + 8,
		FrameStart:                    m.frameStart,
		ManagedFrame:                  f.UsesManagedFrame(),
		OutgoingSize:                  m.outgoing,
		Size:                          uint64(len(code.code)),
		FuncID:                        gometa.RuntimeFunctionID(name),
		DeferReturn:                   deferReturn,
		LocalPointerWords:             m.goLocalPointerWords(),
		StackMapPoints:                m.goStackMapPoints(),
		ArgumentSize:                  argumentFrame.size,
		ArgumentPointerWords:          argumentFrame.pointerWords,
		SafepointArgumentPointerWords: argumentFrame.incomingPointerWords,
	}, nil
}

// emitStackAggregateArgument copies a stack-assigned aggregate argument into
// the outgoing area at its offset. The aggregate's own bytes are copied exactly
// -- no wider -- because neighbouring stack arguments are packed against it.
func (m *mc) emitStackAggregateArgument(argument *ir.Instr) {
	size, _ := argument.RetAgg().Layout()
	source := m.gpValue(argument.Args[0], m.gs1())
	m.emitByteCopy(x64.At(source.mreg(), 0), x64.At(RSP.mreg(), int32(argument.Aux)), size)
}

// emitStackAggregateResult copies a stack-assigned ABIInternal result out of
// the outgoing area, where the callee wrote it, into the home the lowering gave
// it. It runs immediately after the call returns, before anything can reuse the
// outgoing area.
func (m *mc) emitStackAggregateResult(call *ir.Instr) {
	destination, offset := call.StackResult(), call.StackResultOffset()
	if destination.IsNone() || call.RetAgg() == nil {
		return
	}
	size, _ := call.RetAgg().Layout()
	target := m.gpValue(destination, m.gs1())
	m.emitByteCopy(x64.At(RSP.mreg(), int32(offset)), x64.At(target.mreg(), 0), size)
}

// emitByteCopy copies size bytes from source to destination through the
// first general-purpose scratch register, widest pieces first. Neither operand
// may be based on that register.
func (m *mc) emitByteCopy(source, destination x64.Mem, size int) {
	scratch := m.gs0().mreg()
	copied := int32(0)
	for _, width := range []int{8, 4, 2, 1} {
		for int(copied)+width <= size {
			from := source
			from.Disp += copied
			to := destination
			to.Disp += copied
			switch width {
			case 8:
				m.emit(x64.Load(true, scratch, from))
			case 4:
				m.emit(x64.Load(false, scratch, from))
			case 2:
				m.emit(x64.MovzxLoadWord(false, scratch, from))
			default:
				m.emit(x64.MovzxLoadByte(false, scratch, from))
			}
			m.emit(x64.Store(width*8, scratch, to))
			copied += int32(width)
		}
	}
}
