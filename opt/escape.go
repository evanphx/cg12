package opt

import (
	"os"

	"github.com/evanphx/cg12/ir"
)

type localSlot struct {
	base   locKey
	offset int64
}

// EscapeSummaries turns on the cross-function fact table inside
// LowerHeapAllocations. It is on, and GOC_ESCAPE_SUMMARIES=0 turns it off.
//
// The knob is kept for bisection, which is the only thing it is for. With it off
// LowerHeapAllocations is handed a nil table and every call falls into the same
// assume-the-worst arm it fell into before summaries existed: the pass cannot
// tell it is running under a build that has them. So a placement that looks
// wrong can be attributed to the table or cleared of it in one run, without
// rebuilding the compiler.
//
// It does not turn off the loop rule, which is a safety property of promotion
// rather than part of the table -- see promotionsBlockedByALoop.
var EscapeSummaries = os.Getenv("GOC_ESCAPE_SUMMARIES") != "0"

// HeapAllocLowering counts what LowerHeapAllocations did with the candidates it
// saw, which is the number that says whether the pass got smarter or just got
// more work.
type HeapAllocLowering struct {
	Promoted int // candidates that became frame slots
	Lowered  int // candidates that became allocator calls
	// LoopBlocked counts the candidates inside Lowered that the escape analysis
	// was willing to promote and the loop rule sent to the allocator anyway. It
	// is the price of promotionsBlockedByALoop, and the only number that says
	// whether the rule is doing anything.
	LoopBlocked int
}

// Rate is the share of candidates promoted to a frame slot.
func (stats HeapAllocLowering) Rate() float64 {
	total := stats.Promoted + stats.Lowered
	if total == 0 {
		return 0
	}
	return float64(stats.Promoted) / float64(total)
}

// HeapAllocLoweringStats reads the promotion counts back out of a compiled
// module. LowerHeapAllocations records one AllocDecision per candidate, so this
// is a count of that record rather than a second instrument.
func HeapAllocLoweringStats(module *ir.Module) HeapAllocLowering {
	var stats HeapAllocLowering
	for _, decision := range module.AllocDecisions {
		if decision.Placement == ir.AllocInFrame {
			stats.Promoted++
			continue
		}
		stats.Lowered++
		if decision.BlockedByLoop {
			stats.LoopBlocked++
		}
	}
	return stats
}

// LowerHeapAllocations promotes typed heap-allocation candidates whose
// pointers provably remain local to stack slots. Candidates that may escape
// are lowered to ordinary allocator calls.
//
// Each decision is appended to module.AllocDecisions as it is made. That record
// is diagnostic only -- nothing in the compiler reads it -- but it is the only
// place the frame half of the decision survives: a promoted candidate becomes a
// bare OAlloc carrying a byte size and nothing else, so after this pass runs
// there is no way to ask the IR which frame slots used to be allocations, or of
// what type. See AllocationCensus.
func LowerHeapAllocations(module *ir.Module) bool {
	var facts *EscapeFacts
	if EscapeSummaries {
		facts = ComputeEscapeFacts(module)
	}
	return lowerHeapAllocations(module, facts)
}

// LowerHeapAllocationsWithFacts is LowerHeapAllocations handed a summary table
// computed elsewhere, so a caller that already has one -- or that wants to
// measure with and without -- does not pay for it twice. A nil table is the
// no-summaries configuration and behaves exactly as the pass always has.
func LowerHeapAllocationsWithFacts(module *ir.Module, facts *EscapeFacts) bool {
	return lowerHeapAllocations(module, facts)
}

func lowerHeapAllocations(module *ir.Module, facts *EscapeFacts) bool {
	byName := moduleFuncsByName(module, facts)
	changed := false
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		if lowerFunctionHeapAllocations(function, byName, facts, &module.AllocDecisions) {
			changed = true
		}
	}
	return changed
}

// moduleFuncsByName indexes a module's functions for summary lookups. Without a
// table there is nothing to look up, so it is not built.
func moduleFuncsByName(module *ir.Module, facts *EscapeFacts) map[string]*ir.Func {
	if facts == nil {
		return nil
	}
	byName := make(map[string]*ir.Func, len(module.Funcs))
	for _, function := range module.Funcs {
		byName[function.Name] = function
	}
	return byName
}

// candidateEscapes is the per-function may-analysis behind the pass: which of a
// given set of allocations may have their address outlive the frame.
//
// It is separated from the rewrite so the same analysis can be asked about
// allocations the front end already placed, which is what ShadowPlacement does.
// Seeding it with the front-end's own allocations rather than with this
// function's OHeapAlloc candidates is the only difference between the two uses.
type candidateEscapes struct {
	function *ir.Func
	bases    map[uint32]uint32
	escaped  map[uint32]bool
	// reasons names, per escaped allocation, the first use that escaped it. It
	// is only filled when asked for: the pass itself does not need it and the
	// strings would be built on every compile.
	reasons map[uint32]string
}

func (analysis *candidateEscapes) escapes(id uint32) bool { return analysis.escaped[id] }

func (analysis *candidateEscapes) reason(id uint32) string {
	if analysis.reasons == nil {
		return ""
	}
	return analysis.reasons[id]
}

func lowerFunctionHeapAllocations(function *ir.Func, byName map[string]*ir.Func, facts *EscapeFacts, decisions *[]ir.AllocDecision) bool {
	var seeds []uint32
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op == ir.OHeapAlloc && instruction.To.Kind == ir.RefTemp {
				seeds = append(seeds, instruction.To.ID)
			}
		}
	}
	if len(seeds) == 0 {
		return false
	}
	escapes := analyzeCandidateEscapes(function, byName, facts, seeds, false)
	rewriteHeapAllocations(function, escapes, promotionsBlockedByALoop(function, seeds, escapes), decisions)
	return true
}

// promotionsBlockedByALoop escapes every candidate the analysis was willing to
// promote that sits inside a natural loop, and reports which ones it blocked.
//
// Neither this analysis nor goc's AST walk has a notion of iteration. Both
// answer "does a pointer outlive the frame", and an allocation in a loop body
// can be entirely frame-local by that test and still need one object per
// iteration. Promoting it puts every iteration on one slot, so two objects the
// source says are distinct become the same object: goc prints `2 2` where Go
// prints `1 2`. RUNTIME_PLAN.md section 5.9 is what freshVariableStorage exists
// for on the front end's side of the same problem.
//
// opt.FrameEscapes is structurally blind to this. It asks whether a frame
// address was published past its frame, and in the aliasing case none is -- both
// pointers stay inside the frame and are simply the same pointer. A clean audit
// says nothing about it, which is why the rule is here rather than left to be
// caught downstream.
//
// It applies whether or not the summaries are on. The rule is about the loop,
// not about how the analysis came to be willing to promote; every promotion is a
// placement change and every placement change in a loop body is this hazard.
func promotionsBlockedByALoop(function *ir.Func, seeds []uint32, escapes *candidateEscapes) map[uint32]bool {
	promotable := make(map[uint32]bool, len(seeds))
	for _, seed := range seeds {
		if !escapes.escapes(seed) {
			promotable[seed] = true
		}
	}
	if len(promotable) == 0 {
		// Nothing was going to be promoted, so there is no CFG to build. This is
		// the common case: most candidates reach something that escapes them.
		return nil
	}
	blocked := allocationsInLoops(function, promotable)
	for id := range blocked {
		escapes.escaped[id] = true
	}
	return blocked
}

// analyzeCandidateEscapes runs the may-analysis over one function for the
// allocations named by seeds.
func analyzeCandidateEscapes(function *ir.Func, byName map[string]*ir.Func, facts *EscapeFacts, seeds []uint32, wantReasons bool) *candidateEscapes {
	aliases := newAliasInfo(function)
	definitions := make(map[uint32]ir.Instr)
	bases := make(map[uint32]uint32)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.To.Kind == ir.RefTemp {
				definitions[instruction.To.ID] = instruction
			}
		}
	}
	for _, seed := range seeds {
		bases[seed] = seed
	}

	escaped := make(map[uint32]bool)
	var reasons map[uint32]string
	if wantReasons {
		reasons = make(map[uint32]string)
	}
	slotBases := make(map[localSlot]uint32)
	conflictedSlots := make(map[localSlot]bool)
	for updated := true; updated; {
		updated = false
		for id, instruction := range definitions {
			if known, tracked := bases[id]; tracked {
				// A call result can gain a second tracked argument after the
				// round that first named it -- an argument whose own base
				// arrives through a slot load, say, which the loop below
				// resolves. Two allocations reaching one result is a conflict
				// whenever it becomes visible, not only when it is visible
				// first: leaving the result bound to whichever one was resolved
				// earliest escapes that one and silently promotes the other,
				// which the call may equally well have returned.
				if facts != nil {
					if base, ok := leakedCallResultBase(function, byName, facts, instruction, bases, escaped); ok && base != known {
						escaped[base] = true
						escaped[known] = true
					}
				}
				continue
			}
			base, ok := derivedHeapBase(instruction, bases)
			if !ok && facts != nil {
				base, ok = leakedCallResultBase(function, byName, facts, instruction, bases, escaped)
			}
			if ok {
				bases[id] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, phi := range block.Phis {
				base, found, conflict := phiHeapBase(phi, bases)
				if conflict {
					for _, argument := range phi.Args {
						if argumentBase, tracked := heapBase(argument, bases); tracked {
							escaped[argumentBase] = true
						}
					}
					continue
				}
				if !found || phi.To.Kind != ir.RefTemp {
					continue
				}
				if previous, exists := bases[phi.To.ID]; exists {
					if previous != base {
						escaped[previous] = true
						escaped[base] = true
					}
					continue
				}
				bases[phi.To.ID] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				destination, source, size, memoryCopy := memoryCopyOperands(function, instruction)
				if memoryCopy {
					sourceLocation := aliases.locOf(source, int(size))
					destinationLocation := aliases.locOf(destination, int(size))
					if sourceLocation.class == cLocal {
						for sourceSlot, base := range slotBases {
							if sourceSlot.base != sourceLocation.key() {
								continue
							}
							if sourceSlot.offset < sourceLocation.offset || sourceSlot.offset+8 > sourceLocation.offset+size {
								continue
							}
							if destinationLocation.class != cLocal {
								escaped[base] = true
								continue
							}
							destinationSlot := localSlot{
								base:   destinationLocation.key(),
								offset: destinationLocation.offset + sourceSlot.offset - sourceLocation.offset,
							}
							if conflictedSlots[destinationSlot] {
								escaped[base] = true
								continue
							}
							if previous, exists := slotBases[destinationSlot]; exists && previous != base {
								escaped[previous] = true
								escaped[base] = true
								delete(slotBases, destinationSlot)
								conflictedSlots[destinationSlot] = true
								continue
							}
							if _, exists := slotBases[destinationSlot]; !exists {
								slotBases[destinationSlot] = base
								updated = true
							}
						}
					}
				}
				if value, address, isPointerStore := trackedPointerStore(function, instruction); isPointerStore {
					base, tracked := heapBase(value, bases)
					location := aliases.locOf(address, 1)
					if !tracked || location.class != cLocal {
						continue
					}
					slot := localSlot{base: location.key(), offset: location.offset}
					if conflictedSlots[slot] {
						escaped[base] = true
						continue
					}
					if previous, exists := slotBases[slot]; exists && previous != base {
						escaped[previous] = true
						escaped[base] = true
						delete(slotBases, slot)
						conflictedSlots[slot] = true
						continue
					}
					if _, exists := slotBases[slot]; !exists {
						slotBases[slot] = base
						updated = true
					}
					continue
				}
				if !instruction.Op.IsLoad() || instruction.To.Kind != ir.RefTemp {
					continue
				}
				location := aliases.locOf(instruction.Arg(0), 1)
				if location.class != cLocal {
					continue
				}
				slot := localSlot{base: location.key(), offset: location.offset}
				base, tracked := slotBases[slot]
				if !tracked {
					continue
				}
				if previous, exists := bases[instruction.To.ID]; exists && previous != base {
					escaped[previous] = true
					escaped[base] = true
					continue
				}
				if _, exists := bases[instruction.To.ID]; !exists {
					bases[instruction.To.ID] = base
					updated = true
				}
			}
		}
	}

	// Which tracked allocations hold a pointer into themselves. Collected in its
	// own pass rather than as the mark loop meets them, because a call can
	// precede in program order the store that makes its argument
	// self-referential, and the answer has to be the same either way.
	selfReferential := make(map[uint32]bool)
	noteSelfReference := func(value, destination ir.Ref) {
		if base, tracked := heapBase(value, bases); tracked && storesIntoItself(value, destination, bases) {
			selfReferential[base] = true
		}
	}
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			switch {
			case isAtomicPointerStore(function, instruction):
				noteSelfReference(instruction.Arg(2), instruction.Arg(1))
			case instruction.Op.IsStore():
				noteSelfReference(instruction.Arg(0), instruction.Arg(1))
			}
		}
	}

	mark := func(reference ir.Ref, why string) {
		if reference.Kind == ir.RefTemp {
			if base, ok := bases[reference.ID]; ok {
				if reasons != nil && !escaped[base] {
					reasons[base] = why
				}
				escaped[base] = true
			}
		}
	}
	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			if _, tracked := heapBase(phi.To, bases); tracked {
				continue
			}
			for _, argument := range phi.Args {
				mark(argument, "phi")
			}
		}
		for _, instruction := range block.Instrs {
			switch {
			case instruction.Op == ir.OHeapAlloc:
				// The allocator, type descriptor, size, and -- on a converted
				// candidate -- the helper and the value it is handed are not
				// uses of the result. The value is one the helper copies, never
				// a pointer the object could be reached through: only a
				// register-shaped, pointer-free type has a helper at all.
			case instruction.Op.IsLoad():
				// Reading through the candidate keeps it local.
			case instruction.Op == ir.OCmp:
				// Comparing a pointer observes its value but cannot retain it.
			case instruction.Op.IsStore():
				// Frontend variables live in ordinary stack allocations. Saving a
				// candidate in a non-escaping local slot does not make the pointed-to
				// object escape; storing it anywhere externally reachable does.
				if aliases.locOf(instruction.Arg(1), 1).class != cLocal &&
					!storesIntoItself(instruction.Arg(0), instruction.Arg(1), bases) {
					mark(instruction.Arg(0), "store into non-local storage")
				}
			case facts != nil && instruction.Op == ir.OCall &&
				!isAtomicPointerStore(function, instruction) && !benignMemoryCall(function, instruction):
				// The summarised form of the default arm below: an argument is only
				// a publication when the callee's summary says the callee can retain
				// it. This is the entire behavioural difference the fact table makes,
				// and with facts nil the case cannot be reached.
				markSummarisedCall(function, byName, facts, instruction, bases, selfReferential, mark)
			case isTrackedHeapDerivation(instruction, bases):
				// Copies, casts, and constant pointer offsets preserve locality.
			case isAtomicPointerStore(function, instruction):
				// The destination is observed only for the duration of the store.
				// This is the write-barrier form emitted for pointer fields before
				// escape lowering knows whether the enclosing object is stack-local.
				destination := instruction.Arg(1)
				if _, localCandidate := heapBase(destination, bases); localCandidate {
					if !storesIntoItself(instruction.Arg(2), destination, bases) {
						mark(instruction.Arg(2), "write barrier into a candidate")
					}
				} else if aliases.locOf(destination, 1).class != cLocal {
					mark(instruction.Arg(2), "write barrier into non-local storage")
				}
			case benignMemoryCall(function, instruction):
				// memcpy/memset observe the storage but do not retain its address.
			default:
				for _, argument := range instruction.Args {
					mark(argument, instructionReason(function, instruction))
				}
			}
		}
		mark(block.Jmp.Arg, "returned")
		for _, argument := range block.Jmp.Args {
			mark(argument, "returned")
		}
	}
	return &candidateEscapes{function: function, bases: bases, escaped: escaped, reasons: reasons}
}

// rewriteHeapAllocations legalises every candidate in function according to the
// analysis: promoted candidates become frame allocations with the zeroing the
// allocator used to do, and the rest become the allocator call the instruction
// was already shaped like -- or, for a candidate that named a conversion
// helper, the call to that helper.
func rewriteHeapAllocations(function *ir.Func, analysis *candidateEscapes, loopBlocked map[uint32]bool, decisions *[]ir.AllocDecision) {
	escaped := analysis.escaped
	for _, block := range function.Blocks {
		lowered := make([]ir.Instr, 0, len(block.Instrs))
		droppedStore := -1
		for index, original := range block.Instrs {
			if index == droppedStore {
				continue
			}
			instruction := original
			if instruction.Op != ir.OHeapAlloc || instruction.To.Kind != ir.RefTemp {
				lowered = append(lowered, instruction)
				continue
			}
			if escaped[instruction.To.ID] {
				recordAllocDecision(decisions, function, original, ir.AllocOnHeap, loopBlocked[instruction.To.ID])
				if convertsItsObject(original) && initializesCandidate(block, index+1, original.To) {
					// The helper returns storage already holding the value, so
					// the initializing store has nothing left to do -- and must
					// not run, because for a small value the helper hands back a
					// pointer into a read-only table.
					droppedStore = index + 1
					instruction.Op = ir.OCall
					instruction.Args = []ir.Ref{original.Args[3], original.Args[4]}
					instruction.Aux = 0
					lowered = append(lowered, instruction)
					continue
				}
				instruction.Op = ir.OCall
				instruction.Args = instruction.Args[:2]
				instruction.Aux = 0
				lowered = append(lowered, instruction)
				continue
			}
			recordAllocDecision(decisions, function, original, ir.AllocInFrame, false)

			size := instruction.Args[2]
			switch instruction.Aux {
			case 4:
				instruction.Op = ir.OAlloc4
			case 8:
				instruction.Op = ir.OAlloc8
			case 16:
				instruction.Op = ir.OAlloc16
			default:
				panic("opt: invalid heap allocation alignment")
			}
			instruction.Args = []ir.Ref{size}
			instruction.Aux = 0
			lowered = append(lowered, instruction)
			lowered = append(lowered, ir.Instr{
				Op:   ir.OCall,
				Cls:  ir.ClsW,
				Args: []ir.Ref{function.Sym("goc_memset", 0), instruction.To, function.Word(0), size},
				Pos:  instruction.Pos,
			})
		}
		block.Instrs = lowered
	}
}

// convertsItsObject reports whether a candidate names a runtime conversion
// helper that can build its object without an allocator. ir.Block's
// HeapAllocConverted is what puts one there, and its doc comment carries the
// contract; everything this function needs is the two extra arguments.
func convertsItsObject(candidate ir.Instr) bool {
	return len(candidate.Args) == 5
}

// initializesCandidate reports whether the instruction at index is the store
// that gives a freshly allocated candidate its first value.
//
// The candidate's result is a temporary the emitter has just made, so a store
// through it is the initialization and nothing else; requiring the store to sit
// immediately after the candidate is what makes that true of the *only* store
// this looks at, rather than of some later write the emitter did not promise
// anything about.
func initializesCandidate(block *ir.Block, index int, candidate ir.Ref) bool {
	if index >= len(block.Instrs) {
		return false
	}
	store := block.Instrs[index]
	return store.Op.IsStore() && len(store.Args) == 2 && store.Args[1] == candidate
}

// storesIntoItself reports a store whose value and whose destination are both
// derived from the same tracked allocation: the object is being made to point
// at part of itself.
//
// Nothing outside can reach the object any more easily for it, so the store
// says nothing about whether the object escapes -- "it escapes if it escapes"
// is all it means. Marking it a publication instead is what kept a variadic
// `...any` call's backing object on the heap: the front end packs the `[]any`
// array and the boxed payloads it points at into one allocation, so writing
// each element's data word writes a pointer into that object, into that same
// object.
func storesIntoItself(value, destination ir.Ref, bases map[uint32]uint32) bool {
	valueBase, valueTracked := heapBase(value, bases)
	destinationBase, destinationTracked := heapBase(destination, bases)
	return valueTracked && destinationTracked && valueBase == destinationBase
}

// recordAllocDecision notes where one heap-allocation candidate landed.
// candidate must still be the unrewritten OHeapAlloc, whose first two arguments
// are the allocator and the type descriptor.
func recordAllocDecision(decisions *[]ir.AllocDecision, function *ir.Func, candidate ir.Instr, placement ir.AllocPlacement, blockedByLoop bool) {
	if decisions == nil {
		return
	}
	*decisions = append(*decisions, ir.AllocDecision{
		Func:          function.Name,
		Pos:           candidate.Pos,
		Allocator:     constSymbolName(function, candidate.Args[0]),
		Type:          constSymbolName(function, candidate.Args[1]),
		Placement:     placement,
		BlockedByLoop: blockedByLoop,
	})
}

// constSymbolName returns the symbol name a reference names, or "" if it is not
// a symbol constant.
func constSymbolName(function *ir.Func, reference ir.Ref) string {
	if reference.Kind != ir.RefConst || int(reference.ID) >= len(function.Consts) {
		return ""
	}
	constant := function.Consts[reference.ID]
	if constant.Kind != ir.ConstSym {
		return ""
	}
	return constant.Sym
}

func phiHeapBase(phi *ir.Phi, bases map[uint32]uint32) (uint32, bool, bool) {
	var base uint32
	found := false
	for _, argument := range phi.Args {
		argumentBase, tracked := heapBase(argument, bases)
		if !tracked {
			continue
		}
		if found && argumentBase != base {
			return 0, false, true
		}
		base = argumentBase
		found = true
	}
	return base, found, false
}

// trackedPointerStore names the value and the destination address of a store
// the base propagation has to follow.
//
// goc writes a pointer into a frame slot two ways: a plain store, and a call to
// the write barrier, which is what it emits for every pointer field of every
// pointer-bearing local before escape lowering knows whether the enclosing
// object is stack-local. Both put a candidate in that slot, and a later load
// recovers it.
//
// Modelling only the first is how a candidate used to lose its identity. The
// marking switch in analyzeCandidateEscapes already understands the barrier --
// isAtomicPointerStore is one of its cases -- so the two halves of the same
// analysis disagreed about what a barrier is: a candidate stored through one
// got no slotBases entry, the reload got no base, and every use of the reloaded
// pointer, including a publication into a heap object, was invisible. See
// TestLowerHeapAllocationsTracksEscapeThroughAWriteBarrieredLocalSlot.
func trackedPointerStore(function *ir.Func, instruction ir.Instr) (value, address ir.Ref, ok bool) {
	if instruction.Op.IsStore() {
		return instruction.Arg(0), instruction.Arg(1), true
	}
	if isAtomicPointerStore(function, instruction) {
		return instruction.Arg(2), instruction.Arg(1), true
	}
	return ir.R, ir.R, false
}

func isAtomicPointerStore(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) != 3 {
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
	return function.Module().SymAttrOf(constant.Sym).Has(ir.SymAtomicPointerStore)
}

func derivedHeapBase(instruction ir.Instr, bases map[uint32]uint32) (uint32, bool) {
	if instruction.Op == ir.OCopy || instruction.Op == ir.OCast {
		return heapBase(instruction.Arg(0), bases)
	}
	if (instruction.Op != ir.OAdd && instruction.Op != ir.OSub) || instruction.Cls != ir.ClsP {
		return 0, false
	}
	if base, ok := heapBase(instruction.Arg(0), bases); ok {
		return base, true
	}
	if instruction.Op == ir.OAdd {
		if base, ok := heapBase(instruction.Arg(1), bases); ok {
			return base, true
		}
	}
	return 0, false
}

func heapBase(reference ir.Ref, bases map[uint32]uint32) (uint32, bool) {
	if reference.Kind != ir.RefTemp {
		return 0, false
	}
	base, ok := bases[reference.ID]
	return base, ok
}

func isTrackedHeapDerivation(instruction ir.Instr, bases map[uint32]uint32) bool {
	if instruction.To.Kind != ir.RefTemp {
		return false
	}
	_, ok := bases[instruction.To.ID]
	return ok && instruction.Op != ir.OHeapAlloc
}

func benignMemoryCall(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
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
	case "memset", "memcpy", "memmove", "memcmp",
		"goc_memset", "goc_memcpy", "goc_memmove", "goc_memcmp",
		"runtime.growslice":
		return true
	default:
		return false
	}
}

func memoryCopyOperands(function *ir.Func, instruction ir.Instr) (ir.Ref, ir.Ref, int64, bool) {
	if instruction.Op != ir.OCall || len(instruction.Args) != 4 {
		return ir.R, ir.R, 0, false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return ir.R, ir.R, 0, false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return ir.R, ir.R, 0, false
	}
	switch constant.Sym {
	case "memcpy", "memmove", "goc_memcpy", "goc_memmove":
	default:
		return ir.R, ir.R, 0, false
	}
	size, knownSize := constInt(function, instruction.Arg(3))
	if !knownSize || size < 0 {
		return ir.R, ir.R, 0, false
	}
	return instruction.Arg(1), instruction.Arg(2), size, true
}
