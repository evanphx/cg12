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
// LowerHeapAllocations. It defaults to GOC_ESCAPE_SUMMARIES=1 and is off
// otherwise, so nothing about the compiler's output changes unless it is asked
// for.
//
// With it off, LowerHeapAllocations is handed a nil table and every call falls
// into the same assume-the-worst arm it has always fallen into; the pass cannot
// tell it is running under a build that has summaries at all.
var EscapeSummaries = os.Getenv("GOC_ESCAPE_SUMMARIES") == "1"

// HeapAllocLowering counts what LowerHeapAllocations did with the candidates it
// saw, which is the number that says whether the pass got smarter or just got
// more work.
type HeapAllocLowering struct {
	Promoted int // candidates that became frame slots
	Lowered  int // candidates that became allocator calls
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
		} else {
			stats.Lowered++
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
	analysis := analyzeCandidateEscapes(function, byName, facts, seeds, false)
	rewriteHeapAllocations(function, analysis, decisions)
	return true
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
			if _, known := bases[id]; known {
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
				// The allocator, type descriptor, and size are not uses of the result.
			case instruction.Op.IsLoad():
				// Reading through the candidate keeps it local.
			case instruction.Op == ir.OCmp:
				// Comparing a pointer observes its value but cannot retain it.
			case instruction.Op.IsStore():
				// Frontend variables live in ordinary stack allocations. Saving a
				// candidate in a non-escaping local slot does not make the pointed-to
				// object escape; storing it anywhere externally reachable does.
				if aliases.locOf(instruction.Arg(1), 1).class != cLocal {
					mark(instruction.Arg(0), "store into non-local storage")
				}
			case facts != nil && instruction.Op == ir.OCall &&
				!isAtomicPointerStore(function, instruction) && !benignMemoryCall(function, instruction):
				// The summarised form of the default arm below: an argument is only
				// a publication when the callee's summary says the callee can retain
				// it. This is the entire behavioural difference the fact table makes,
				// and with facts nil the case cannot be reached.
				markSummarisedCall(function, byName, facts, instruction, mark)
			case isTrackedHeapDerivation(instruction, bases):
				// Copies, casts, and constant pointer offsets preserve locality.
			case isAtomicPointerStore(function, instruction):
				// The destination is observed only for the duration of the store.
				// This is the write-barrier form emitted for pointer fields before
				// escape lowering knows whether the enclosing object is stack-local.
				destination := instruction.Arg(1)
				if _, localCandidate := heapBase(destination, bases); localCandidate {
					mark(instruction.Arg(2), "write barrier into a candidate")
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
// was already shaped like.
func rewriteHeapAllocations(function *ir.Func, analysis *candidateEscapes, decisions *[]ir.AllocDecision) {
	escaped := analysis.escaped
	for _, block := range function.Blocks {
		lowered := make([]ir.Instr, 0, len(block.Instrs))
		for _, original := range block.Instrs {
			instruction := original
			if instruction.Op != ir.OHeapAlloc || instruction.To.Kind != ir.RefTemp {
				lowered = append(lowered, instruction)
				continue
			}
			if escaped[instruction.To.ID] {
				recordAllocDecision(decisions, function, original, ir.AllocOnHeap)
				instruction.Op = ir.OCall
				instruction.Args = instruction.Args[:2]
				instruction.Aux = 0
				lowered = append(lowered, instruction)
				continue
			}
			recordAllocDecision(decisions, function, original, ir.AllocInFrame)

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

// recordAllocDecision notes where one heap-allocation candidate landed.
// candidate must still be the unrewritten OHeapAlloc, whose first two arguments
// are the allocator and the type descriptor.
func recordAllocDecision(decisions *[]ir.AllocDecision, function *ir.Func, candidate ir.Instr, placement ir.AllocPlacement) {
	if decisions == nil {
		return
	}
	*decisions = append(*decisions, ir.AllocDecision{
		Func:      function.Name,
		Pos:       candidate.Pos,
		Allocator: constSymbolName(function, candidate.Args[0]),
		Type:      constSymbolName(function, candidate.Args[1]),
		Placement: placement,
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
