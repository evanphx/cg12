package amd64

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/ir"
)

// This file is the pure computational core of the amd64 Go argument frame: given
// a function or a call site, it says *where* every value goes -- which register,
// which byte offset, which home slot, which pointer word. It rewrites no IR and
// emits no instruction. Lowering and the morestack prologue are built on top of
// it (B1's lowering half and B2 respectively); everything here is a pure
// function over ir types so it can be tested without a machine.
//
// It is the amd64 counterpart of arm64/goabi.go and deliberately keeps that
// file's vocabulary -- goRegisterSpill, goRegisterSpillGroup, goArgumentFrame,
// goArgumentFrameFor, assignGoAggregate, goCallStackBytes -- so the two can be
// read against each other. Four things are genuinely different, and each one is
// a place a naive port would be silently wrong:
//
//  1. There is no hand-reserved frame-chain link. arm64's Go ABI reserves eight
//     bytes at the bottom of the argument frame for the saved link register
//     (goStackLinkSize) and adds it to every spill offset. On amd64 the CALL
//     instruction pushes the return address itself, so
//     conventionABI(...).stackLinkBytes is zero under both conventions and no
//     offset here carries a link adjustment. Adding one would double-count eight
//     bytes in every frame.
//
//  2. ClsQ is refused, never truncated. Every amd64 spill slot is eight bytes
//     (frameLayout.slotAddr's stride, gcalloc.go, callersave.go), so a 16-byte
//     home slot would overrun its neighbour. arm64 sizes slots per class and can
//     afford to carry quads; amd64 cannot, so a quad anywhere in a Go argument
//     frame is a named error.
//
//  3. The float argument registers stop at X12 (goArgFP), where Go's own
//     FloatArgRegs is 15. A signature that wants more is refused by name rather
//     than spilled to the stack -- see goCheckFloatArgumentRegisters.
//
//  4. The base/sign convention is inverted from arm64. See below.
//
// # Base and sign convention (B1 -> B2/C2/C3 contract)
//
// Every offset this file produces -- goRegisterSpill.offset, the stack offsets
// inside goArgumentFrame, and the byte offsets behind pointerWords /
// incomingPointerWords -- is a *non-negative byte offset from the base of the
// argument frame*, growing upward toward higher addresses. Word index is
// offset/PointerWordBytes, with no bias term: word 0 is the first eight bytes of
// the argument frame.
//
// The argument frame base is where the caller's outgoing argument area starts,
// which on amd64 is:
//
//	at function entry, before any prologue:   RSP + goArgumentFrameFromEntrySP  (RSP+8, past the pushed return address)
//	after `push %rbp; mov %rsp,%rbp`:         RBP + goArgumentFrameFromFramePointer  (RBP+16)
//	in the caller, at the CALL:               RSP + 0  (computeFrame puts the outgoing area at the bottom of the frame)
//
// B2 adds exactly one of those biases when it turns a spill offset into an
// addressing mode; this file adds none. That is the whole of point (1) above:
// arm64's goRegisterSpill.offset is SP-relative and therefore link-biased while
// its pointer-word offsets are not, so the two origins differ there. Here they
// are the same origin, so a spill offset and a pointer-word offset for the same
// slot are the same number.
//
// This is the *positive* direction. The local frame is the opposite: locals and
// spill slots live at negative offsets from RBP (frameLayout.slotAddr returns
// -(spillBase+8+slot), and mc.go's rootFrame values are negative), whereas
// arm64's local roots are positive offsets from x29 with the frame record at
// [0,16) decoded as (val-16)/8. Any word-index conversion for a *local* root on
// amd64 therefore flips sign relative to arm64 and has no -16 bias to remove.
// That conversion belongs to C2/C3 and is not implemented here; it is stated so
// the two halves cannot drift.

// goArgumentFrameFromEntrySP and goArgumentFrameFromFramePointer convert an
// offset produced by this file into an address, at the two points a prologue
// cares about. They exist so no consumer re-derives "8" or "16" and so the
// choice of origin is visible in one place.
//
// The CALL that reached this function pushed an eight-byte return address below
// the caller's outgoing argument area, so at entry the argument frame starts at
// RSP+8. The standard prologue then pushes RBP, putting the return address at
// RBP+8 and the argument frame at RBP+16.
const (
	goArgumentFrameFromEntrySP      = 8
	goArgumentFrameFromFramePointer = 16
)

// goABIPart is one flattened field of an aggregate together with the register
// the Go ABI assigned it. Go's ABIInternal decomposes an aggregate *by field*,
// giving each scalar field its own register; this is not System V's eightbyte
// rule, and classifyAgg/assignAgg (amd64/abi.go) must not be used for the Go
// path.
type goABIPart struct {
	sub     ir.SubCls
	offset  int // byte offset within the aggregate
	pointer bool
	reg     Reg
}

// flattenGoAggregate is ir.FlattenAggregate with an amd64 register slot added to
// each part. The flattening is architecture-neutral and lives in ir; only the
// assignment of a part to a register is architectural, so only that lives here.
func flattenGoAggregate(aggregate *ir.AggType) ([]goABIPart, bool) {
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

// goSubClassForCls is the memory width one scalar of a register class occupies
// in its home slot.
func goSubClassForCls(class ir.Cls) ir.SubCls {
	switch class {
	case ir.ClsS:
		return ir.SubS
	case ir.ClsD:
		return ir.SubD
	case ir.ClsQ:
		return ir.SubQ
	case ir.ClsW:
		return ir.SubW
	default:
		return ir.SubL
	}
}

// ---------------------------------------------------------------------------
// Loud refusals
// ---------------------------------------------------------------------------

// goQuadRefusal reports a 128-bit value in a Go argument frame.
//
// Every amd64 spill slot is eight bytes wide -- frameLayout.slotAddr strides by
// 8, and gcalloc.go and callersave.go both assume it -- so a register argument's
// 16-byte home slot would run into its neighbour's. The plan's standing decision
// is that 128-bit values stay register-resident until B3/B4 lift the fixed slot
// size, so the argument frame refuses them by name rather than truncating to
// eight bytes, which would silently drop the high half of the value.
//
// The refusal is deliberately independent of register pressure: a quad is
// rejected whether or not it would have fit in registers, so whether a signature
// compiles does not depend on how many arguments precede it. Aggregates that do
// not flatten are exempt, because they travel as opaque memory and never occupy
// a register or a home slot; only a value or flattened field that would land in
// a register is a hazard.
func goQuadRefusal(what string) error {
	return fmt.Errorf("amd64: %s has 128-bit class q, which the Go argument frame cannot carry: "+
		"every amd64 spill slot is 8 bytes (frameLayout.slotAddr), so a 16-byte register home slot "+
		"would overrun its neighbour; 128-bit values must stay register-resident", what)
}

// goFloatArgRegsUpstream is Go's own float argument register sequence,
// FloatArgRegs = 15 (stdlib/src/internal/abi/abi_amd64.go). It is here only to
// answer "would Go's compiler have used more float registers than cg12's table
// has?" -- it is never used to assign a register, because X13/X14 are the
// emitter's float scratch pair and X15 is the Go zero register. goArgFP, capped
// at X12, is the table assignment actually uses.
var goFloatArgRegsUpstream = []Reg{
	XMM(0), XMM(1), XMM(2), XMM(3), XMM(4), XMM(5), XMM(6), XMM(7),
	XMM(8), XMM(9), XMM(10), XMM(11), XMM(12), XMM(13), XMM(14),
}

// goFloatRegisterDemand returns how many float argument registers Go's own
// compiler would consume for this sequence of values.
//
// It replays the identical assignment walk against the upstream 15-register
// table. That is exact rather than approximate: the integer table is faithful
// (nine registers, matching IntArgRegs), so the only way cg12 and Go can place a
// value differently is the float bank running out at 13 where Go still had room,
// and that is precisely the case where this walk ends above len(goArgFP).
func goFloatRegisterDemand(values []goValue) int {
	assigner := newArgAssigner(true)
	assigner.floatRegs = goFloatArgRegsUpstream
	for _, value := range values {
		assignGoValue(&assigner, value)
	}
	return assigner.nsrn
}

// goCheckFloatArgumentRegisters refuses a signature whose float register demand
// exceeds cg12's capped table.
//
// Passing the excess on the stack instead would be self-consistent between two
// cg12-compiled functions and wrong against the runtime: abi.RegArgs is sized by
// Go's FloatArgRegs = 15, so a reflect-mediated call would read the argument out
// of a register cg12 never wrote. Refusing by name keeps that a compile error
// rather than a memory-corruption bug in someone else's package.
func goCheckFloatArgumentRegisters(what string, values []goValue) error {
	demand := goFloatRegisterDemand(values)
	if !goFloatArgRegSpill(demand) {
		return nil
	}
	return fmt.Errorf("amd64: %s needs %d float argument registers, but cg12 caps Go ABIInternal at %d (X0..X12): "+
		"X13/X14 are the emitter's float scratch pair and X15 is Go's zero register, so there is no faithful "+
		"14th float argument register; passing the excess on the stack would disagree with the runtime's "+
		"abi.RegArgs, which is sized by Go's FloatArgRegs = %d",
		what, demand, len(goArgFP), len(goFloatArgRegsUpstream))
}

// ---------------------------------------------------------------------------
// ABI-level values
// ---------------------------------------------------------------------------

// goValue is one ABI-level value in an argument frame: a whole aggregate, or a
// scalar. It is the unit Go's ABIInternal assigns atomically -- an aggregate
// either occupies registers entirely or goes on the stack entirely -- so the
// assignment walk is written over these rather than over ir.Params directly,
// which also lets the float-cap guard replay the same walk against a different
// register table.
type goValue struct {
	name    string      // for diagnostics only
	agg     *ir.AggType // non-nil for an aggregate value
	cls     ir.Cls      // register class, for a scalar
	pointer bool        // scalar only; an aggregate's pointers come from its type
}

// layout returns the value's size and alignment in the argument frame.
func (v goValue) layout() (size, alignment int) {
	if v.agg != nil {
		return v.agg.Layout()
	}
	return v.cls.Size(), v.cls.Size()
}

// goScalarValue builds a scalar value, refusing a quad.
func goScalarValue(name string, class ir.Cls, pointer bool) (goValue, error) {
	if class == ir.ClsQ {
		return goValue{}, goQuadRefusal(fmt.Sprintf("scalar %s", name))
	}
	return goValue{name: name, cls: class, pointer: pointer}, nil
}

// goAggregateValue builds an aggregate value, refusing one with a quad field.
//
// An aggregate that does not flatten (a union, an opaque type, or a struct with
// a non-trivial inline array) is accepted: ABIInternal never register-assigns
// one, so it travels as memory and no quad inside it can reach a home slot.
func goAggregateValue(name string, aggregate *ir.AggType) (goValue, error) {
	if parts, flattenable := flattenGoAggregate(aggregate); flattenable {
		for index, part := range parts {
			if part.sub == ir.SubQ {
				return goValue{}, goQuadRefusal(fmt.Sprintf("aggregate %s field %d (at byte %d)", name, index, part.offset))
			}
		}
	}
	return goValue{name: name, agg: aggregate}, nil
}

// goInternalParamValues describes f's parameters as ABI-level values.
func goInternalParamValues(f *ir.Func) ([]goValue, error) {
	values := make([]goValue, 0, len(f.Params))
	for index := 0; index < len(f.Params); {
		if group, ok := ir.ValueGroupAt(f.ParamGroups, index); ok {
			value, err := goAggregateValue(fmt.Sprintf("parameter group at %d", index), group.Type)
			if err != nil {
				return nil, fmt.Errorf("amd64: function %q: %w", f.Name, err)
			}
			values = append(values, value)
			index += max(group.Count, 1)
			continue
		}
		parameter := f.Params[index]
		if parameter.Agg != nil {
			value, err := goAggregateValue(fmt.Sprintf("parameter %q", parameter.Name), parameter.Agg)
			if err != nil {
				return nil, fmt.Errorf("amd64: function %q: %w", f.Name, err)
			}
			values = append(values, value)
			index++
			continue
		}
		value, err := goScalarValue(
			fmt.Sprintf("parameter %q", parameter.Name),
			parameter.Cls,
			parameter.GCRef || parameter.Cls.IsManaged(),
		)
		if err != nil {
			return nil, fmt.Errorf("amd64: function %q: %w", f.Name, err)
		}
		values = append(values, value)
		index++
	}
	return values, nil
}

// goInternalArgValues describes one call's arguments as ABI-level values.
func goInternalArgValues(f *ir.Func, call *ir.Instr) ([]goValue, error) {
	arguments := call.Args[1:]
	values := make([]goValue, 0, len(arguments))
	for index := 0; index < len(arguments); {
		if group, ok := ir.ValueGroupAt(call.ArgGroups, index); ok {
			value, err := goAggregateValue(fmt.Sprintf("argument group at %d", index), group.Type)
			if err != nil {
				return nil, fmt.Errorf("amd64: function %q: %w", f.Name, err)
			}
			values = append(values, value)
			index += max(group.Count, 1)
			continue
		}
		if aggregate := aggArgAt(call, index); aggregate != nil {
			value, err := goAggregateValue(fmt.Sprintf("argument %d", index), aggregate)
			if err != nil {
				return nil, fmt.Errorf("amd64: function %q: %w", f.Name, err)
			}
			values = append(values, value)
			index++
			continue
		}
		argument := arguments[index]
		class := f.ClassOf(argument)
		value, err := goScalarValue(fmt.Sprintf("argument %d", index), class, refIsManagedPointer(f, argument))
		if err != nil {
			return nil, fmt.Errorf("amd64: function %q: %w", f.Name, err)
		}
		values = append(values, value)
		index++
	}
	return values, nil
}

// refIsManagedPointer reports whether a reference is a garbage-collector root.
func refIsManagedPointer(f *ir.Func, ref ir.Ref) bool {
	if f.ClassOf(ref).IsManaged() {
		return true
	}
	return ref.Kind == ir.RefTemp && int(ref.ID) < len(f.Temps) && f.Temps[ref.ID].GCRef
}

// ---------------------------------------------------------------------------
// Assignment
// ---------------------------------------------------------------------------

// assignGoAggregate assigns one complete aggregate under Go's ABIInternal: each
// flattened field takes its own integer or float argument register, and if any
// field cannot fit, the whole value goes on the stack at its natural alignment.
// ABIInternal does not exhaust the bank on failure, so a later, smaller value
// can still take the registers this one could not.
//
// This is field decomposition, not System V's eightbyte classification. A System
// V aggregate is classified by classifyAgg and assigned by argAssigner.assignAgg
// (amd64/abi.go); using this function for one would produce a plausible-looking
// but wrong assignment, so it asserts its assigner is a Go one.
func assignGoAggregate(assigner *argAssigner, aggregate *ir.AggType) (parts []goABIPart, onStack bool, stackOffset int) {
	if !assigner.goABI {
		panic("amd64: assignGoAggregate called with a System V assigner; System V classifies aggregates " +
			"by eightbyte (classifyAgg/assignAgg), not by field")
	}
	parts, flattenable := flattenGoAggregate(aggregate)
	size, alignment := aggregate.Layout()
	if size == 0 || !flattenable {
		return nil, true, assigner.assignStack(size, alignment)
	}

	integerCount, floatCount := 0, 0
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

// assignGoValue assigns one ABI-level value, aggregate or scalar. A
// register-assigned scalar comes back as a single part so callers have one shape
// to handle.
func assignGoValue(assigner *argAssigner, value goValue) (parts []goABIPart, onStack bool, stackOffset int) {
	if value.agg != nil {
		return assignGoAggregate(assigner, value.agg)
	}
	location := assigner.assign(value.cls)
	if location.onStack {
		return nil, true, location.stacky
	}
	return []goABIPart{{
		sub:     goSubClassForCls(value.cls),
		pointer: value.pointer,
		reg:     location.reg,
	}}, false, 0
}

// ---------------------------------------------------------------------------
// The argument frame
// ---------------------------------------------------------------------------

// goRegisterSpill is one register argument's home slot: the register to store,
// where in the argument frame it goes, how wide the store is, which bank it is
// in, and whether the collector must scan the slot.
//
// offset is a non-negative byte offset from the argument frame base; see the
// base and sign convention at the top of this file. It carries no bias for the
// return address -- B2 adds goArgumentFrameFromEntrySP or
// goArgumentFrameFromFramePointer when it forms the addressing mode.
type goRegisterSpill struct {
	reg     Reg
	offset  int
	size    int
	float   bool
	pointer bool
}

// goRegisterSpillGroup is one value's home slots before offsets are assigned.
// An aggregate carries parts (one per field, each with its own register and
// offset within the group) and the group's size/alignment; a scalar carries a
// single register and leaves parts empty.
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
//
// size is rounded to a pointer word, not to 16: it is the callee-side argument
// frame size the Go runtime records, and the caller's 16-byte stack alignment is
// goCallStackBytes's business.
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
func goRegisterSpills(function *ir.Func) ([]goRegisterSpill, error) {
	frame, err := goArgumentFrameFor(function)
	if err != nil {
		return nil, err
	}
	return frame.spills, nil
}

// goArgumentFrameFor computes the whole argument frame for a function.
//
// It dispatches on the function's argument convention, not on its frame: goc
// emits managed-frame *platform-ABI* helpers, and those still need register home
// slots so morestack can copy their arguments, but their arguments are placed by
// System V's rules. Getting this wrong in either direction gives the prologue
// home slots at addresses the caller never reserved.
func goArgumentFrameFor(function *ir.Func) (goArgumentFrame, error) {
	if function.UsesGoInternalCallConvention() {
		return goInternalArgumentFrame(function)
	}
	return platformArgumentFrame(function)
}

// goInternalArgumentFrame is goArgumentFrameFor's Go ABIInternal half.
func goInternalArgumentFrame(function *ir.Func) (goArgumentFrame, error) {
	values, err := goInternalParamValues(function)
	if err != nil {
		return goArgumentFrame{}, err
	}
	if err := goCheckFloatArgumentRegisters(fmt.Sprintf("function %q", function.Name), values); err != nil {
		return goArgumentFrame{}, err
	}

	// incoming are the stack-passed argument words, written by the caller before
	// the call and so valid for its whole duration. prologue are the words only
	// the callee ever writes -- the stack result area and the register home slots
	// -- which hold nothing meaningful outside the entry window.
	incoming := map[int]bool{}
	prologue := map[int]bool{}

	assigner := newArgAssigner(true)
	groups := make([]goRegisterSpillGroup, 0, len(values))
	for _, value := range values {
		parts, onStack, stackOffset := assignGoValue(&assigner, value)
		if onStack {
			markStackedPointers(incoming, value, stackOffset)
			continue
		}
		groups = append(groups, spillGroupFor(value, parts))
	}

	// The closure environment arrives in RDX on an ABIInternal closure call. It is
	// a live managed pointer at entry, so morestack has to see it: it gets a home
	// slot like any other register argument.
	//
	// arm64 additionally looks for the Fixed temporary pinned to its closure
	// register and skips the slot when there is none. That check is dropped here
	// on purpose: ir.Func.HasClosureContext already *is* the statement that
	// ABIInternal supplies the pointer in the dedicated register, and keying off a
	// temporary that only exists after lowering would make the argument frame
	// change shape between the lowering that reserves it and the prologue that
	// fills it.
	if function.HasClosureContext {
		groups = append(groups, goRegisterSpillGroup{
			reg:       regClosure,
			size:      8,
			alignment: 8,
			pointer:   true,
		})
	}

	resultEnd := roundUp(assigner.nsaa, ir.PointerWordBytes)
	if function.RetAgg != nil {
		result, err := goAggregateValue(fmt.Sprintf("result of function %q", function.Name), function.RetAgg)
		if err != nil {
			return goArgumentFrame{}, err
		}
		// Results start their own register banks, so the cap is checked separately.
		if err := goCheckFloatArgumentRegisters(fmt.Sprintf("result of function %q", function.Name), []goValue{result}); err != nil {
			return goArgumentFrame{}, err
		}
		results := newArgAssigner(true)
		results.nsaa = resultEnd
		_, onStack, stackOffset := assignGoAggregate(&results, function.RetAgg)
		if onStack {
			markStackedPointers(prologue, result, stackOffset)
		}
		resultEnd = results.nsaa
	}

	return finishGoArgumentFrame(groups, resultEnd, incoming, prologue), nil
}

// platformArgumentFrame is goArgumentFrameFor's System V half: a managed-frame
// function whose arguments follow the platform ABI still needs home slots so
// morestack can copy its register arguments.
//
// It mirrors lowerParams exactly -- including ignoring ParamGroups, which
// lowerParams does not consult -- because a home slot that disagrees with where
// lowering actually put the argument is worse than no home slot at all.
// Aggregates are classified System V's way, by eightbyte.
func platformArgumentFrame(function *ir.Func) (goArgumentFrame, error) {
	incoming := map[int]bool{}
	prologue := map[int]bool{}

	assigner := newArgAssigner(false)
	var groups []goRegisterSpillGroup

	// A MEMORY-class result reserves the first integer argument register for the
	// caller's result buffer, ahead of the arguments (lowerParams does the same).
	// The pointer is into the caller's frame, so it is a root.
	if function.RetAgg != nil && classifyAgg(function.RetAgg).memory {
		assigner.ngrn = 1
		groups = append(groups, goRegisterSpillGroup{
			reg:       argGP[0],
			size:      8,
			alignment: 8,
			pointer:   true,
		})
	}

	for _, parameter := range function.Params {
		if parameter.Agg != nil {
			group, stackOffset, stacked := platformAggregateSpillGroup(&assigner, parameter.Agg)
			if stacked {
				for _, offset := range ir.AggregatePointerOffsets(parameter.Agg) {
					incoming[stackOffset+offset] = true
				}
				continue
			}
			groups = append(groups, group)
			continue
		}
		if parameter.Cls == ir.ClsQ {
			return goArgumentFrame{}, goQuadRefusal(fmt.Sprintf("function %q parameter %q", function.Name, parameter.Name))
		}
		pointer := parameter.GCRef || parameter.Cls.IsManaged()
		location := assigner.assign(parameter.Cls)
		if location.onStack {
			if pointer {
				incoming[location.stacky] = true
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

	// System V has no stack-assigned results: a MEMORY result travels through the
	// caller's buffer, whose address is an ordinary register argument.
	resultEnd := roundUp(assigner.nsaa, ir.PointerWordBytes)
	return finishGoArgumentFrame(groups, resultEnd, incoming, prologue), nil
}

// platformAggregateSpillGroup gives one System V by-value aggregate parameter
// its home slots, or reports the stack offset it was passed at.
func platformAggregateSpillGroup(assigner *argAssigner, aggregate *ir.AggType) (group goRegisterSpillGroup, stackOffset int, stacked bool) {
	classification := classifyAgg(aggregate)
	registers, onStack, offset := assigner.assignAgg(classification)
	if classification.memory || onStack {
		return goRegisterSpillGroup{}, offset, true
	}
	pointerAt := map[int]bool{}
	for _, pointerOffset := range ir.AggregatePointerOffsets(aggregate) {
		pointerAt[pointerOffset] = true
	}
	parts := make([]goABIPart, len(registers))
	for index, register := range registers {
		sub := ir.SubL
		pointer := pointerAt[index*8]
		if register.float {
			// A float eightbyte holds one double, or a single in the tail.
			sub = ir.SubD
			if ebBytes(classification.size, index) < 8 {
				sub = ir.SubS
			}
			pointer = false
		}
		parts[index] = goABIPart{sub: sub, offset: index * 8, pointer: pointer, reg: register.reg}
	}
	// The home area is whole eightbytes: an integer eightbyte is stored as a full
	// register, exactly as lowerAggParam reconstructs it.
	return goRegisterSpillGroup{
		parts:     parts,
		size:      len(registers) * 8,
		alignment: 8,
	}, 0, false
}

// spillGroupFor turns one register-assigned value into its home-slot group.
func spillGroupFor(value goValue, parts []goABIPart) goRegisterSpillGroup {
	size, alignment := value.layout()
	if value.agg != nil {
		return goRegisterSpillGroup{parts: parts, size: size, alignment: alignment}
	}
	return goRegisterSpillGroup{
		reg:       parts[0].reg,
		size:      size,
		alignment: alignment,
		float:     value.cls.IsFloat(),
		pointer:   value.pointer,
	}
}

// markStackedPointers records the pointer words of a value passed on the stack.
// An aggregate's pointers come from its type -- including from a shape that does
// not flatten, which is exactly the case that travels as memory -- and a scalar's
// from the value itself.
func markStackedPointers(offsets map[int]bool, value goValue, stackOffset int) {
	if value.agg != nil {
		for _, offset := range ir.AggregatePointerOffsets(value.agg) {
			offsets[stackOffset+offset] = true
		}
		return
	}
	if value.pointer {
		offsets[stackOffset] = true
	}
}

// finishGoArgumentFrame lays the register home slots out above the stack
// arguments and stack results, and assembles the frame.
//
// The cursor starts at resultEnd -- the end of everything the caller placed --
// and never carries a frame-chain link term, because amd64 has none: the return
// address the CALL pushed sits *below* the argument frame base, not inside it.
func finishGoArgumentFrame(groups []goRegisterSpillGroup, resultEnd int, incoming, prologue map[int]bool) goArgumentFrame {
	cursor := roundUp(resultEnd, ir.PointerWordBytes)
	var spills []goRegisterSpill
	for _, group := range groups {
		if group.size == 0 {
			continue
		}
		cursor = roundUp(cursor, group.alignment)
		if len(group.parts) == 0 {
			spill := goRegisterSpill{
				reg:     group.reg,
				offset:  cursor,
				size:    group.size,
				float:   group.float,
				pointer: group.pointer,
			}
			spills = append(spills, spill)
			if spill.pointer {
				prologue[cursor] = true
			}
		} else {
			for _, part := range group.parts {
				spill := goRegisterSpill{
					reg:     part.reg,
					offset:  cursor + part.offset,
					size:    part.sub.Size(),
					float:   part.sub.Cls().IsFloat(),
					pointer: part.pointer,
				}
				spills = append(spills, spill)
				if spill.pointer {
					prologue[cursor+part.offset] = true
				}
			}
		}
		cursor += group.size
	}
	cursor = roundUp(cursor, ir.PointerWordBytes)

	entry := make(map[int]bool, len(incoming)+len(prologue))
	for offset := range incoming {
		entry[offset] = true
	}
	for offset := range prologue {
		entry[offset] = true
	}
	return goArgumentFrame{
		spills:               spills,
		size:                 cursor,
		pointerWords:         argumentPointerWords(entry),
		incomingPointerWords: argumentPointerWords(incoming),
	}
}

// argumentPointerWords turns a set of argument-frame byte offsets into the
// sorted word indexes a Go stack map is written from, dropping any offset that
// is not word-aligned and so cannot name a pointer word.
//
// The word index is offset/8 with no bias: argument-frame offsets are already
// measured from the argument frame base (see the convention note at the top of
// this file), unlike arm64's, which are measured past a hand-reserved link.
func argumentPointerWords(offsets map[int]bool) []int {
	words := make([]int, 0, len(offsets))
	for offset := range offsets {
		if offset%ir.PointerWordBytes == 0 {
			words = append(words, offset/ir.PointerWordBytes)
		}
	}
	sort.Ints(words)
	return words
}

// ---------------------------------------------------------------------------
// Call-area sizing
// ---------------------------------------------------------------------------

// goABIHome is one register argument's home slot at a call site, before offsets
// are assigned. Only its size and alignment matter to the caller: the caller
// reserves the room, and the callee's prologue decides what to put there.
type goABIHome struct {
	size      int
	alignment int
}

// goCallStackBytes returns the outgoing-argument area an ABIInternal call site
// must reserve: the stack-passed arguments, then any stack-assigned results
// (resultEnd is where the caller's result assignment ended), then home slots for
// every register argument, then the closure context slot for a closure call.
//
// The result is rounded to 16 so RSP stays 16-aligned at the CALL. It carries no
// eight-byte frame-chain link: computeFrame places the outgoing area at the
// bottom of the frame, so the area *is* the argument frame, and the CALL pushes
// the return address below it. arm64 adds goStackLinkSize here because AArch64's
// caller has to reserve the link slot by hand; adding it on amd64 would
// double-count eight bytes at every call site.
func goCallStackBytes(f *ir.Func, call *ir.Instr, resultEnd int) (int, error) {
	values, err := goInternalArgValues(f, call)
	if err != nil {
		return 0, err
	}
	if err := goCheckFloatArgumentRegisters(fmt.Sprintf("call in function %q", f.Name), values); err != nil {
		return 0, err
	}

	assigner := newArgAssigner(true)
	var homes []goABIHome
	for _, value := range values {
		_, onStack, _ := assignGoValue(&assigner, value)
		if onStack {
			continue
		}
		size, alignment := value.layout()
		homes = append(homes, goABIHome{size: size, alignment: alignment})
	}
	return callAreaBytes(homes, max(resultEnd, roundUp(assigner.nsaa, ir.PointerWordBytes)), call.ClosureCall), nil
}

// platformCallStackBytes returns the outgoing-argument area a *managed* System V
// call site must reserve. Stack arguments keep their ordinary System V offsets at
// the bottom of the area; register arguments are given home slots above them so
// the callee's stack-growth prologue can preserve them across morestack, exactly
// as platformArgumentFrame expects to find them.
//
// An unmanaged System V call needs none of this and keeps using
// argAssigner.stackBytes.
func platformCallStackBytes(f *ir.Func, call *ir.Instr) (int, error) {
	assigner := newArgAssigner(false)
	var homes []goABIHome

	if call.RetAgg != nil && classifyAgg(call.RetAgg).memory {
		assigner.ngrn = 1
		homes = append(homes, goABIHome{size: 8, alignment: 8})
	}

	arguments := call.Args[1:]
	for index := 0; index < len(arguments); index++ {
		if aggregate := aggArgAt(call, index); aggregate != nil {
			classification := classifyAgg(aggregate)
			registers, onStack, _ := assigner.assignAgg(classification)
			if !classification.memory && !onStack && len(registers) > 0 {
				homes = append(homes, goABIHome{size: len(registers) * 8, alignment: 8})
			}
			continue
		}
		class := f.ClassOf(arguments[index])
		if class == ir.ClsQ {
			return 0, goQuadRefusal(fmt.Sprintf("function %q call argument %d", f.Name, index))
		}
		location := assigner.assign(class)
		if location.onStack {
			continue
		}
		homes = append(homes, goABIHome{size: class.Size(), alignment: class.Size()})
	}

	return callAreaBytes(homes, roundUp(assigner.nsaa, ir.PointerWordBytes), call.ClosureCall), nil
}

// callAreaBytes lays home slots out above the caller-written area and rounds the
// whole thing to the 16-byte stack alignment the CALL requires.
func callAreaBytes(homes []goABIHome, callerEnd int, closureCall bool) int {
	cursor := roundUp(callerEnd, ir.PointerWordBytes)
	for _, home := range homes {
		if home.size == 0 {
			continue
		}
		cursor = roundUp(cursor, home.alignment)
		cursor += home.size
	}
	if closureCall {
		cursor = roundUp(cursor, ir.PointerWordBytes)
		cursor += 8
	}
	cursor = roundUp(cursor, ir.PointerWordBytes)
	if cursor == 0 {
		return 0
	}
	return roundUp(cursor, 16)
}
