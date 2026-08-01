package opt

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// FrameEscapeKind names the shape of a frame-address publication.
type FrameEscapeKind string

const (
	// FrameEscapeStore is a plain store of a frame address into memory the
	// frame does not own.
	FrameEscapeStore FrameEscapeKind = "store"
	// FrameEscapeBarrier is the same publication made through the write-barrier
	// helper, which is what a pointer field of a heap object receives.
	FrameEscapeBarrier FrameEscapeKind = "barrier"
	// FrameEscapeReturn is a function returning the address of its own frame.
	FrameEscapeReturn FrameEscapeKind = "return"
)

// FrameEscapeDestination classifies where a published frame address ended up.
// It is part of a finding's identity, so it names a category rather than an IR
// temporary: temporary numbers shift whenever anything upstream emits one more
// instruction, and a baseline keyed on them would churn on every unrelated
// change.
type FrameEscapeDestination string

const (
	// FrameEscapeIntoGlobal is a package-level variable's storage.
	FrameEscapeIntoGlobal FrameEscapeDestination = "a global"
	// FrameEscapeIntoResult is the result area the caller supplied, which the
	// caller reads after this frame is gone.
	FrameEscapeIntoResult FrameEscapeDestination = "the caller's result area"
	// FrameEscapeIntoParameter is memory reached through an incoming pointer,
	// which belongs to some frame other than this one.
	FrameEscapeIntoParameter FrameEscapeDestination = "memory reached through a parameter"
	// FrameEscapeIntoCallResult is memory reached through a pointer a call
	// returned, which for an allocator call is a heap object.
	FrameEscapeIntoCallResult FrameEscapeDestination = "memory reached through a call result"
	// FrameEscapeIntoLoaded is memory reached through a pointer loaded from
	// elsewhere, whose provenance this analysis cannot see.
	FrameEscapeIntoLoaded FrameEscapeDestination = "memory reached through a loaded pointer"
	// FrameEscapeIntoCaller is the return value.
	FrameEscapeIntoCaller FrameEscapeDestination = "the caller"
	// FrameEscapeIntoOpaque is anything else.
	FrameEscapeIntoOpaque FrameEscapeDestination = "memory reached through an opaque pointer"
)

// FrameEscape is one place where a function publishes the address of its own
// frame into storage that outlives the frame.
//
// It is a statement about the emitted IR, not about the Go source: the front
// end and opt.LowerHeapAllocations between them decide which allocations live
// in a frame, and a wrong decision shows up here as an OAlloc-derived pointer
// reaching a destination that is not part of the same frame.
type FrameEscape struct {
	// Func is the IR function the publication happens in.
	Func string
	// Kind is how the address leaves the frame.
	Kind FrameEscapeKind
	// Destination is the category of storage that received the address.
	Destination FrameEscapeDestination
	// Pos is the source position of the publishing instruction, when the front
	// end recorded one.
	Pos ir.SrcPos
	// File is Pos's file name, resolved against the module's file table.
	File string
	// Alloc is the frame allocation whose address is published, as an IR
	// temporary name. It is diagnostic detail, not part of Key.
	Alloc string
	// Callee names the function a call destination came from, when the
	// destination is a direct call's result.
	Callee string
}

// Key is the finding's stable identity: everything except the IR temporary
// names, which renumber whenever an unrelated change emits one more
// instruction. Two findings with the same key are the same defect.
func (escape FrameEscape) Key() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s", escape.location(), escape.Func, escape.Kind, escape.destination())
}

// String renders one finding as a single diagnostic line: the key, plus the
// temporary that names the allocation.
func (escape FrameEscape) String() string {
	return fmt.Sprintf("%s: %s: %s %s into %s", escape.location(), escape.Func, escape.Kind, escape.Alloc, escape.destination())
}

func (escape FrameEscape) location() string {
	if !escape.Pos.Valid() {
		return "?"
	}
	file := escape.File
	if file == "" {
		file = "?"
	}
	return fmt.Sprintf("%s:%d:%d", file, escape.Pos.Line, escape.Pos.Col)
}

func (escape FrameEscape) destination() string {
	if escape.Callee != "" {
		return fmt.Sprintf("%s $%s", escape.Destination, escape.Callee)
	}
	return string(escape.Destination)
}

// FrameEscapes reports every place in module where the address of a frame
// allocation is published to storage that outlives the frame.
//
// This is the verifier for the escape decision. The front end decides, per Go
// expression, whether an allocation may stay in the frame, and
// LowerHeapAllocations decides the same thing again for the candidates the
// front end left open. Neither decision is checked against the code that is
// actually emitted, and a wrong "does not escape" is silent: the program keeps
// running with a dead frame address in a live object until, minutes later and
// in an unrelated goroutine, the collector walks it and reports a bad pointer
// in the heap. Running this over the finished IR turns that into a compile-time
// report naming the storing function and source line.
//
// The analysis is a may-analysis over one function at a time. A value is
// frame-derived when it is an OAlloc result or is reachable from one through
// copies, casts, pointer arithmetic, phis, and the memory helpers that return
// their destination. A publication is reported when such a value is stored
// somewhere that is not itself part of the same frame -- a global, a heap
// object, or memory reached through a parameter, all of which outlive the
// frame.
//
// It is deliberately silent where it cannot prove the destination outlives the
// frame. Values that reach a call are not reported at all: passing &local to a
// callee is the ordinary way Go code is written, and whether the callee retains
// it is exactly the interprocedural question the front end already answers.
// What is reported is the store the callee, or the caller, actually performs.
func FrameEscapes(module *ir.Module) []FrameEscape {
	var escapes []FrameEscape
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		escapes = append(escapes, frameEscapesIn(module, function)...)
	}
	return escapes
}

// frameFacts is the per-function may-analysis: which SSA values can hold the
// address of one of this function's own frame allocations, and which frame
// slots can hold one.
type frameFacts struct {
	function *ir.Func
	aliases  *aliasInfo
	// base maps a temporary to the frame allocation its value can name.
	base map[uint32]uint32
	// slots maps a frame slot to a frame allocation its contents can name. A
	// load from such a slot can produce that address again.
	slots map[localSlot]uint32
}

func frameEscapesIn(module *ir.Module, function *ir.Func) []FrameEscape {
	facts := newFrameFacts(function)
	if len(facts.base) == 0 {
		return nil
	}

	var escapes []FrameEscape
	report := func(kind FrameEscapeKind, allocation uint32, position ir.SrcPos, destination ir.Ref, returned bool) {
		escape := FrameEscape{
			Func:  function.Name,
			Kind:  kind,
			Pos:   position,
			File:  module.FileName(position.File),
			Alloc: facts.allocName(allocation),
		}
		if returned {
			escape.Destination = FrameEscapeIntoCaller
		} else {
			escape.Destination, escape.Callee = facts.classify(destination)
		}
		escapes = append(escapes, escape)
	}

	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			switch {
			case instruction.Op.IsStore():
				allocation, derived := facts.frameBase(instruction.Arg(0))
				if !derived || facts.isFrameAddress(instruction.Arg(1)) {
					continue
				}
				report(FrameEscapeStore, allocation, instruction.Pos, instruction.Arg(1), false)
			case isAtomicPointerStore(function, instruction):
				allocation, derived := facts.frameBase(instruction.Arg(2))
				if !derived || facts.isFrameAddress(instruction.Arg(1)) {
					continue
				}
				report(FrameEscapeBarrier, allocation, instruction.Pos, instruction.Arg(1), false)
			}
		}
		if block.Jmp.Kind != ir.JmpRet || function.RetAgg != nil {
			continue
		}
		if allocation, derived := facts.frameBase(block.Jmp.Arg); derived {
			report(FrameEscapeReturn, allocation, block.Pos, ir.R, true)
		}
	}
	return escapes
}

func newFrameFacts(function *ir.Func) *frameFacts {
	facts := &frameFacts{
		function: function,
		aliases:  newAliasInfo(function),
		base:     make(map[uint32]uint32),
		slots:    make(map[localSlot]uint32),
	}
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if isFrameAllocation(instruction) && instruction.To.Kind == ir.RefTemp {
				facts.base[instruction.To.ID] = instruction.To.ID
			}
		}
	}
	if len(facts.base) == 0 {
		return facts
	}
	for changed := true; changed; {
		changed = facts.propagate()
	}
	return facts
}

// propagate runs one round of the fixed point, returning whether it learned
// anything new.
func (facts *frameFacts) propagate() bool {
	changed := false
	for _, block := range facts.function.Blocks {
		for _, phi := range block.Phis {
			for _, argument := range phi.Args {
				allocation, derived := facts.frameBase(argument)
				if derived && facts.record(phi.To, allocation) {
					changed = true
				}
			}
		}
		for _, instruction := range block.Instrs {
			if allocation, derived := facts.derivedFrameBase(instruction); derived {
				if facts.record(instruction.To, allocation) {
					changed = true
				}
			}
			if instruction.Op.IsStore() {
				allocation, derived := facts.frameBase(instruction.Arg(0))
				if !derived {
					continue
				}
				slot, inFrame := facts.frameSlot(instruction.Arg(1))
				if !inFrame {
					continue
				}
				if _, known := facts.slots[slot]; !known {
					facts.slots[slot] = allocation
					changed = true
				}
				continue
			}
			if !instruction.Op.IsLoad() {
				continue
			}
			slot, inFrame := facts.frameSlot(instruction.Arg(0))
			if !inFrame {
				continue
			}
			allocation, known := facts.slots[slot]
			if !known {
				continue
			}
			if facts.record(instruction.To, allocation) {
				changed = true
			}
		}
	}
	return changed
}

func (facts *frameFacts) record(reference ir.Ref, allocation uint32) bool {
	if reference.Kind != ir.RefTemp {
		return false
	}
	if _, known := facts.base[reference.ID]; known {
		return false
	}
	facts.base[reference.ID] = allocation
	return true
}

// derivedFrameBase reports the frame allocation an instruction's result can
// name, for the instructions that carry a pointer through unchanged.
func (facts *frameFacts) derivedFrameBase(instruction ir.Instr) (uint32, bool) {
	if instruction.To.Kind != ir.RefTemp {
		return 0, false
	}
	switch instruction.Op {
	case ir.OCopy, ir.OCast:
		return facts.frameBase(instruction.Arg(0))
	case ir.OAdd:
		if allocation, derived := facts.frameBase(instruction.Arg(0)); derived {
			return allocation, true
		}
		return facts.frameBase(instruction.Arg(1))
	case ir.OSub:
		return facts.frameBase(instruction.Arg(0))
	case ir.OSel:
		if allocation, derived := facts.frameBase(instruction.Arg(1)); derived {
			return allocation, true
		}
		return facts.frameBase(instruction.Arg(2))
	case ir.OCall:
		if !returnsItsDestination(facts.function, instruction) {
			return 0, false
		}
		return facts.frameBase(instruction.Arg(1))
	}
	return 0, false
}

// frameBase reports the frame allocation a value can name.
func (facts *frameFacts) frameBase(reference ir.Ref) (uint32, bool) {
	if reference.Kind != ir.RefTemp {
		return 0, false
	}
	allocation, known := facts.base[reference.ID]
	return allocation, known
}

// isFrameAddress reports whether a pointer provably names storage belonging to
// this function's frame, which is where a frame address may be stored.
func (facts *frameFacts) isFrameAddress(reference ir.Ref) bool {
	if _, derived := facts.frameBase(reference); derived {
		return true
	}
	location := facts.aliases.locOf(reference, 1)
	return location.keyKind == keyLocal || location.keyKind == keyEscaped
}

// frameSlot resolves a pointer to the frame slot it names, when it names one.
func (facts *frameFacts) frameSlot(reference ir.Ref) (localSlot, bool) {
	location := facts.aliases.locOf(reference, 1)
	if location.keyKind != keyLocal && location.keyKind != keyEscaped {
		return localSlot{}, false
	}
	if !location.offKnown {
		return localSlot{}, false
	}
	return localSlot{base: location.key(), offset: location.offset}, true
}

// classify names the category of storage a publication reached, and the callee
// when the destination came straight out of a direct call. The category is part
// of the finding's identity, so it must not mention a temporary number.
func (facts *frameFacts) classify(reference ir.Ref) (FrameEscapeDestination, string) {
	location := facts.aliases.locOf(reference, 1)
	switch location.keyKind {
	case keyGlobal:
		return FrameEscapeIntoGlobal, location.keySym
	case keyConst:
		return FrameEscapeIntoOpaque, ""
	}
	base := facts.pointerBase(reference)
	if base.Kind != ir.RefTemp {
		return FrameEscapeIntoOpaque, ""
	}
	if name, isParameter := facts.parameterName(base); isParameter {
		if strings.HasPrefix(name, "result") {
			return FrameEscapeIntoResult, ""
		}
		return FrameEscapeIntoParameter, ""
	}
	definition := facts.aliases.def[base.ID]
	if definition == nil {
		return FrameEscapeIntoOpaque, ""
	}
	switch {
	case definition.Op == ir.OCall:
		return FrameEscapeIntoCallResult, calleeSymbol(facts.function, *definition)
	case definition.Op.IsLoad():
		return FrameEscapeIntoLoaded, ""
	}
	return FrameEscapeIntoOpaque, ""
}

// pointerBase strips the constant pointer arithmetic locOf already accounted
// for, so classify looks at the instruction that produced the base pointer.
func (facts *frameFacts) pointerBase(reference ir.Ref) ir.Ref {
	current := reference
	for current.Kind == ir.RefTemp && int(current.ID) < len(facts.aliases.def) {
		definition := facts.aliases.def[current.ID]
		if definition == nil {
			return current
		}
		if definition.Op == ir.OCopy || definition.Op == ir.OCast {
			current = definition.Arg(0)
			continue
		}
		if definition.Op != ir.OAdd && definition.Op != ir.OSub {
			return current
		}
		if _, constant := constInt(facts.function, definition.Arg(1)); constant {
			current = definition.Arg(0)
			continue
		}
		if _, constant := constInt(facts.function, definition.Arg(0)); constant && definition.Op == ir.OAdd {
			current = definition.Arg(1)
			continue
		}
		return current
	}
	return current
}

func (facts *frameFacts) parameterName(reference ir.Ref) (string, bool) {
	if reference.Kind != ir.RefTemp {
		return "", false
	}
	for _, parameter := range facts.function.Params {
		if parameter != nil && uint32(parameter.ID) == reference.ID {
			return parameter.Name, true
		}
	}
	return "", false
}

func calleeSymbol(function *ir.Func, instruction ir.Instr) string {
	if len(instruction.Args) == 0 {
		return ""
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return ""
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return ""
	}
	return constant.Sym
}

func (facts *frameFacts) allocName(allocation uint32) string {
	return facts.tempName(allocation)
}

func (facts *frameFacts) tempName(id uint32) string {
	if int(id) >= len(facts.function.Temps) || facts.function.Temps[id] == nil {
		return fmt.Sprintf("%%t%d", id)
	}
	return "%" + facts.function.Temps[id].Name
}

// isFrameAllocation reports whether an instruction allocates storage in the
// current frame. OAllocN is included: a run-time-sized frame allocation is
// still a frame allocation.
func isFrameAllocation(instruction ir.Instr) bool {
	return instruction.Op.IsAlloc() || instruction.Op == ir.OAllocN
}

// returnsItsDestination reports whether a call is one of the memory helpers
// whose result is the destination pointer it was handed, so a frame address
// survives the call unchanged.
func returnsItsDestination(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) < 2 {
		return false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	switch constant.Sym {
	case "memset", "memcpy", "memmove",
		"goc_memset", "goc_memcpy", "goc_memmove":
		return true
	default:
		return false
	}
}
